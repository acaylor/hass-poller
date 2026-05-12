package ha

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchStates_Success(t *testing.T) {
	var gotAuth, gotPath, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[
			{"entity_id":"sensor.kitchen_temperature","state":"20.5","attributes":{"unit_of_measurement":"°C","state_class":"measurement"}},
			{"entity_id":"sensor.unavailable_thing","state":"unavailable","attributes":{}}
		]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "secret-token", 2*time.Second)
	states, err := c.FetchStates(context.Background())
	if err != nil {
		t.Fatalf("FetchStates returned error: %v", err)
	}

	if gotPath != "/api/states" {
		t.Errorf("path = %q, want /api/states", gotPath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret-token")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}

	if len(states) != 2 {
		t.Fatalf("got %d states, want 2", len(states))
	}
	if states[0].EntityID != "sensor.kitchen_temperature" {
		t.Errorf("states[0].EntityID = %q", states[0].EntityID)
	}
	if states[0].State != "20.5" {
		t.Errorf("states[0].State = %q", states[0].State)
	}
	if states[0].Attributes.UnitOfMeasurement != "°C" {
		t.Errorf("states[0].Attributes.UnitOfMeasurement = %q", states[0].Attributes.UnitOfMeasurement)
	}
	if states[0].Attributes.StateClass != "measurement" {
		t.Errorf("states[0].Attributes.StateClass = %q", states[0].Attributes.StateClass)
	}
}

func TestFetchStates_TrimsTrailingSlashFromBaseURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/", "tok", time.Second)
	if _, err := c.FetchStates(context.Background()); err != nil {
		t.Fatalf("FetchStates: %v", err)
	}
	if gotPath != "/api/states" {
		t.Errorf("path = %q, want /api/states (no double slash)", gotPath)
	}
}

func TestFetchStates_Non200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"bad token"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", time.Second)
	_, err := c.FetchStates(context.Background())
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q should mention status code", err.Error())
	}
	if !strings.Contains(err.Error(), "bad token") {
		t.Errorf("error %q should include body snippet", err.Error())
	}
}

func TestFetchStates_MalformedJSONReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", time.Second)
	_, err := c.FetchStates(context.Background())
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error %q should mention decode", err.Error())
	}
}

func TestFetchStates_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call

	c := NewClient(srv.URL, "tok", 5*time.Second)
	_, err := c.FetchStates(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}

func TestFetchStates_TransportError(t *testing.T) {
	// Point at a server that's already closed so Do() fails immediately.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	c := NewClient(srv.URL, "tok", 500*time.Millisecond)
	_, err := c.FetchStates(context.Background())
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if !strings.Contains(err.Error(), "/api/states") {
		t.Errorf("error %q should mention endpoint", err.Error())
	}
}

func TestFetchStates_InvalidBaseURL(t *testing.T) {
	// Control character in URL makes http.NewRequestWithContext fail.
	c := NewClient("http://example.com\x7f", "tok", time.Second)
	_, err := c.FetchStates(context.Background())
	if err == nil {
		t.Fatal("expected request-construction error, got nil")
	}
	if !strings.Contains(err.Error(), "create request") {
		t.Errorf("error %q should mention request construction", err.Error())
	}
}
