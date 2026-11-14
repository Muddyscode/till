package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/money"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Shifts (till sessions) and cash-up. Expected cash is derived from the
// ledger, not from sale code: see store.ShiftCashFlow. The merchant setting
// "require an open shift to sell" is future work; enforcing it needs a hook in
// the sale path, which Phase 3B deliberately leaves untouched.

func shiftBody(sh store.Shift) map[string]any {
	return map[string]any{
		"id": sh.ID, "outlet_id": sh.OutletID, "staff_id": sh.StaffID, "status": sh.Status,
		"opening_float_minor": sh.OpeningFloatMinor, "opened_at": sh.OpenedAt,
		"closed_at": sh.ClosedAt, "closed_by": sh.ClosedBy,
		"expected_cash_minor": sh.ExpectedMinor, "counted_cash_minor": sh.CountedMinor,
		"variance_minor": sh.VarianceMinor, "variance_journal_id": sh.VarianceJournalID, "note": sh.Note,
	}
}

type openShiftReq struct {
	OutletID     string          `json:"outlet_id"`
	OpeningFloat json.RawMessage `json:"opening_float_minor"`
}

func (s *Server) openShift(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in openShiftReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	outletID, err := uuid.Parse(in.OutletID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is invalid")
		return
	}
	float, err := money.ParseOptionalKobo(in.OpeningFloat)
	if err != nil {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, "opening_float_minor must be a JSON integer in kobo")
		return
	}
	outlet, ok := s.authorizeOutlet(w, r, outletID)
	if !ok {
		return
	}
	actor := s.staff(r)
	var sh store.Shift
	err = s.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if sh, err = s.store.OpenShift(r.Context(), tx, actor.MerchantID, outlet.ID, actor.ID, float); err != nil {
			return err
		}
		meta, _ := json.Marshal(map[string]any{"shift_id": sh.ID, "opening_float_minor": float})
		mid, sid, oid := actor.MerchantID, actor.ID, outlet.ID
		return s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
			MerchantID: &mid, StaffID: &sid, Action: "shift.opened", EntityType: "shift", EntityID: sh.ID, OutletID: &oid, Metadata: meta,
		})
	})
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, shiftBody(sh))
}

// canSeeShift: admin any, manager any shift at an assigned outlet, cashier only their own.
func (s *Server) canSeeShift(w http.ResponseWriter, r *http.Request, sh store.Shift) (store.Outlet, bool) {
	st := s.staff(r)
	if st.Role == auth.RoleCashier && sh.StaffID != st.ID {
		writeError(w, http.StatusNotFound, ledger.CodeNotFound, "shift not found")
		return store.Outlet{}, false
	}
	o, ok := s.authorizeOutlet(w, r, sh.OutletID)
	return o, ok
}

func (s *Server) listShifts(w http.ResponseWriter, r *http.Request) {
	st := s.staff(r)
	q := r.URL.Query()
	outletID, ok := s.outletScope(w, r)
	if !ok {
		return
	}
	status := strings.TrimSpace(q.Get("status"))
	if status != "" && status != "open" && status != "closed" {
		badQuery(w, "status must be open or closed")
		return
	}
	var staffID *uuid.UUID
	if st.Role == auth.RoleCashier {
		staffID = &st.ID
	}
	list, err := s.store.ListShifts(r.Context(), s.store.Pool, st.MerchantID, outletID, staffID, status, limitParam(r, 50, 200))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, sh := range list {
		out = append(out, shiftBody(sh))
	}
	writeJSON(w, http.StatusOK, map[string]any{"shifts": out})
}

func (s *Server) getShift(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	sh, err := s.store.GetShift(r.Context(), s.store.Pool, s.staff(r).MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	if _, ok := s.canSeeShift(w, r, sh); !ok {
		return
	}
	writeJSON(w, http.StatusOK, shiftBody(sh))
}

func (s *Server) shiftZReport(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	sh, err := s.store.GetShift(r.Context(), s.store.Pool, s.staff(r).MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	outlet, ok := s.canSeeShift(w, r, sh)
	if !ok {
		return
	}
	z, err := s.store.LiveZReport(r.Context(), s.store.Pool, sh, outlet.CashAccountID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, z)
}

type closeShiftReq struct {
	CountedCash json.RawMessage `json:"counted_cash_minor"`
	Note        string          `json:"note"`
}

func (s *Server) closeShift(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in closeShiftReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	if c := strings.TrimSpace(string(in.CountedCash)); c == "" || c == "null" {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, "counted_cash_minor is required")
		return
	}
	counted, err := money.ParseOptionalKobo(in.CountedCash)
	if err != nil {
		writeError(w, http.StatusBadRequest, ledger.CodeInvalidAmount, "counted_cash_minor must be a JSON integer in kobo")
		return
	}
	note := strings.TrimSpace(in.Note)
	if len(note) > 500 {
		writeError(w, http.StatusBadRequest, "invalid_order", "note is too long")
		return
	}
	actor := s.staff(r)
	sh, err := s.store.GetShift(r.Context(), s.store.Pool, actor.MerchantID, id)
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	// A cashier closes only their own shift; managers and admins may close anyone's.
	if actor.Role == auth.RoleCashier && sh.StaffID != actor.ID {
		writeError(w, http.StatusForbidden, ledger.CodeForbidden, "only the shift owner or a manager can close this shift")
		return
	}
	outlet, ok := s.authorizeOutlet(w, r, sh.OutletID)
	if !ok {
		return
	}
	var closed store.Shift
	var z store.ZReport
	err = s.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		closed, z, err = s.store.CloseShift(r.Context(), tx, actor.MerchantID, id, outlet, actor.ID, counted, note)
		return err
	})
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	body := shiftBody(closed)
	body["z_report"] = z
	writeJSON(w, http.StatusOK, body)
}
