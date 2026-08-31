package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// automation_test.go — Phase 1's browser-facing surface is READ-ONLY, signed-in, and store-backed.

func automationServer(t *testing.T, handler http.HandlerFunc) (*Server, *deniedTransport) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	parsed, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &deniedTransport{
		allow: map[string]bool{parsed.Host: true}, base: http.DefaultTransport,
	}
	cfg := loadConfig()
	cfg.EventsURL = upstream.URL
	// Configured provider keys must not change a read path or open a second host.
	cfg.AlphaVantageKey = "must-not-fetch"
	cfg.MarketauxKey = "must-not-fetch"
	return &Server{
		cfg: cfg, cache: newCache(), http: &http.Client{Transport: transport, Timeout: time.Second},
	}, transport
}

func automationStatusFake(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/automation/status" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"automationEnabled": true,
			"selectedLanes":     []string{"ingest"},
			"lanes": []map[string]any{{
				"lane": "ingest", "owner": "events", "runner": "events",
				"enabled": true, "laneFlagEnv": "INGEST_ENABLED", "laneFlagEnabled": true,
				"intervalSeconds": 3600, "running": false,
				"lastStatus": "success", "lastSuccessAt": "2026-08-23T11:00:00Z",
				"lastFailureAt": nil, "consecutiveFailures": 0,
				"secondsSinceLastSuccess": 3600,
			}},
			"recentRuns": []map[string]any{{
				"id": "aut_0123456789abcdef", "lane": "ingest", "status": "success",
			}},
		})
	}
}

func automationRequest(t *testing.T, srv *Server, uid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/automation/status", nil)
	if uid != "" {
		req.AddCookie(&http.Cookie{
			Name: srv.cfg.CookieName, Value: testSessionToken(srv.cfg.Secret, uid),
		})
	}
	w := httptest.NewRecorder()
	mux := http.NewServeMux()
	srv.registerEventRoutes(mux)
	mux.ServeHTTP(w, req)
	return w
}

func TestAutomationStatusRequiresASignedInUser(t *testing.T) {
	srv, _ := automationServer(t, automationStatusFake(t))
	if code := automationRequest(t, srv, "").Code; code != http.StatusUnauthorized {
		t.Fatalf("guest status=%d, want 401", code)
	}
}

func TestAutomationStatusReadsTheStoreAndCallsNoProvider(t *testing.T) {
	srv, transport := automationServer(t, automationStatusFake(t))
	w := automationRequest(t, srv, "u1")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["automationEnabled"] != true {
		t.Fatalf("the store's answer must be passed through: %v", body)
	}
	lanes, _ := body["lanes"].([]any)
	if len(lanes) != 1 {
		t.Fatalf("want the store's lane list, got %v", body["lanes"])
	}
	// Exactly one host was reached, and it is the events service.
	for _, host := range transport.attempted() {
		if host == "" {
			continue
		}
		if !transport.allow[host] {
			t.Fatalf("a read path reached a provider host: %s", host)
		}
	}
}

func TestAutomationStatusDegradesHonestlyWhenTheStoreIsDown(t *testing.T) {
	srv, _ := automationServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	w := automationRequest(t, srv, "u1")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["automationEnabled"] != false {
		t.Fatalf("an unreachable store must never read as enabled: %v", body)
	}
	degraded, _ := body["degraded"].([]any)
	if len(degraded) == 0 {
		t.Fatalf("degradation must be stated, not implied: %v", body)
	}
}

func TestNoAutomationMutationRouteIsExposedToTheBrowser(t *testing.T) {
	srv, _ := automationServer(t, automationStatusFake(t))
	mux := http.NewServeMux()
	srv.registerEventRoutes(mux)

	for _, path := range []string{
		"/api/automation/lease",
		"/api/automation/runs/aut_1/complete",
		"/api/automation/run",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.AddCookie(&http.Cookie{
			Name: srv.cfg.CookieName, Value: testSessionToken(srv.cfg.Secret, "u1"),
		})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s must not be reachable from a browser (status=%d)", path, w.Code)
		}
	}
}
