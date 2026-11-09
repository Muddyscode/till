package checkout

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	sweepBatch          = 50
	sweepMaxBatches     = 20
	idempotencyTTL      = 30 * 24 * time.Hour
	idempotencyPurgeGap = time.Hour
)

// releaseOne releases one locked pending order: reservations, stock holds,
// status and audit. Stock rows are locked in product id order first.
func releaseOne(ctx context.Context, tx pgx.Tx, st *store.Store, o store.Order) error {
	res, err := st.ReservationsByOrder(ctx, tx, o.ID)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(res))
	for _, r := range res {
		ids = append(ids, r.ProductID)
	}
	if len(ids) > 0 {
		if err := st.LockStock(ctx, tx, o.OutletID, ids); err != nil {
			return err
		}
	}
	for _, r := range res {
		if r.Status != "active" {
			continue
		}
		if err := st.ReleaseReserved(ctx, tx, o.OutletID, r.ProductID, r.Qty); err != nil {
			return err
		}
	}
	if err := st.SetReservationsStatus(ctx, tx, o.ID, "active", "released"); err != nil {
		return err
	}
	if err := returnDeposits(ctx, tx, st, o); err != nil {
		return err
	}
	if err := st.MarkExpired(ctx, tx, o.ID); err != nil {
		return err
	}
	raw, err := json.Marshal(map[string]any{"reference": o.Reference})
	if err != nil {
		return err
	}
	mid := o.MerchantID
	outlet := o.OutletID
	return st.InsertAudit(ctx, tx, store.AuditEvent{
		MerchantID: &mid, Action: "reservation.released", EntityType: "order",
		EntityID: o.ID, OutletID: &outlet, Metadata: raw,
	})
}

// ReleaseExpired releases up to 50 expired orders inside the caller's single
// transaction. The sweeper itself uses SweepExpired (one transaction per order).
func ReleaseExpired(ctx context.Context, tx pgx.Tx, st *store.Store, now time.Time) (int, error) {
	orders, err := st.LockExpiredOrders(ctx, tx, now, sweepBatch)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range orders {
		if err := releaseOne(ctx, tx, st, o); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// SweepExpired releases expired orders, each in its own transaction with
// deadlock retry. An order that keeps failing is logged and added to failed so
// it cannot block the rest of this sweep. It returns how many were released.
func SweepExpired(ctx context.Context, st *store.Store, log *slog.Logger, now time.Time, failed map[uuid.UUID]struct{}) int {
	if log == nil {
		log = slog.Default()
	}
	if failed == nil {
		failed = map[uuid.UUID]struct{}{}
	}
	released := 0
	for batch := 0; batch < sweepMaxBatches; batch++ {
		skip := make([]uuid.UUID, 0, len(failed))
		for id := range failed {
			skip = append(skip, id)
		}
		ids, err := st.ExpiredOrderIDs(ctx, st.Pool, now, sweepBatch, skip)
		if err != nil {
			log.Error("sweeper: list expired orders", "err", err)
			return released
		}
		if len(ids) == 0 {
			return released
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return released
			}
			var did bool
			err := store.RetryDeadlock(func() error {
				did = false
				return st.WithTx(ctx, func(tx pgx.Tx) error {
					o, ok, err := st.LockExpiredOrder(ctx, tx, id, now)
					if err != nil || !ok {
						return err
					}
					if err := releaseOne(ctx, tx, st, o); err != nil {
						return err
					}
					did = true
					return nil
				})
			})
			if err != nil {
				failed[id] = struct{}{}
				log.Error("sweeper: release expired order failed", "order_id", id.String(), "err", err)
				continue
			}
			if did {
				released++
			} else {
				// Locked by a webhook or already settled; look again next tick.
				failed[id] = struct{}{}
			}
		}
	}
	return released
}

// RunSweeper releases expired holds every tick and purges old idempotency
// keys hourly. beat, when non-nil, is called after every completed tick so
// readiness can report a stalled sweeper.
func RunSweeper(ctx context.Context, st *store.Store, every time.Duration, log *slog.Logger, beat func()) {
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var lastPurge time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n := SweepExpired(ctx, st, log, time.Now(), map[uuid.UUID]struct{}{})
			if n > 0 {
				log.Info("sweeper: released expired orders", "count", n)
			}
			if time.Since(lastPurge) >= idempotencyPurgeGap {
				purged, err := st.PurgeIdempotency(ctx, idempotencyTTL)
				if err != nil {
					log.Error("sweeper: purge idempotency keys", "err", err)
				} else {
					lastPurge = time.Now()
					if purged > 0 {
						log.Info("sweeper: purged idempotency keys", "count", purged)
					}
				}
			}
			if beat != nil {
				beat()
			}
		}
	}
}
