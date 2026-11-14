package httpserver

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/checkout"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Inter-outlet transfers and stock counts (Phase 3B).
//
// Transfers post no journal: the inventory account (1200) is merchant-wide and
// average cost is per product, so moving units between outlets changes neither.
// /v1/invariants counts units in transit at their average cost.

type transferLineReq struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

type stockTransferReq struct {
	FromOutletID string            `json:"from_outlet_id"`
	ToOutletID   string            `json:"to_outlet_id"`
	Lines        []transferLineReq `json:"lines"`
	Note         string            `json:"note"`
}

func transferBody(t store.Transfer) map[string]any {
	lines := make([]map[string]any, 0, len(t.Lines))
	for _, l := range t.Lines {
		lines = append(lines, map[string]any{"sku": l.SKU, "quantity": l.Quantity})
	}
	return map[string]any{
		"id": t.ID, "from_outlet_id": t.FromOutletID, "to_outlet_id": t.ToOutletID, "status": t.Status,
		"note": t.Note, "created_by": t.CreatedBy, "created_at": t.CreatedAt,
		"received_by": t.ReceivedBy, "received_at": t.ReceivedAt,
		"cancelled_by": t.CancelledBy, "cancelled_at": t.CancelledAt, "lines": lines,
	}
}

func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in stockTransferReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	from, err1 := uuid.Parse(in.FromOutletID)
	to, err2 := uuid.Parse(in.ToOutletID)
	if err1 != nil || err2 != nil || from == to {
		writeError(w, http.StatusBadRequest, "invalid_order", "from_outlet_id and to_outlet_id must be two different outlets")
		return
	}
	if len(in.Lines) == 0 || len(in.Lines) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_order", "lines must have between 1 and 200 entries")
		return
	}
	if len(in.Note) > 500 {
		writeError(w, http.StatusBadRequest, "invalid_order", "note is too long")
		return
	}
	// Manager of the source outlet, or admin.
	if _, ok := s.authorizeOutlet(w, r, from); !ok {
		return
	}
	actor := s.staff(r)
	if _, err := s.store.GetOutlet(r.Context(), s.store.Pool, actor.MerchantID, to); err != nil {
		writeLedgerError(w, err)
		return
	}
	lines := make([]store.TransferLine, 0, len(in.Lines))
	seen := map[uuid.UUID]bool{}
	for _, l := range in.Lines {
		if l.Quantity <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_order", "quantity must be > 0")
			return
		}
		p, err := s.store.GetProductBySKU(r.Context(), s.store.Pool, actor.MerchantID, normalizeSKU(l.SKU))
		if err != nil {
			writeLedgerError(w, err)
			return
		}
		if seen[p.ID] {
			writeError(w, http.StatusBadRequest, "invalid_order", "each sku may appear once: "+p.SKU)
			return
		}
		seen[p.ID] = true
		lines = append(lines, store.TransferLine{ProductID: p.ID, SKU: p.SKU, Quantity: l.Quantity})
	}
	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.finishIdempotent(w, r, actor.MerchantID, key, fingerprint(r, raw), func(tx pgx.Tx) (int, []byte, *uuid.UUID, error) {
		t, err := s.store.CreateTransfer(r.Context(), tx, store.Transfer{
			ID: id, MerchantID: actor.MerchantID, FromOutletID: from, ToOutletID: to,
			Note: strings.TrimSpace(in.Note), CreatedBy: actor.ID, Lines: lines,
		})
		if err != nil {
			return 0, nil, nil, err
		}
		body, err := json.Marshal(transferBody(t))
		return http.StatusCreated, body, nil, err
	})
}

func (s *Server) listStockTransfers(w http.ResponseWriter, r *http.Request) {
	st := s.staff(r)
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && status != "in_transit" && status != "received" && status != "cancelled" {
		badQuery(w, "status must be in_transit, received or cancelled")
		return
	}
	var scope *uuid.UUID
	if st.Role != auth.RoleAdmin {
		scope = &st.ID
	}
	list, err := s.store.ListTransfers(r.Context(), s.store.Pool, st.MerchantID, scope, status, limitParam(r, 50, 200))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, transferBody(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"transfers": out})
}

