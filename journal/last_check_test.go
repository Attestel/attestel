package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// last_check_test.go — Wave 5 Lane 5B. The server-side reading boundary.
//
// Three assertions carry the whole clause: an unrecorded boundary is NULL and never a substituted
// default, a read never advances it, and a stamp is monotonic.

func lastCheckFixture(t *testing.T) (*Server, *http.ServeMux) {
	t.Helper()
	cfg := loadConfig()
	cfg.TradesDir = t.TempDir()
	cfg.Secret = testSecret
	cfg.CookieName = "nvda_session"
	srv := &Server{cfg: cfg, http: &http.Client{}}
	mux := http.NewServeMux()
	srv.registerLastCheckAPI(mux)
	return srv, mux
}

func lastCheckCall(t *testing.T, srv *Server, mux *http.ServeMux, method, body, uid string) map[string]any {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "/event-state/last-check", reader)
	if uid != "" {
		req.AddCookie(sessionFor(uid))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if uid == "" {
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a session = %d, want 401 — a guest has no partition",
				method, rec.Code)
		}
		return nil
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("%s = %d: %s", method, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body.String())
	}
	return out
}

func TestAnUnrecordedBoundaryIsNullNotADefault(t *testing.T) {
	// THE COPY RULE, SERVER-SIDE. A boundary the server invented would be indistinguishable at the
	// client from one the user earned, and the heading would then read "since your last check" over
	// a 24-hour window.
	srv, mux := lastCheckFixture(t)
	got := lastCheckCall(t, srv, mux, http.MethodGet, "", "u1")
	if v, present := got["lastCheck"]; !present || v != nil {
		t.Fatalf("lastCheck = %v (present=%v), want an explicit null", v, present)
	}
}

func TestAReadNeverAdvancesTheBoundary(t *testing.T) {
	// Advancing on a read would silently skip past events the user never saw — the one failure this
	// whole object exists to prevent.
	srv, mux := lastCheckFixture(t)
	lastCheckCall(t, srv, mux, http.MethodGet, "", "u1")
	lastCheckCall(t, srv, mux, http.MethodGet, "", "u1")
	got := lastCheckCall(t, srv, mux, http.MethodGet, "", "u1")
	if got["lastCheck"] != nil {
		t.Fatalf("three reads produced a boundary of %v", got["lastCheck"])
	}
}

func TestAStampIsRecordedAndReadBack(t *testing.T) {
	srv, mux := lastCheckFixture(t)
	posted := lastCheckCall(t, srv, mux, http.MethodPost, `{"at":1780000000}`, "u1")
	if posted["lastCheck"] != float64(1780000000) {
		t.Fatalf("POST returned %v", posted["lastCheck"])
	}
	got := lastCheckCall(t, srv, mux, http.MethodGet, "", "u1")
	if got["lastCheck"] != float64(1780000000) {
		t.Fatalf("GET returned %v after a stamp", got["lastCheck"])
	}
}

func TestTheBoundaryIsMonotonic(t *testing.T) {
	// Two tabs racing must not move a user's reading position BACKWARDS: that re-shows events they
	// were already shown, which erodes the same trust as skipping them.
	srv, mux := lastCheckFixture(t)
	lastCheckCall(t, srv, mux, http.MethodPost, `{"at":1780000000}`, "u1")
	got := lastCheckCall(t, srv, mux, http.MethodPost, `{"at":1779000000}`, "u1")
	if got["lastCheck"] != float64(1780000000) {
		t.Fatalf("an earlier stamp moved the boundary back to %v", got["lastCheck"])
	}
}

func TestTheBoundaryIsPerUser(t *testing.T) {
	srv, mux := lastCheckFixture(t)
	lastCheckCall(t, srv, mux, http.MethodPost, `{"at":1780000000}`, "u1")
	got := lastCheckCall(t, srv, mux, http.MethodGet, "", "u2")
	if got["lastCheck"] != nil {
		t.Fatalf("u2 inherited u1's boundary: %v", got["lastCheck"])
	}
}

func TestAGuestIsRefusedRatherThanGivenAPartition(t *testing.T) {
	srv, mux := lastCheckFixture(t)
	lastCheckCall(t, srv, mux, http.MethodGet, "", "")
	lastCheckCall(t, srv, mux, http.MethodPost, `{"at":1780000000}`, "")
}

func TestAPostWithNoBodyStampsNow(t *testing.T) {
	// The ordinary client call: "I have now seen this surface." An empty body is not an error.
	srv, mux := lastCheckFixture(t)
	got := lastCheckCall(t, srv, mux, http.MethodPost, "", "u1")
	at, ok := got["lastCheck"].(float64)
	if !ok || at <= 0 {
		t.Fatalf("lastCheck = %v, want a wall-clock stamp", got["lastCheck"])
	}
}
