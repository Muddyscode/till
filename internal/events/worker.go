package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const cursorName = "webhooks"

// Sign returns the Till-Signature header value: t=<unix>,v1=<hex HMAC-SHA256 of "t.body">.
func Sign(secret string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte{'.'})
	m.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(m.Sum(nil)))
}

// Worker fans new audit events out to webhook endpoints and delivers them.
// Several instances may run at once: the fan-out cursor is row-locked and
// deliveries are claimed with FOR UPDATE SKIP LOCKED.
type Worker struct {
	Store  *store.Store
	Log    *slog.Logger
	Client *http.Client
	// Interval between passes. Default 2s.
	Interval time.Duration
	// Lag hides events younger than this from fan-out, so transactions that
	// started earlier but commit later are not skipped by the cursor.
	Lag time.Duration
	// BackoffBase is the first retry delay; it doubles per attempt up to 1h. Default 10s.
	BackoffBase time.Duration
	// MaxAttempts per delivery. Default 8.
	MaxAttempts int
	// Lease is how long a claimed delivery stays invisible to other workers. Default 60s.
	Lease time.Duration
	Batch int
}

func (w *Worker) defaults() {
	if w.Interval <= 0 {
		w.Interval = 2 * time.Second
	}
	if w.BackoffBase <= 0 {
		w.BackoffBase = 10 * time.Second
	}
	if w.MaxAttempts <= 0 {
		w.MaxAttempts = 8
	}
	if w.Lease <= 0 {
		w.Lease = 60 * time.Second
	}
	if w.Batch <= 0 {
		w.Batch = 200
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	if w.Client == nil {
		w.Client = NewClient(true)
	}
}

// Run loops until ctx is cancelled and returns after the current pass ends.
func (w *Worker) Run(ctx context.Context) {
	w.defaults()
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			w.Log.Error("webhook worker", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick runs one fan-out and delivery pass.
func (w *Worker) Tick(ctx context.Context) error {
	w.defaults()
	for {
		n, err := w.fanOut(ctx)
		if err != nil {
			return err
		}
		if n < w.Batch {
			break
		}
	}
	return w.deliverDue(ctx)
}

func matches(e store.WebhookEndpoint, typ string) bool {
	if len(e.EventTypes) == 0 {
		return true
	}
	for _, t := range e.EventTypes {
		if t == typ {
			return true
		}
	}
	return false
}

func (w *Worker) fanOut(ctx context.Context) (int, error) {
	var count int
	err := w.Store.WithTx(ctx, func(tx pgx.Tx) error {
		cur, err := w.Store.LockCursor(ctx, tx, cursorName)
		if err != nil {
			return err
		}
		var before *time.Time
		if w.Lag > 0 {
			b := time.Now().Add(-w.Lag)
			before = &b
		}
		rows, err := w.Store.EventRows(ctx, tx, nil, cur, nil, before, w.Batch)
		if err != nil {
			return err
		}
		count = len(rows)
		if count == 0 {
			return nil
		}
		eps := map[uuid.UUID][]store.WebhookEndpoint{}
		endpoints := func(m uuid.UUID) ([]store.WebhookEndpoint, error) {
			if v, ok := eps[m]; ok {
				return v, nil
			}
			v, err := w.Store.ListWebhookEndpoints(ctx, tx, m)
			eps[m] = v
			return v, err
		}
		enqueue := func(list []store.WebhookEndpoint, ev Event) error {
			var payload []byte
			for _, ep := range list {
				if !matches(ep, ev.Type) {
					continue
				}
				if payload == nil {
					if payload, err = json.Marshal(ev); err != nil {
						return err
					}
				}
				id, err := uuid.NewV7()
				if err != nil {
					return err
				}
				if err := w.Store.InsertDelivery(ctx, tx, store.Delivery{
					ID: id, EndpointID: ep.ID, EventID: ev.ID, EventType: ev.Type, Payload: payload,
				}); err != nil {
					return err
				}
			}
			return nil
		}
		for _, row := range rows {
			list, err := endpoints(row.MerchantID)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				continue
			}
			if ev, ok := FromRow(row); ok {
				if err := enqueue(list, ev); err != nil {
					return err
				}
			}
			if err := w.lowStock(ctx, tx, row, list, enqueue); err != nil {
				return err
			}
		}
		return w.Store.SetCursor(ctx, tx, cursorName, rows[len(rows)-1].Seq)
	})
	return count, err
}

// skusTouched names the products whose stock a row may have lowered.
func (w *Worker) skusTouched(ctx context.Context, tx pgx.Tx, row store.EventRow) ([]string, error) {
	var meta struct {
		SKU  string   `json:"sku"`
		SKUs []string `json:"skus"`
	}
	_ = json.Unmarshal(row.Metadata, &meta)
	switch row.Action {
	case "order.paid":
		return w.Store.OrderSKUs(ctx, tx, row.EntityID)
	case "stock.adjusted":
		if meta.SKU != "" {
			return []string{meta.SKU}, nil
		}
	case "stock.counted", "transfer.sent":
		return meta.SKUs, nil
	}
	return nil, nil
}

func (w *Worker) lowStock(ctx context.Context, tx pgx.Tx, row store.EventRow, list []store.WebhookEndpoint, enqueue func([]store.WebhookEndpoint, Event) error) error {
	if row.OutletID == nil {
		return nil
	}
	skus, err := w.skusTouched(ctx, tx, row)
	if err != nil || len(skus) == 0 {
		return err
	}
	low, err := w.Store.LowStockAmong(ctx, tx, row.MerchantID, *row.OutletID, skus)
	if err != nil {
		return err
	}
	for _, l := range low {
		ev := Event{
			ID:         uuid.NewSHA1(uuid.NameSpaceOID, []byte(row.ID.String()+"|"+l.SKU)),
			Type:       StockLow,
			OutletID:   row.OutletID,
			OccurredAt: row.CreatedAt.UTC(),
			Data: map[string]any{
				"sku": l.SKU, "available": l.Available, "on_hand": l.OnHand,
				"reorder_level": l.ReorderLevel, "caused_by_event_id": row.ID,
			},
		}
		if err := enqueue(list, ev); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) backoff(attempt int) time.Duration {
	d := w.BackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= time.Hour {
			return time.Hour
		}
	}
	return d
}

func (w *Worker) deliverDue(ctx context.Context) error {
	for {
		due, err := w.Store.ClaimDueDeliveries(ctx, 20, w.Lease)
		if err != nil {
			return err
		}
		if len(due) == 0 {
			return nil
		}
		for _, d := range due {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			status, derr := w.send(ctx, d)
			ok := derr == nil
			final := !ok && d.Attempts >= w.MaxAttempts
			msg := ""
			if derr != nil {
				msg = derr.Error()
			}
			if err := w.Store.FinishDelivery(ctx, d.ID, ok, final, time.Now().Add(w.backoff(d.Attempts)), status, msg); err != nil {
				return err
			}
		}
	}
}

func (w *Worker) send(ctx context.Context, d store.DueDelivery) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(d.Payload))
	if err != nil {
		return 0, err
	}
	ts := time.Now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Till-Webhooks/1")
	req.Header.Set("Till-Event-Id", d.EventID.String())
	req.Header.Set("Till-Signature", Sign(d.Secret, ts, d.Payload))
	res, err := w.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return res.StatusCode, fmt.Errorf("endpoint returned %d", res.StatusCode)
	}
	return res.StatusCode, nil
}
