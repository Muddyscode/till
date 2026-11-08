package httpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/jackc/pgx/v5"
)

const (
	apiKeyPrefix  = "till_live_"
	maxPasswordLn = 128
	minPasswordLn = 12
)

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) tooLarge(w http.ResponseWriter) {
	writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
}

func isTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

func retrySeconds(d time.Duration) string {
	n := int((d + time.Second - 1) / time.Second)
	if n < 1 {
		n = 1
	}
	return strconv.Itoa(n)
}

func (s *Server) rateLimited(w http.ResponseWriter, scope, code string, wait time.Duration) {
	s.metrics.limited(scope)
	w.Header().Set("Retry-After", retrySeconds(wait))
	writeError(w, http.StatusTooManyRequests, code, "too many attempts, try again later")
}

func (s *Server) tokenTTL() time.Duration {
	if s.cfg.TokenTTL > 0 {
		return s.cfg.TokenTTL
	}
	return 8 * time.Hour
}

func isNotFound(err error) bool {
	le := ledger.AsError(err)
	return le != nil && le.Code == ledger.CodeNotFound
}

// login verifies credentials. Order of defences: per-IP bucket, per-email bucket,
// persistent lockout, then an argon2 verify that runs for unknown emails too.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if ok, wait := s.ipLimiter.Allow(s.clientIP(r)); !ok {
		s.rateLimited(w, "ip", "rate_limited", wait)
		return
	}
	var in creds
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		if isTooLarge(err) {
			s.tooLarge(w)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if len(email) > 254 || len(in.Password) > maxPasswordLn {
		writeError(w, http.StatusUnauthorized, ledger.CodeUnauthorized, "invalid credentials")
		return
	}
	if ok, wait := s.emailLimiter.Allow(email); !ok {
		s.rateLimited(w, "email", "rate_limited", wait)
		return
	}
	ctx := r.Context()
	st, err := s.store.GetStaffAuthByEmail(ctx, s.store.Pool, email)
	found := err == nil
	if err != nil && !isNotFound(err) {
		s.internalError(w, r, err)
		return
	}
	if found && st.LockedUntil != nil && st.LockedUntil.After(time.Now()) {
		s.rateLimited(w, "lockout", "account_locked", time.Until(*st.LockedUntil))
		return
	}
	hash := ""
	if found {
		hash = st.PasswordHash
	}
	okPw, err := s.argon.Verify(ctx, in.Password, hash)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "busy", "server busy, retry")
		return
	}
	if !found || !okPw || st.DisabledAt != nil || st.IsService {
		s.metrics.loginFailure()
		if found {
			if _, err := s.store.RecordLoginFailure(ctx, s.store.Pool, st.ID); err != nil {
				s.log.Error("record login failure", "err", err, "request_id", RequestIDFrom(r))
			}
		}
		writeError(w, http.StatusUnauthorized, ledger.CodeUnauthorized, "invalid credentials")
		return
	}
	if st.FailedCount > 0 || st.LockedUntil != nil {
		_ = s.store.ClearLoginFailures(ctx, s.store.Pool, st.ID)
	}
	token, exp, err := auth.IssueToken(s.cfg.JWTSecret, st.Email, st.Role, st.ID, st.MerchantID, st.TokenVersion, s.tokenTTL())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	info := infoFrom(ctx)
	info.MerchantID, info.StaffID = st.MerchantID.String(), st.ID.String()
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token, "expires_at": exp.UTC().Format(time.RFC3339), "staff": asStaff(st.Staff),
	})
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	return strings.TrimSpace(h[7:]), true
}

func unauthorized(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusUnauthorized, ledger.CodeUnauthorized, msg)
}

// authn accepts a staff JWT or a machine API key and puts the acting store.Staff in the context.
func (s *Server) authn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearer(r)
		if !ok || raw == "" {
			unauthorized(w, "missing token")
			return
		}
		var actor store.Staff
		info := infoFrom(r.Context())
		if strings.HasPrefix(raw, apiKeyPrefix) {
			st, keyID, ok := s.authnAPIKey(r.Context(), raw)
			if !ok {
				unauthorized(w, "invalid api key")
				return
			}
			actor = st
			info.APIKeyID = keyID
		} else {
			id, err := auth.ParseToken(s.cfg.JWTSecret, raw)
			if err != nil {
				unauthorized(w, "invalid token")
				return
			}
			sa, err := s.store.GetStaffAuthByID(r.Context(), s.store.Pool, id.StaffID)
			if err != nil || sa.DisabledAt != nil || sa.IsService || sa.MerchantID != id.MerchantID ||
				sa.Role != id.Role || sa.TokenVersion != id.TokenVersion {
				unauthorized(w, "invalid token")
				return
			}
			actor = sa.Staff
		}
		info.MerchantID, info.StaffID = actor.MerchantID.String(), actor.ID.String()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), staffKey, actor)))
	})
}