// transferVisible lets a manager see only transfers touching one of their outlets.
func (s *Server) transferVisible(w http.ResponseWriter, r *http.Request, t store.Transfer) bool {
	st := s.staff(r)
	if st.Role == auth.RoleAdmin {
		return true
	}
	for _, o := range []uuid.UUID{t.FromOutletID, t.ToOutletID} {
		ok, err := s.store.StaffHasOutlet(r.Context(), s.store.Pool, st.ID, o, st.MerchantID)
		if err != nil {
			s.internalError(w, r, err)
			return false
		}
		if ok {
			return true
		}
	}
	writeError(w, http.StatusNotFound, ledger.CodeNotFound, "transfer not found")
	return false
}

func (s *Server) getTransfer(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	t, err := s.store.GetTransfer(r.Context(), s.store.Pool, s.staff(r).MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	if !s.transferVisible(w, r, t) {
		return
	}
	writeJSON(w, http.StatusOK, transferBody(t))
}

func (s *Server) receiveTransfer(w http.ResponseWriter, r *http.Request) {
	s.settleTransfer(w, r, true)
}
func (s *Server) cancelTransfer(w http.ResponseWriter, r *http.Request) {
	s.settleTransfer(w, r, false)
}

// settleTransfer: receive needs a manager of the destination (or admin), cancel a manager of the source.
func (s *Server) settleTransfer(w http.ResponseWriter, r *http.Request, receive bool) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	actor := s.staff(r)
	t, err := s.store.GetTransfer(r.Context(), s.store.Pool, actor.MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	outlet := t.FromOutletID
	if receive {
		outlet = t.ToOutletID
	}
	if _, ok := s.authorizeOutlet(w, r, outlet); !ok {
		return
	}
	var done store.Transfer
	err = s.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		done, err = s.store.SettleTransfer(r.Context(), tx, actor.MerchantID, id, actor.ID, receive)
		return err
	})
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, transferBody(done))
}

// ---- stock counts ----

type countLineReq struct {
	SKU     string `json:"sku"`
	Counted *int   `json:"counted"`
}

type countReq struct {
	OutletID string         `json:"outlet_id"`
	Note     string         `json:"note"`
	Lines    []countLineReq `json:"lines"`
}

func countBody(c store.StockCount) map[string]any {
	lines := make([]map[string]any, 0, len(c.Lines))
	var net int64
	for _, l := range c.Lines {
		net += l.VarianceValueMinor
		lines = append(lines, map[string]any{
			"sku": l.SKU, "expected_on_hand": l.ExpectedOnHand, "counted": l.Counted, "variance": l.Variance,
			"unit_cost_minor": l.UnitCostMinor, "variance_value_minor": l.VarianceValueMinor,
		})
	}
	return map[string]any{
		"id": c.ID, "outlet_id": c.OutletID, "note": c.Note, "created_by": c.CreatedBy, "created_at": c.CreatedAt,
		"lines": lines, "net_variance_value_minor": net,
	}
}

