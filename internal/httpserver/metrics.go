package httpserver

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// metrics is a tiny Prometheus text-format registry. It holds only what Till exports.
type metrics struct {
	mu            sync.Mutex
	requests      map[[2]string]uint64 // route, status
	hist          map[string]*histogram
	loginFailures uint64
	rateLimited   map[string]uint64 // scope
}

type histogram struct {
	counts []uint64 // per bucket, non-cumulative
	sum    float64
	n      uint64
}

var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

func newMetrics() *metrics {
	return &metrics{
		requests:    map[[2]string]uint64{},
		hist:        map[string]*histogram{},
		rateLimited: map[string]uint64{},
	}
}

func (m *metrics) observeRequest(route string, status int, seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[[2]string{route, fmt.Sprint(status)}]++
	h := m.hist[route]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(durationBuckets)+1)}
		m.hist[route] = h
	}
	i := sort.SearchFloat64s(durationBuckets, seconds)
	h.counts[i]++
	h.sum += seconds
	h.n++
}

func (m *metrics) loginFailure() {
	m.mu.Lock()
	m.loginFailures++
	m.mu.Unlock()
}

func (m *metrics) limited(scope string) {
	m.mu.Lock()
	m.rateLimited[scope]++
	m.mu.Unlock()
}

func esc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func (m *metrics) write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Fprintln(w, "# HELP http_requests_total HTTP requests by route pattern and status.")
	fmt.Fprintln(w, "# TYPE http_requests_total counter")
	keys := make([][2]string, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		fmt.Fprintf(w, "http_requests_total{route=\"%s\",status=\"%s\"} %d\n", esc(k[0]), k[1], m.requests[k])
	}

	fmt.Fprintln(w, "# HELP http_request_duration_seconds HTTP request latency by route pattern.")
	fmt.Fprintln(w, "# TYPE http_request_duration_seconds histogram")
	routes := make([]string, 0, len(m.hist))
	for r := range m.hist {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	for _, r := range routes {
		h := m.hist[r]
		var cum uint64
		for i, le := range durationBuckets {
			cum += h.counts[i]
			fmt.Fprintf(w, "http_request_duration_seconds_bucket{route=\"%s\",le=\"%g\"} %d\n", esc(r), le, cum)
		}
		cum += h.counts[len(durationBuckets)]
		fmt.Fprintf(w, "http_request_duration_seconds_bucket{route=\"%s\",le=\"+Inf\"} %d\n", esc(r), cum)
		fmt.Fprintf(w, "http_request_duration_seconds_sum{route=\"%s\"} %g\n", esc(r), h.sum)
		fmt.Fprintf(w, "http_request_duration_seconds_count{route=\"%s\"} %d\n", esc(r), h.n)
	}

	fmt.Fprintln(w, "# HELP login_failures_total Failed login attempts (bad password, unknown email, disabled account).")
	fmt.Fprintln(w, "# TYPE login_failures_total counter")
	fmt.Fprintf(w, "login_failures_total %d\n", m.loginFailures)

	fmt.Fprintln(w, "# HELP rate_limited_total Requests rejected by a rate limiter or account lockout.")
	fmt.Fprintln(w, "# TYPE rate_limited_total counter")
	scopes := make([]string, 0, len(m.rateLimited))
	for s := range m.rateLimited {
		scopes = append(scopes, s)
	}
	sort.Strings(scopes)
	if len(scopes) == 0 {
		fmt.Fprintln(w, "rate_limited_total 0")
	}
	for _, s := range scopes {
		fmt.Fprintf(w, "rate_limited_total{scope=\"%s\"} %d\n", esc(s), m.rateLimited[s])
	}
}
