package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// routeseams_test.go — proves the Wave 0 seam is WIRED.
//
// The seam exists so Wave 1 Lane A can attach /subscriptions (contract §2.1) and /event-state (§2.3)
// from its own files without editing handlers.go. Wave 0 shipped it empty and asserted the route
// table was byte-for-byte unchanged; that assertion was a tripwire, explicitly scoped to fail "the
// moment a registrar is attached, which is exactly when a lane must also update its own
// expectations". Lane 1A attached two registrars, so this file now proves the other half: the seam
// really is the mechanism those routes arrive through, and nothing else was edited to make them work.
//
// What it does NOT prove is behaviour — status codes, auth and payloads are subscriptions_test.go's
// and event_state_test.go's job. This file only answers "did they arrive, and did they arrive
// through the seam".

// wantSubscriptionRegistrars counts the init()s on this seam: Lane 1A's two — subscriptions.go and
// event_state.go — plus Wave 5 Lane 5B's last_check.go, which extends §2.3 with the server-side
// "when did you last look at what changed" boundary. It is spelled out rather than derived so that
// an unexpected registrar — from a later wave, or from a merge that duplicated an init() — is a
// test failure and not a silent change in how the journal's routes are assembled. Bumped 2 -> 3 by
// Wave 5B, per this file's header.
const wantSubscriptionRegistrars = 3

func TestSubscriptionSeamIsWired(t *testing.T) {
	if len(subscriptionRouteRegistrars) != wantSubscriptionRegistrars {
		t.Fatalf("subscriptionRouteRegistrars = %d, want %d (subscriptions.go, event_state.go and last_check.go, one init() each)",
			len(subscriptionRouteRegistrars), wantSubscriptionRegistrars)
	}
}

// TestSubscriptionSeamRegistersTheLaneRoutes runs the hook against a FRESH mux — one that has been
// through no other registration — so a route that resolves there can only have come from the seam.
// It then repeats the check on the real route table, because a seam that works in isolation but is
// not actually called from routes() would be just as broken.
func TestSubscriptionSeamRegistersTheLaneRoutes(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	theses, err := openThesisStore(dir)
	if err != nil {
		t.Fatalf("openThesisStore: %v", err)
	}
	srv := &Server{
		cfg:    Config{TradesDir: dir, Secret: "test-secret", CookieName: "nvda_session"},
		store:  store,
		theses: theses,
		http:   &http.Client{Timeout: time.Second},
	}

	// The lane's routes are session-authenticated, so an anonymous request resolves to 401. What
	// matters here is that it is NOT 404: the mux knows the path. Asserting 401 rather than "any
	// non-404" also catches a route accidentally mounted without requireAuth.
	paths := []string{"/subscriptions", "/event-state"}

	mux := http.NewServeMux()
	srv.registerSubscriptionRoutes(mux)

	for _, path := range paths {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusNotFound {
			t.Fatalf("GET %s on a bare mux = 404 — the seam did not register it", path)
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s on a bare mux = %d, want 401 (the route exists and requires a session)", path, rec.Code)
		}
	}

	// The seam is wired into the real route table, so the same paths must resolve there too.
	real := httptest.NewServer(srv.routes())
	defer real.Close()
	for _, path := range paths {
		res, err := http.Get(real.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s on the real mux = %d, want 401", path, res.StatusCode)
		}
	}
}