func (s *Server) createStockCount(w http.ResponseWriter, r *http.Request) {
	key, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in countReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	outletID, err := uuid.Parse(in.OutletID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is invalid")
		return
	}
	if len(in.Lines) == 0 || len(in.Lines) > 500 {
		writeError(w, http.StatusBadRequest, "invalid_order", "lines must have between 1 and 500 entries")
		return
	}
	if len(in.Note) > 500 {
		writeError(w, http.StatusBadRequest, "invalid_order", "note is too long")
		return
	}
	outlet, ok := s.authorizeOutlet(w, r, outletID)
	if !ok {
		return
	}
	actor := s.staff(r)
	type item struct {
		product store.Product
		counted int
	}
	items := make([]item, 0, len(in.Lines))
	seen := map[uuid.UUID]bool{}
	for _, l := range in.Lines {
		if l.Counted == nil || *l.Counted < 0 {
			writeError(w, http.StatusBadRequest, "invalid_order", "counted must be an integer >= 0")
			return
		}
		p, err := s.store.GetProductBySKU(r.Context(), s.store.Pool, actor.MerchantID, normalizeSKU(l.SKU))
		if err != nil {
			writeLedgerError(w, err)
			return
		}
		if seen[p.ID] {
			writeError(w, http.StatusBadRequest, "invalid_order", "each sku may appear once: "+p.SKU)
			return
		}
		seen[p.ID] = true
		items = append(items, item{p, *l.Counted})
	}
	slices.SortFunc(items, func(a, b item) int { return strings.Compare(a.product.ID.String(), b.product.ID.String()) })
	countID, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.finishIdempotent(w, r, actor.MerchantID, key, fingerprint(r, raw), func(tx pgx.Tx) (int, []byte, *uuid.UUID, error) {
		merchant, err := s.store.EnsureBooks(r.Context(), tx, actor.MerchantID)
		if err != nil {
			return 0, nil, nil, err
		}
		ids := make([]uuid.UUID, len(items))
		for i, it := range items {
			ids[i] = it.product.ID
		}
		locked, err := s.store.LockProducts(r.Context(), tx, actor.MerchantID, ids)
		if err != nil {
			return 0, nil, nil, err
		}
		for _, id := range ids {
			if err := s.store.EnsureStockRow(r.Context(), tx, outlet.ID, id); err != nil {
				return 0, nil, nil, err
			}
		}
		if err := s.store.LockStock(r.Context(), tx, outlet.ID, ids); err != nil {
			return 0, nil, nil, err
		}
		count := store.StockCount{ID: countID, MerchantID: actor.MerchantID, OutletID: outlet.ID, Note: strings.TrimSpace(in.Note), CreatedBy: actor.ID}
		skus := make([]string, 0, len(items))
		for _, it := range items {
			p := locked[it.product.ID]
			onHand, reserved, err := s.store.StockLevel(r.Context(), tx, outlet.ID, p.ID)
			if err != nil {
				return 0, nil, nil, err
			}
			if it.counted < reserved {
				return 0, nil, nil, ledger.Err(ledger.CodeInsufficientStock, "counted quantity for "+p.SKU+" is below the units reserved for pending orders")
			}
			variance := it.counted - onHand
			if variance != 0 {
				if _, err := checkout.Adjust(r.Context(), tx, s.store, checkout.AdjustInput{
					Merchant: merchant, Outlet: outlet, StaffID: actor.ID, ProductID: p.ID, Delta: variance, Reason: "stock_count",
				}); err != nil {
					return 0, nil, nil, err
				}
			}
			skus = append(skus, p.SKU)
			count.Lines = append(count.Lines, store.CountLine{
				ProductID: p.ID, SKU: p.SKU, ExpectedOnHand: onHand, Counted: it.counted, Variance: variance,
				UnitCostMinor: p.AvgCostMinor, VarianceValueMinor: int64(variance) * p.AvgCostMinor,
			})
		}
		count, err = s.store.InsertCount(r.Context(), tx, count)
		if err != nil {
			return 0, nil, nil, err
		}
		meta, _ := json.Marshal(map[string]any{"count_id": count.ID, "skus": skus, "lines": len(count.Lines)})
		mid, sid, oid := actor.MerchantID, actor.ID, outlet.ID
		if err := s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
			MerchantID: &mid, StaffID: &sid, Action: "stock.counted", EntityType: "stock_count", EntityID: count.ID, OutletID: &oid, Metadata: meta,
		}); err != nil {
			return 0, nil, nil, err
		}
		body, err := json.Marshal(countBody(count))
		return http.StatusCreated, body, nil, err
	})
}

func (s *Server) getStockCount(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	c, err := s.store.GetCount(r.Context(), s.store.Pool, s.staff(r).MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	if _, ok := s.authorizeOutlet(w, r, c.OutletID); !ok {
		return
	}
	writeJSON(w, http.StatusOK, countBody(c))
}
