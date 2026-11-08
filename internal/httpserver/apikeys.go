package httpserver

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	apiKeyPrefixLen = 8  // hex characters
	apiKeySecretLen = 48 // hex characters (192 bits)
)

type apiKeyReq struct {
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	OutletIDs []string `json:"outlet_ids"`
}

type apiKeyJSON struct {
	ID         uuid.UUID   `json:"id"`
	Name       string      `json:"name"`
	Prefix     string      `json:"prefix"`
	Role       string      `json:"role"`
	OutletIDs  []uuid.UUID `json:"outlet_ids"`
	StaffID    uuid.UUID   `json:"staff_id"`
	CreatedBy  uuid.UUID   `json:"created_by"`
	CreatedAt  time.Time   `json:"created_at"`
	LastUsedAt *time.Time  `json:"last_used_at"`
	RevokedAt  *time.Time  `json:"revoked_at"`
	Secret     string      `json:"secret,omitempty"`
}

func asAPIKey(k store.APIKey) apiKeyJSON {
	ids := k.OutletIDs
	if ids == nil {
		ids = []uuid.UUID{}
	}
	return apiKeyJSON{
		ID: k.ID, Name: k.Name, Prefix: k.Prefix, Role: k.Role, OutletIDs: ids, StaffID: k.StaffID,
		CreatedBy: k.CreatedBy, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt,
	}
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// createAPIKey issues a key. The full secret is in this response only; Till stores a SHA-256 digest.
func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	var in apiKeyReq
	if err := decodeStrict(raw, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid JSON")
		return
	}
	name := strings.TrimSpace(in.Name)
	role := strings.ToLower(strings.TrimSpace(in.Role))
	if name == "" || len(name) > 80 {
		writeError(w, http.StatusBadRequest, "invalid_name", "name is required (80 characters at most)")
		return
	}
	if role != auth.RoleCashier && role != auth.RoleManager {
		writeError(w, http.StatusBadRequest, "invalid_role", "api key role must be cashier or manager")
		return
	}
	if len(in.OutletIDs) == 0 || len(in.OutletIDs) > 50 {
		writeError(w, http.StatusBadRequest, "invalid_outlet", "outlet_ids needs 1 to 50 outlets")
		return
	}
	actor := s.staff(r)
	var outlets []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, rawID := range in.OutletIDs {
		id, err := uuid.Parse(rawID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_outlet", "outlet_id is invalid")
			return
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, err := s.store.GetOutlet(r.Context(), s.store.Pool, actor.MerchantID, id); err != nil {
			writeLedgerError(w, err)
			return
		}
		outlets = append(outlets, id)
	}
	keyID, err := uuid.NewV7()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	prefix, err1 := randHex(apiKeyPrefixLen / 2)
	secret, err2 := randHex(apiKeySecretLen / 2)
	if err1 != nil || err2 != nil {
		s.internalError(w, r, err1)
		return
	}
	var created store.APIKey
	err = s.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		created, err = s.store.CreateAPIKey(r.Context(), tx, store.APIKey{
			ID: keyID, MerchantID: actor.MerchantID, Name: name, Prefix: prefix,
			Hash: store.HashAPIKeySecret(secret), Role: role, OutletIDs: outlets, CreatedBy: actor.ID,
		})
		if err != nil {
			return err
		}
		meta, _ := json.Marshal(map[string]any{"name": name, "role": role, "prefix": prefix})
		mid, sid := actor.MerchantID, actor.ID
		return s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
			MerchantID: &mid, StaffID: &sid, Action: "apikey.created", EntityType: "api_key",
			EntityID: created.ID, Metadata: meta,
		})
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := asAPIKey(created)
	out.Secret = apiKeyPrefix + prefix + "_" + secret
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListAPIKeys(r.Context(), s.store.Pool, s.staff(r).MerchantID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]apiKeyJSON, 0, len(keys))
	for _, k := range keys {
		out = append(out, asAPIKey(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_keys": out})
}

func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	actor := s.staff(r)
	var revoked store.APIKey
	err := s.withTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		revoked, err = s.store.RevokeAPIKey(r.Context(), tx, actor.MerchantID, id)
		if err != nil {
			return err
		}
		meta, _ := json.Marshal(map[string]any{"name": revoked.Name, "prefix": revoked.Prefix})
		mid, sid := actor.MerchantID, actor.ID
		return s.store.InsertAudit(r.Context(), tx, store.AuditEvent{
			MerchantID: &mid, StaffID: &sid, Action: "apikey.revoked", EntityType: "api_key",
			EntityID: id, Metadata: meta,
		})
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, ledger.CodeNotFound, "api key not found")
			return
		}
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, asAPIKey(revoked))
}
