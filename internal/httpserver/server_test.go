package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubChecker struct {
	lastPoll time.Time
	dbOK     bool
}

func (s stubChecker) LastSuccessfulPoll() time.Time           { return s.lastPoll }
func (s stubChecker) DBHealthy(ctx context.Context) bool      { return s.dbOK }

func TestHandleHealth(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name       string
		checker    stubChecker
		wantStatus int
		wantBody   map[string]any
	}{
		{
			name:       "healthy when poll recent and db ok",
			checker:    stubChecker{lastPoll: now.Add(-30 * time.Second), dbOK: true},
			wantStatus: http.StatusOK,
			wantBody:   map[string]any{"healthy": true, "poll_ok": true, "db_ok": true},
		},
		{
			name:       "unhealthy when poll stale",
			checker:    stubChecker{lastPoll: now.Add(-10 * time.Minute), dbOK: true},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   map[string]any{"healthy": false, "poll_ok": false, "db_ok": true},
		},
		{
			name:       "unhealthy when never polled",
			checker:    stubChecker{lastPoll: time.Time{}, dbOK: true},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   map[string]any{"healthy": false, "poll_ok": false, "db_ok": true},
		},
		{
			name:       "unhealthy when db down",
			checker:    stubChecker{lastPoll: now, dbOK: false},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   map[string]any{"healthy": false, "poll_ok": true, "db_ok": false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(":0", tt.checker)
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rec := httptest.NewRecorder()

			s.handleHealth(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}

			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal body: %v (body=%q)", err, rec.Body.String())
			}
			for k, v := range tt.wantBody {
				if body[k] != v {
					t.Errorf("body[%q] = %v, want %v", k, body[k], v)
				}
			}
			if _, ok := body["last_poll"]; !ok {
				t.Errorf("body missing last_poll field")
			}
		})
	}
}

func TestServer_MetricsEndpoint(t *testing.T) {
	s := New(":0", stubChecker{lastPoll: time.Now(), dbOK: true})

	ts := httptest.NewServer(s.srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestServer_ListenAndServeAndShutdown(t *testing.T) {
	s := New("127.0.0.1:0", stubChecker{lastPoll: time.Now(), dbOK: true})

	// Replace the listener-bound server with one we can drive directly.
	ts := httptest.NewServer(s.srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()

	// Exercise Shutdown path on a server that hasn't started — it should not error.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown returned: %v", err)
	}

	// ListenAndServe on the (now shutdown) server should return ErrServerClosed.
	err = s.ListenAndServe()
	if err == nil || !strings.Contains(err.Error(), "Server closed") {
		t.Errorf("ListenAndServe after Shutdown = %v, want ErrServerClosed", err)
	}
}

func TestAtomicTime_StoreLoad(t *testing.T) {
	var at AtomicTime

	// Zero value: Load() before any Store() returns zero Time.
	if got := at.Load(); !got.IsZero() {
		t.Errorf("Load() on fresh AtomicTime = %v, want zero time", got)
	}

	now := time.Unix(1_700_000_000, 12345).UTC()
	at.Store(now)
	got := at.Load()
	if !got.Equal(now) {
		t.Errorf("Load() = %v, want %v", got, now)
	}

	// Overwrite.
	later := now.Add(time.Hour)
	at.Store(later)
	if got := at.Load(); !got.Equal(later) {
		t.Errorf("Load() after second Store = %v, want %v", got, later)
	}
}
