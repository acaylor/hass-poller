package httpserver

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HealthChecker provides health status for the application.
type HealthChecker interface {
	LastSuccessfulPoll() time.Time
	DBHealthy(ctx context.Context) bool
}

type Server struct {
	srv     *http.Server
	checker HealthChecker
	maxAge  time.Duration
}

func New(addr string, checker HealthChecker, pollInterval time.Duration) *Server {
	// Saturate rather than wrap when doubling a very large valid duration.
	maxAge := time.Duration(math.MaxInt64)
	if pollInterval <= time.Duration(math.MaxInt64)/2 {
		maxAge = max(2*time.Minute, 2*pollInterval)
	}
	s := &Server{
		checker: checker,
		maxAge:  maxAge,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.Handle("/metrics", promhttp.Handler())

	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	return s
}

func (s *Server) ListenAndServe() error {
	return s.srv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	lastPoll := s.checker.LastSuccessfulPoll()
	pollOK := !lastPoll.IsZero() && time.Since(lastPoll) < s.maxAge
	dbOK := s.checker.DBHealthy(ctx)

	status := http.StatusOK
	if !pollOK || !dbOK {
		status = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"healthy":   pollOK && dbOK,
		"poll_ok":   pollOK,
		"db_ok":     dbOK,
		"last_poll": lastPoll,
	})
}
