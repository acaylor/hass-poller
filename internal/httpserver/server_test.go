package httpserver

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type stubChecker struct {
	lastPoll time.Time
	dbOK     bool
}

func (s stubChecker) LastSuccessfulPoll() time.Time      { return s.lastPoll }
func (s stubChecker) DBHealthy(ctx context.Context) bool { return s.dbOK }

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
			s := New(":0", tt.checker, time.Minute)
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
	s := New(":0", stubChecker{lastPoll: time.Now(), dbOK: true}, time.Minute)

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

func TestHandleHealth_LongPollInterval(t *testing.T) {
	for _, age := range []time.Duration{4 * time.Minute, 11 * time.Minute} {
		s := New(":0", stubChecker{lastPoll: time.Now().Add(-age), dbOK: true}, 5*time.Minute)
		rec := httptest.NewRecorder()
		s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		want := http.StatusOK
		if age > 10*time.Minute {
			want = http.StatusServiceUnavailable
		}
		if rec.Code != want {
			t.Fatalf("poll age %s: status %d, want %d", age, rec.Code, want)
		}
	}
}

func TestHandleHealth_OverflowingPollInterval(t *testing.T) {
	for _, interval := range []time.Duration{time.Duration(math.MaxInt64)/2 + 1, time.Duration(math.MaxInt64)} {
		s := New(":0", stubChecker{lastPoll: time.Now().Add(-time.Hour), dbOK: true}, interval)
		rec := httptest.NewRecorder()
		s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("interval %s: status %d, want 200", interval, rec.Code)
		}
	}
}
