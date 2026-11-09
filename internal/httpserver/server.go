package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Muddyscode/till/internal/auth"
	"github.com/Muddyscode/till/internal/config"
	"github.com/Muddyscode/till/internal/ledger"
	"github.com/Muddyscode/till/internal/ratelimit"
	"github.com/Muddyscode/till/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type contextKey int

const staffKey contextKey = 1

type Server struct {
	cfg          config.Config
	store        *store.Store
	log          *slog.Logger
	router       chi.Router
	metrics      *metrics
	argon        *auth.Verifier
	ipLimiter    *ratelimit.Limiter
	emailLimiter *ratelimit.Limiter
	sweeperEvery atomic.Int64 // nanoseconds; 0 = sweeper check off
	lastBeat     atomic.Int64 // unix nanoseconds of the last sweeper pass
	draining     atomic.Bool
}

func New(cfg config.Config, st *store.Store, log *slog.Logger) *Server {
	ipRate, emailRate := cfg.LoginRatePerIP, cfg.LoginRatePerEmail
	if ipRate <= 0 {
		ipRate = 30
	}
	if emailRate <= 0 {
		emailRate = 6
	}
	s := &Server{
		cfg: cfg, store: st, log: log, metrics: newMetrics(),
		argon:        auth.NewVerifier(4),
		ipLimiter:    ratelimit.New(ipRate, float64(ipRate)),
		emailLimiter: ratelimit.New(12, float64(emailRate)),
	}
	r := chi.NewRouter()
	r.Use(s.observe, s.recoverer, s.cors, s.headers)
	r.NotFound(s.notFound)
	r.MethodNotAllowed(s.methodNotAllowed)
	r.Get("/v1/health", s.health)
	r.Get("/livez", s.livez)
	r.Get("/readyz", s.readyz)
	r.Get("/metrics", s.metricsHandler)
	r.Post("/v1/auth/login", s.login)
	r.Post("/v1/webhooks/paystack", s.paystackWebhook)

	r.Group(func(r chi.Router) {
		r.Use(s.authn)
		r.Get("/v1/auth/me", s.me)
		r.Post("/v1/auth/logout", s.logout)
		r.Post("/v1/auth/password", s.changePassword)
		r.Get("/v1/outlets", s.listOutlets)
		r.Get("/v1/products", s.listProducts)
		r.Post("/v1/customers", s.createCustomer)
		r.Get("/v1/customers", s.listCustomers)
		r.Get("/v1/customers/{id}", s.getCustomer)
		r.Post("/v1/customers/{id}/topups", s.topUpCustomer)
		r.Get("/v1/orders", s.listOrdersPage)
		r.Get("/v1/orders/{id}", s.getOrder)
		r.Post("/v1/orders", s.createOrder)
		r.Get("/v1/reports/stock-on-hand", s.stockOnHand)
		r.Get("/v1/reports/low-stock", s.lowStock)
		s.registerOperations(r)

		r.Group(func(r chi.Router) {
			r.Use(s.requireRole(auth.RoleAdmin, auth.RoleManager))
			r.With(s.adminPricing).Post("/v1/products", s.createProduct)
			r.With(s.adminPricing).Patch("/v1/products/{id}", s.patchProduct)
			r.Post("/v1/stock/receipts", s.receiveStock)
			r.Post("/v1/stock/adjustments", s.adjustStock)
			r.Post("/v1/orders/{id}/refund", s.refundOrder)
			r.Get("/v1/reports/daily-sales", s.dailySales)
			r.Get("/v1/audit", s.listAuditPage)
			r.Get("/v1/unmatched-payments", s.listUnmatched)
			r.Post("/v1/unmatched-payments/{id}/apply", s.applyUnmatched)
			r.Post("/v1/unmatched-payments/{id}/refund", s.refundUnmatched)
			r.Post("/v1/unmatched-payments/{id}/refunded", s.settleUnmatched)
		})

		r.Group(func(r chi.Router) {
			r.Use(s.requireRole(auth.RoleAdmin))
			r.Post("/v1/staff", s.createStaff)
			r.Get("/v1/staff", s.listStaff)
			r.Post("/v1/staff/{id}/disable", s.disableStaff)
			r.Post("/v1/staff/{id}/password-reset", s.resetStaffPassword)
			r.Post("/v1/api-keys", s.createAPIKey)
			r.Get("/v1/api-keys", s.listAPIKeys)
			r.Delete("/v1/api-keys/{id}", s.revokeAPIKey)
			r.Post("/v1/outlets", s.createOutlet)
			r.Get("/v1/invariants", s.invariants)
			r.Post("/v1/accounts", s.createAccount)
			r.Get("/v1/accounts", s.listAccounts)
			r.Get("/v1/accounts/{id}", s.getAccount)
			r.Get("/v1/accounts/{id}/statement", s.statement)
			r.Post("/v1/accounts/{id}/close", s.closeAccount)
			r.Post("/v1/transfers", s.postTransfer)
			r.Get("/v1/transfers", s.listJournalsPaged(true))
			r.Get("/v1/transfers/{id}", s.getJournal)
			r.Post("/v1/journals", s.postJournal)
			r.Get("/v1/journals", s.listJournalsPaged(false))
			r.Get("/v1/journals/{id}", s.getJournal)
			r.Post("/v1/journals/{id}/reversal", s.reverseJournal)
		})
	})

	s.router = r
	return s
}

func (s *Server) Router() chi.Router { return s.router }

func (s *Server) poster() ledger.StorePoster {
	return ledger.StorePoster{
		LockAccounts:  s.store.LockAccounts,
		InsertJournal: s.store.InsertJournal,
		InsertPosting: s.store.InsertPosting,
		UpdateBalance: s.store.UpdateBalance,
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unhealthy", "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func WithStaff(ctx context.Context, st store.Staff) context.Context {
	return context.WithValue(ctx, staffKey, st)
}

func StaffFrom(r *http.Request) store.Staff {
	return r.Context().Value(staffKey).(store.Staff)
}

func (s *Server) staff(r *http.Request) store.Staff {
	return StaffFrom(r)
}

func (s *Server) parseID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, ledger.CodeNotFound, "not found")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		if isTooLarge(err) {
			s.tooLarge(w)
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "could not read body")
		return nil, false
	}
	return raw, true
}

func (s *Server) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return store.RetryDeadlock(func() error {
		return s.store.WithTx(ctx, fn)
	})
}

func limitParam(r *http.Request, def, max int) int {
	v := strings.TrimSpace(r.URL.Query().Get("limit"))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func Handler(s *Server) http.Handler {
	return http.TimeoutHandler(s.router, 30*time.Second, `{"error":{"code":"timeout","message":"request timed out"}}`)
}