// authnAPIKey parses till_live_<prefix>_<secret>, checks the digest in constant time,
// and returns the service staff row that backs the key.
func (s *Server) authnAPIKey(ctx context.Context, raw string) (store.Staff, string, bool) {
	prefix, secret, ok := strings.Cut(strings.TrimPrefix(raw, apiKeyPrefix), "_")
	if !ok || len(prefix) != apiKeyPrefixLen || len(secret) != apiKeySecretLen {
		return store.Staff{}, "", false
	}
	k, err := s.store.GetAPIKeyByPrefix(ctx, s.store.Pool, prefix)
	if err != nil || k.RevokedAt != nil {
		return store.Staff{}, "", false
	}
	if subtle.ConstantTimeCompare(store.HashAPIKeySecret(secret), k.Hash) != 1 {
		return store.Staff{}, "", false
	}
	sa, err := s.store.GetStaffAuthByID(ctx, s.store.Pool, k.StaffID)
	if err != nil || sa.DisabledAt != nil || !sa.IsService {
		return store.Staff{}, "", false
	}
	s.store.TouchAPIKey(ctx, s.store.Pool, k.ID)
	return sa.Staff, k.ID.String(), true
}

// humanOnly rejects API-key callers on endpoints that manage a person's own session.
func (s *Server) humanOnly(w http.ResponseWriter, r *http.Request) bool {
	if infoFrom(r.Context()).APIKeyID != "" {
		writeError(w, http.StatusForbidden, ledger.CodeForbidden, "not available to API keys")
		return false
	}
	return true
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.humanOnly(w, r) {
		return
	}
	if _, err := s.store.BumpTokenVersion(r.Context(), s.store.Pool, s.staff(r).ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "logged_out"})
}

type passwordChangeReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func validNewPassword(w http.ResponseWriter, pw string) bool {
	if len(pw) < minPasswordLn {
		writeError(w, http.StatusBadRequest, "weak_password", "password must be at least 12 characters")
		return false
	}
	if len(pw) > maxPasswordLn {
		writeError(w, http.StatusBadRequest, "weak_password", "password must be at most 128 characters")
		return false
	}
	return true
}

func (s *Server) auditStaff(r *http.Request, tx pgx.Tx, action string, target store.Staff, actor store.Staff) error {
	meta, _ := json.Marshal(map[string]any{"email": target.Email})
	mid, sid := actor.MerchantID, actor.ID
	return s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
		MerchantID: &mid, StaffID: &sid, Action: action, EntityType: "staff", EntityID: target.ID, Metadata: meta,
	})
}

// changePassword lets a person change their own password. Every older token stops working;
// the response carries a fresh one.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	if !s.humanOnly(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var in passwordChangeReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		if isTooLarge(err) {
			s.tooLarge(w)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	if !validNewPassword(w, in.NewPassword) {
		return
	}
	if in.NewPassword == in.CurrentPassword {
		writeError(w, http.StatusBadRequest, "weak_password", "new password must differ from the current one")
		return
	}
	ctx := r.Context()
	actor := s.staff(r)
	sa, err := s.store.GetStaffAuthByID(ctx, s.store.Pool, actor.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if sa.LockedUntil != nil && sa.LockedUntil.After(time.Now()) {
		s.rateLimited(w, "lockout", "account_locked", time.Until(*sa.LockedUntil))
		return
	}
	ok, err := s.argon.Verify(ctx, in.CurrentPassword, sa.PasswordHash)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "busy", "server busy, retry")
		return
	}
	if !ok {
		s.metrics.loginFailure()
		_, _ = s.store.RecordLoginFailure(ctx, s.store.Pool, actor.ID)
		writeError(w, http.StatusUnauthorized, ledger.CodeUnauthorized, "current password is wrong")
		return
	}
	hash, err := s.argon.Hash(ctx, in.NewPassword)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var version int
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		var err error
		version, err = s.store.SetPassword(ctx, tx, actor.MerchantID, actor.ID, hash)
		if err != nil {
			return err
		}
		return s.auditStaff(r, tx, "staff.password_changed", actor, actor)
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	token, exp, err := auth.IssueToken(s.cfg.JWTSecret, actor.Email, actor.Role, actor.ID, actor.MerchantID, version, s.tokenTTL())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expires_at": exp.UTC().Format(time.RFC3339)})
}

type passwordResetReq struct {
	NewPassword string `json:"new_password"`
}

// resetStaffPassword is the admin path: set a new password for a staff member, revoke their tokens.
func (s *Server) resetStaffPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var in passwordResetReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		if isTooLarge(err) {
			s.tooLarge(w)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	if !validNewPassword(w, in.NewPassword) {
		return
	}
	ctx := r.Context()
	actor := s.staff(r)
	target, err := s.store.GetStaffAuthByID(ctx, s.store.Pool, id)
	if err != nil || target.MerchantID != actor.MerchantID || target.IsService {
		writeError(w, http.StatusNotFound, ledger.CodeNotFound, "staff not found")
		return
	}
	hash, err := s.argon.Hash(ctx, in.NewPassword)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.store.SetPassword(ctx, tx, actor.MerchantID, id, hash); err != nil {
			return err
		}
		return s.auditStaff(r, tx, "staff.password_reset", target.Staff, actor)
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, ledger.CodeNotFound, "staff not found")
			return
		}
		s.internalError(w, r, fmt.Errorf("password reset: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "password_reset"})
}
