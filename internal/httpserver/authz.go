package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/ledger"
)

// adminPricing keeps price and cost under admin control. Creating a product sets a price,
// so it is admin only. Managers may still PATCH name, reorder_level and active; a PATCH that
// names price_minor or avg_cost_minor from anyone but an admin is 403.
func (s *Server) adminPricing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.staff(r).Role == auth.RoleAdmin {
			next.ServeHTTP(w, r)
			return
		}
		deny := func() {
			writeError(w, http.StatusForbidden, ledger.CodeForbidden, "only an admin can set price, cost or tax class")
		}
		if r.Method == http.MethodPost {
			deny()
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			if isTooLarge(err) {
				s.tooLarge(w)
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_json", "could not read body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) == nil {
			// encoding/json matches keys case-insensitively, so the guard must too.
			for k := range fields {
				if strings.EqualFold(k, "price_minor") || strings.EqualFold(k, "avg_cost_minor") || strings.EqualFold(k, "tax_class") {
					deny()
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
