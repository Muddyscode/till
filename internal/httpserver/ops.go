package httpserver

import (
	"net/http"
	"time"
)

// ExpectSweeper turns on the sweeper check in /readyz. The sweeper must call
// SweeperBeat at least every `every`; /readyz fails after 3x that.
func (s *Server) ExpectSweeper(every time.Duration) {
	s.sweeperEvery.Store(int64(every))
	s.lastBeat.Store(time.Now().UnixNano())
}

// SweeperBeat records that the sweeper completed a pass.
func (s *Server) SweeperBeat() { s.lastBeat.Store(time.Now().UnixNano()) }

// SetDraining makes /readyz fail so the load balancer stops sending traffic during shutdown.
func (s *Server) SetDraining() { s.draining.Store(true) }

func (s *Server) livez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{"database": "ok", "sweeper": "disabled"}
	ready := true
	if s.draining.Load() {
		checks["draining"] = "shutting down"
		ready = false
	}
	if err := s.store.Ping(r.Context()); err != nil {
		checks["database"] = "unreachable"
		ready = false
	}
	if every := time.Duration(s.sweeperEvery.Load()); every > 0 {
		age := time.Since(time.Unix(0, s.lastBeat.Load()))
		if age > 3*every {
			checks["sweeper"] = "stale"
			ready = false
		} else {
			checks["sweeper"] = "ok"
		}
	}
	status, code := "ready", http.StatusOK
	if !ready {
		status, code = "unready", http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"status": status, "checks": checks})
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.write(w)
}
