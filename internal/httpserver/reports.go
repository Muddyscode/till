package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/google/uuid"
)

func lagosDay(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		loc, err := time.LoadLocation("Africa/Lagos")
		if err != nil {
			return "", err
		}
		return time.Now().In(loc).Format("2006-01-02"), nil
	}
	if _, err := time.Parse("2006-01-02", raw); err != nil {
		return "", err
	}
	return raw, nil
}

func (s *Server) outletQuery(w http.ResponseWriter, r *http.Request, required bool) (uuid.UUID, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("outlet_id"))
	if raw == "" {
		if required || s.staff(r).Role != auth.RoleAdmin {
			writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is required")
			return uuid.Nil, false
		}
		return uuid.Nil, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is invalid")
		return uuid.Nil, false
	}
	if _, ok := s.authorizeOutlet(w, r, id); !ok {
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) dailySales(w http.ResponseWriter, r *http.Request) {
	day, err := lagosDay(r.URL.Query().Get("date"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_order", "date must be YYYY-MM-DD")
		return
	}
	outletID, ok := s.outletQuery(w, r, true)
	if !ok {
		return
	}
	ds, err := s.store.DailySales(r.Context(), s.store.Pool, outletID, day)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	methods := make([]map[string]any, 0, len(ds.ByMethod))
	for _, m := range ds.ByMethod {
		methods = append(methods, map[string]any{
			"method": m.Method, "order_count": m.OrderCount,
			"total_minor": m.TotalMinor, "vat_minor": m.VATMinor, "net_minor": m.NetMinor,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"date": day, "outlet_id": outletID,
		"order_count": ds.OrderCount, "total_minor": ds.TotalMinor,
		"vat_minor": ds.VATMinor, "net_minor": ds.NetMinor,
		"by_method": methods, "books_debit_minor": ds.BooksDebit,
		"matches_books": ds.TotalMinor == ds.BooksDebit,
	})
}

func (s *Server) stockOnHand(w http.ResponseWriter, r *http.Request) {
	s.stockReport(w, r, false)
}

func (s *Server) lowStock(w http.ResponseWriter, r *http.Request) {
	s.stockReport(w, r, true)
}

func (s *Server) stockReport(w http.ResponseWriter, r *http.Request, lowOnly bool) {
	outletID, ok := s.outletQuery(w, r, true)
	if !ok {
		return
	}
	rows, err := s.store.StockOnHand(r.Context(), s.store.Pool, outletID, lowOnly)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := map[string]any{
			"sku": row.SKU, "name": row.Name, "on_hand": row.OnHand, "reserved": row.Reserved,
			"available": row.Available, "reorder_level": row.ReorderLevel, "price_minor": row.PriceMinor,
			"avg_cost_minor": row.AvgCostMinor, "value_minor": int64(row.OnHand) * row.AvgCostMinor,
		}
		if lowOnly {
			item["low_stock"] = true
		}
		out = append(out, item)
	}
	key := "stock"
	if lowOnly {
		key = "low_stock"
	}
	writeJSON(w, http.StatusOK, map[string]any{"outlet_id": outletID, key: out})
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	st := s.staff(r)
	var outletID *uuid.UUID
	if raw := strings.TrimSpace(r.URL.Query().Get("outlet_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is invalid")
			return
		}
		if _, ok := s.authorizeOutlet(w, r, id); !ok {
			return
		}
		outletID = &id
	} else if st.Role != auth.RoleAdmin {
		writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is required")
		return
	}
	list, err := s.store.ListAudit(r.Context(), s.store.Pool, st.MerchantID, outletID, limitParam(r, 50, 200))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, ev := range list {
		out = append(out, map[string]any{
			"id": ev.ID, "staff_id": ev.StaffID, "action": ev.Action,
			"entity_type": ev.EntityType, "entity_id": ev.EntityID,
			"outlet_id": ev.OutletID, "metadata": jsonRaw(ev.Metadata), "created_at": ev.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func jsonRaw(b []byte) any {
	if len(b) == 0 {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return map[string]any{}
	}
	return v
}
