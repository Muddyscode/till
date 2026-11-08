package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type reqInfoKey struct{}

// reqInfo is a mutable record the outer logging middleware shares with inner handlers,
// because context values only flow inward.
type reqInfo struct {
	RequestID  string
	MerchantID string
	StaffID    string
	APIKeyID   string
	Err        error
}

func infoFrom(ctx context.Context) *reqInfo {
	if v, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
		return v
	}
	return &reqInfo{}
}

// RequestIDFrom returns the request id of r, or "".
func RequestIDFrom(r *http.Request) string { return infoFrom(r.Context()).RequestID }

// errorSetter lets writeLedgerError hand the underlying error of a 500 to the access log.
type errorSetter interface{ SetError(error) }

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
	info   *reqInfo
}

func (w *recorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *recorder) SetError(err error)          { w.info.Err = err }
func (w *recorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func validRequestID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// observe assigns the request id, then logs and counts every request.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		info := &reqInfo{RequestID: id}
		rec := &recorder{ResponseWriter: w, info: info}
		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, info)))
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		elapsed := time.Since(start)
		route := "unmatched"
		if rc := chi.RouteContext(r.Context()); rc != nil {
			if p := rc.RoutePattern(); p != "" {
				route = p
			}
		}
		s.metrics.observeRequest(route, rec.status, elapsed.Seconds())

		level := slog.LevelInfo
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case route == "/livez" || route == "/readyz" || route == "/metrics":
			level = slog.LevelDebug
		}
		attrs := []any{
			"method", r.Method, "route", route, "path", r.URL.Path, "status", rec.status,
			"duration_ms", float64(elapsed.Microseconds()) / 1000, "bytes", rec.bytes, "request_id", id,
		}
		if info.MerchantID != "" {
			attrs = append(attrs, "merchant_id", info.MerchantID)
		}
		if info.StaffID != "" {
			attrs = append(attrs, "staff_id", info.StaffID)
		}
		if info.APIKeyID != "" {
			attrs = append(attrs, "api_key_id", info.APIKeyID)
		}
		if info.Err != nil {
			attrs = append(attrs, "err", info.Err.Error())
		}
		s.log.Log(r.Context(), level, "request", attrs...)
	})
}

// recoverer turns a panic into a logged JSON 500.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "panic", v, "request_id", RequestIDFrom(r), "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// internalError logs err against the request and writes the generic 500.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	infoFrom(r.Context()).Err = err
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// cors allows only origins listed in TILL_CORS_ORIGINS. With none listed it does nothing.
func (s *Server) cors(next http.Handler) http.Handler {
	if len(s.cfg.CORSOrigins) == 0 {
		return next
	}
	allowed := make(map[string]bool, len(s.cfg.CORSOrigins))
	for _, o := range s.cfg.CORSOrigins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Origin")
		if !allowed[origin] {
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				writeError(w, http.StatusForbidden, "cors_forbidden", "origin not allowed")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, Retry-After, Idempotent-Replayed")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-ID")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "route not found")
}

func (s *Server) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}
