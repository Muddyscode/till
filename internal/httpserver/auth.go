package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type creds struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type staffCreateReq struct {
	Email     string   `json:"email"`
	Password  string   `json:"password"`
	Role      string   `json:"role"`
	OutletIDs []string `json:"outlet_ids"`
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"staff": asStaff(s.staff(r))})
}

func (s *Server) requireRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			st := s.staff(r)
			for _, role := range roles {
				if st.Role == role {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeError(w, http.StatusForbidden, ledger.CodeForbidden, "role cannot do that")
		})
	}
}

func (s *Server) createStaff(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in staffCreateReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	role := strings.ToLower(strings.TrimSpace(in.Role))
	if email == "" || !strings.Contains(email, "@") {
		writeError(w, http.StatusBadRequest, "invalid_email", "email is required")
		return
	}
	if len(in.Password) > maxPasswordLn {
		writeError(w, http.StatusBadRequest, "weak_password", "password must be at most 128 characters")
		return
	}
	if len(in.Password) < 12 {
		writeError(w, http.StatusBadRequest, "weak_password", "password must be at least 12 characters")
		return
	}
	if !auth.ValidRole(role) {
		writeError(w, http.StatusBadRequest, "invalid_role", "role must be admin, manager, or cashier")
		return
	}
	if role != auth.RoleAdmin && len(in.OutletIDs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_role", "manager and cashier need an outlet")
		return
	}
	actor := s.staff(r)
	var outlets []uuid.UUID
	for _, rawID := range in.OutletIDs {
		id, err := uuid.Parse(rawID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_order", "outlet_id is invalid")
			return
		}
		if _, err := s.store.GetOutlet(r.Context(), s.store.Pool, actor.MerchantID, id); err != nil {
			writeLedgerError(w, err)
			return
		}
		outlets = append(outlets, id)
	}
	hash, err := s.argon.Hash(r.Context(), in.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	var created store.Staff
	err = s.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		created, err = s.store.CreateStaff(r.Context(), tx, store.Staff{
			ID: id, MerchantID: actor.MerchantID, Email: email, PasswordHash: hash, Role: role,
		})
		if err != nil {
			return err
		}
		for _, oid := range outlets {
			if err := s.store.AssignStaffOutlet(r.Context(), tx, created.ID, oid); err != nil {
				return err
			}
		}
		meta, _ := json.Marshal(map[string]any{"email": email, "role": role})
		mid, sid := actor.MerchantID, actor.ID
		return s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
			MerchantID: &mid, StaffID: &sid, Action: "staff.created", EntityType: "staff",
			EntityID: created.ID, Metadata: meta,
		})
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if asPg(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, "email_taken", "email already registered")
			return
		}
		writeLedgerError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, asStaff(created))
}

func (s *Server) listStaff(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListLoginStaff(r.Context(), s.store.Pool, s.staff(r).MerchantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]staffJSON, 0, len(list))
	for _, st := range list {
		out = append(out, asStaff(st))
	}
	writeJSON(w, http.StatusOK, map[string]any{"staff": out})
}

func (s *Server) disableStaff(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	actor := s.staff(r)
	target, err := s.store.GetStaffByID(r.Context(), s.store.Pool, id)
	if err != nil || target.MerchantID != actor.MerchantID {
		writeError(w, http.StatusNotFound, ledger.CodeNotFound, "staff not found")
		return
	}
	if target.Role == auth.RoleAdmin && target.DisabledAt == nil {
		n, err := s.store.CountActiveAdmins(r.Context(), s.store.Pool, actor.MerchantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if n <= 1 {
			writeError(w, http.StatusConflict, "last_admin", "the last admin cannot be disabled")
			return
		}
	}
	updated, err := s.store.DisableStaff(r.Context(), s.store.Pool, actor.MerchantID, id, time.Now())
	if err != nil {
		writeLedgerError(w, err)
		return
	}
	_ = s.withTx(r.Context(), func(tx pgx.Tx) error {
		mid, sid := actor.MerchantID, actor.ID
		return s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
			MerchantID: &mid, StaffID: &sid, Action: "staff.disabled", EntityType: "staff", EntityID: id,
		})
	})
	writeJSON(w, http.StatusOK, asStaff(updated))
}

func asPg(err error, target **pgconn.PgError) bool {
	var e *pgconn.PgError
	if errors.As(err, &e) {
		*target = e
		return true
	}
	return false
}
