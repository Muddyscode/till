package checkout

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Muddyscode/till/internal/store"
	"github.com/jackc/pgx/v5"
)

func ReleaseExpired(ctx context.Context, tx pgx.Tx, st *store.Store, now time.Time) (int, error) {
	orders, err := st.LockExpiredOrders(ctx, tx, now, 50)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range orders {
		res, err := st.ReservationsByOrder(ctx, tx, o.ID)
		if err != nil {
			return n, err
		}
		for _, r := range res {
			if r.Status != "active" {
				continue
			}
			if err := st.ReleaseReserved(ctx, tx, o.OutletID, r.ProductID, r.Qty); err != nil {
				return n, err
			}
		}
		if err := st.SetReservationsStatus(ctx, tx, o.ID, "active", "released"); err != nil {
			return n, err
		}
		if err := st.MarkExpired(ctx, tx, o.ID); err != nil {
			return n, err
		}
		raw, err := json.Marshal(map[string]any{"reference": o.Reference})
		if err != nil {
			return n, err
		}
		mid := o.MerchantID
		outlet := o.OutletID
		if err := st.InsertAudit(ctx, tx, store.AuditEvent{
			MerchantID: &mid, Action: "reservation.released", EntityType: "order",
			EntityID: o.ID, OutletID: &outlet, Metadata: raw,
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func RunSweeper(ctx context.Context, st *store.Store, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = st.WithTx(ctx, func(tx pgx.Tx) error {
				_, err := ReleaseExpired(ctx, tx, st, time.Now())
				return err
			})
		}
	}
}
