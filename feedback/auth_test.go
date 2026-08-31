package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// auth_test.go — the D-16 access-control policy, as executable rules.
//
// Before this, every feedback route was unauthenticated: anyone who could reach port 8098 could read
// every tester's submission and DELETE it. The policy now is:
//
//	/health            public
//	POST /feedback     any signed-in tester; the SERVER decides who owns the record
//	GET  /feedback     your own records; admins see everything, including legacy ownerless records
//	summary/PATCH/DELETE  admin allow-list only — 401 for a guest, 403 for a signed-in non-admin
//
// These tests use the same token format the auth service mints, so they exercise the REAL verifier.
// Nothing here weakens production verification and there is no test-only bypass.

const testSecret = "test-secret-not-the-dev-default"

const (
	uidAlice = "usr_alice_0000000000000001"
	uidBob   = "usr_bob_0000000000000002"
	uidAdmin = "usr_admin_000000000000003"
)

// mint builds a session token exactly as auth/token.go's issueToken does.
func mint(t *testing.T, secret, uid string, ttl time.Duration) string {
	t.Helper()
	now := time.Now()
	raw, err := json.Marshal(sessionPayload{UID: uid, IAT: now.Unix(), Exp: now.Add(ttl).Unix()})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	body := b64.EncodeToString(raw)
	return body + "." + signPayload(secret, body)
}

// session returns a Cookie header value carrying a valid token for uid.
func session(t *testing.T, uid string) string {
	t.Helper()
	return "nvda_session=" + mint(t, testSecret, uid, time.Hour)
}

type testEnv struct {
	srv *Server
	h   http.Handler
	dir string
}

// newTestEnv builds a server over a temp dir. rawSeed, when non-empty, is written to feedback.json
// VERBATIM so a fixture's on-disk shape (e.g. a legacy record with no owner key at all) is exact.
func newTestEnv(t *testing.T, adminUIDs []string, rawSeed string) *testEnv {
	t.Helper()
	dir := t.TempDir()
	if rawSeed != "" {
		if err := os.WriteFile(filepath.Join(dir, "feedback.json"), []byte(rawSeed), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	store, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	cfg := Config{
		Port: "0", FeedbackDir: dir,
		Secret: testSecret, CookieName: "nvda_session",
		CORSOrigins: []string{"http://localhost:5173", "http://localhost:4173"},
		AdminUIDs:   adminUIDs,
	}
	srv := &Server{cfg: cfg, store: store}
	return &testEnv{srv: srv, h: srv.routes(), dir: dir}
}

// do issues a request. cookie and body may be "".
func (e *testEnv) do(t *testing.T, method, path, cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// submit posts one submission as uid and returns the created id.
func (e *testEnv) submit(t *testing.T, uid, message string) string {
	t.Helper()
	w := e.do(t, "POST", "/feedback", session(t, uid), `{"category":"bug","rating":4,"message":"`+message+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("submit as %s: want 201, got %d (%s)", uid, w.Code, w.Body.String())
	}
	var got struct {
		Feedback publicFeedback `json:"feedback"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return got.Feedback.ID
}

func decodeList(t *testing.T, w *httptest.ResponseRecorder) (items []publicFeedback, unread int, admin bool) {
	t.Helper()
	var got struct {
		Feedback []publicFeedback `json:"feedback"`
		Unread   int              `json:"unread"`
		Admin    bool             `json:"admin"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v (%s)", err, w.Body.String())
	}
	return got.Feedback, got.Unread, got.Admin
}

// ───────────────────────────────── authentication ─────────────────────────────────

// Health stays public — a load balancer has no cookie — and must not leak any submission.
func TestHealthIsPublicAndRevealsNoFeedback(t *testing.T) {
	e := newTestEnv(t, nil, "")
	e.submit(t, uidAlice, "a private complaint about the chart")

	w := e.do(t, "GET", "/health", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("health: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "private complaint") || strings.Contains(body, "feedback\":[") {
		t.Errorf("health leaked feedback data: %s", body)
	}
}

// Every non-health route refuses a caller with no cookie. This is the exposure D-16 named: GET and
// DELETE in particular used to succeed with no credentials at all.
func TestNoCookieIsRejectedEverywhere(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, "")
	id := e.submit(t, uidAlice, "seeded")

	cases := []struct {
		method, path, body string
	}{
		{"POST", "/feedback", `{"category":"bug","message":"anonymous"}`},
		{"GET", "/feedback", ""},
		{"GET", "/feedback/summary", ""},
		{"PATCH", "/feedback/" + id, `{"status":"reviewed"}`},
		{"DELETE", "/feedback/" + id, ""},
	}
	for _, c := range cases {
		w := e.do(t, c.method, c.path, "", c.body)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no cookie: want 401, got %d", c.method, c.path, w.Code)
		}
	}

	// And the record survived the unauthenticated DELETE attempt.
	if _, ok := e.srv.store.Get(id); !ok {
		t.Error("an unauthenticated DELETE destroyed the record")
	}
}

// A token that is malformed, forged, or expired is a guest — the verifier fails closed on each.
func TestBadTokensAreRejected(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, "")

	expired := mint(t, testSecret, uidAlice, -time.Hour)        // correctly signed, exp in the past
	forged := mint(t, "some-other-secret", uidAdmin, time.Hour) // right shape, wrong secret
	noSubject := func() string {                                // valid signature, empty uid
		raw, _ := json.Marshal(sessionPayload{UID: "", IAT: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix()})
		b := b64.EncodeToString(raw)
		return b + "." + signPayload(testSecret, b)
	}()

	bad := map[string]string{
		"malformed (no dot)": "nvda_session=not-a-token",
		"malformed (empty)":  "nvda_session=",
		"malformed (no sig)": "nvda_session=" + strings.Split(mint(t, testSecret, uidAlice, time.Hour), ".")[0] + ".",
		"bad signature":      "nvda_session=" + forged,
		"tampered payload":   "nvda_session=" + "eyJ1aWQiOiJ1c3JfYWRtaW4ifQ." + strings.Split(mint(t, testSecret, uidAlice, time.Hour), ".")[1],
		"expired":            "nvda_session=" + expired,
		"no subject":         "nvda_session=" + noSubject,
		"wrong cookie name":  "other_cookie=" + mint(t, testSecret, uidAlice, time.Hour),
		"bad payload base64": "nvda_session=!!!." + signPayload(testSecret, "!!!"),
	}
	for name, cookie := range bad {
		if w := e.do(t, "GET", "/feedback", cookie, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("GET /feedback with %s: want 401, got %d", name, w.Code)
		}
		if w := e.do(t, "POST", "/feedback", cookie, `{"category":"bug","message":"x"}`); w.Code != http.StatusUnauthorized {
			t.Errorf("POST /feedback with %s: want 401, got %d", name, w.Code)
		}
		if w := e.do(t, "GET", "/feedback/summary", cookie, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("GET /feedback/summary with %s: want 401, got %d", name, w.Code)
		}
	}
}

// The forged-admin token above is the interesting one: it names an allow-listed uid, so it must be
// the SIGNATURE that stops it, not the identity.
func TestForgedAdminTokenIsRejectedBeforeTheAllowList(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, "")
	forged := "nvda_session=" + mint(t, "attacker-secret", uidAdmin, time.Hour)

	for _, p := range []string{"/feedback", "/feedback/summary"} {
		if w := e.do(t, "GET", p, forged, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with a forged admin token: want 401, got %d", p, w.Code)
		}
	}
}

// A valid session does everything policy permits.
func TestValidSessionSucceedsWherePolicyPermits(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, "")

	if w := e.do(t, "POST", "/feedback", session(t, uidAlice), `{"category":"idea","rating":5,"message":"nice"}`); w.Code != http.StatusCreated {
		t.Errorf("signed-in POST: want 201, got %d (%s)", w.Code, w.Body.String())
	}
	if w := e.do(t, "GET", "/feedback", session(t, uidAlice), ""); w.Code != http.StatusOK {
		t.Errorf("signed-in GET: want 200, got %d", w.Code)
	}
	if w := e.do(t, "GET", "/feedback/summary", session(t, uidAdmin), ""); w.Code != http.StatusOK {
		t.Errorf("admin summary: want 200, got %d", w.Code)
	}
}

// ───────────────────────────────── authorization ─────────────────────────────────

// A tester needs no privilege to submit, and the server — not the request — decides the owner.
func TestServerStampsOwnershipFromTheToken(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, "")
	id := e.submit(t, uidAlice, "stamped by the server")

	rec, ok := e.srv.store.Get(id)
	if !ok {
		t.Fatal("record not stored")
	}
	if rec.Owner != uidAlice {
		t.Errorf("owner must come from the verified session: want %q, got %q", uidAlice, rec.Owner)
	}
}

// A client cannot name someone else as the owner, nor promote itself, by putting fields in the body.
func TestClientSuppliedOwnerAndAdminFieldsAreInert(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, "")

	body := `{"category":"bug","message":"forge attempt","owner":"` + uidBob + `","admin":true,"isAdmin":true,"role":"admin"}`
	w := e.do(t, "POST", "/feedback", session(t, uidAlice), body)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", w.Code, w.Body.String())
	}
	var got struct {
		Feedback publicFeedback `json:"feedback"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rec, ok := e.srv.store.Get(got.Feedback.ID)
	if !ok {
		t.Fatal("record not stored")
	}
	if rec.Owner != uidAlice {
		t.Errorf("body-supplied owner won: want %q, got %q", uidAlice, rec.Owner)
	}

	// The claimed privilege buys nothing: Alice is still not an admin on any route.
	if _, _, admin := decodeList(t, e.do(t, "GET", "/feedback", session(t, uidAlice), "")); admin {
		t.Error("a request body granted admin:true")
	}
	if w := e.do(t, "GET", "/feedback/summary", session(t, uidAlice), ""); w.Code != http.StatusForbidden {
		t.Errorf("summary after claiming admin in a body: want 403, got %d", w.Code)
	}
	// Nor does a header or query parameter, which are equally client-controlled.
	r := httptest.NewRequest("GET", "/feedback/summary?admin=true", nil)
	r.Header.Set("Cookie", session(t, uidAlice))
	r.Header.Set("X-Admin", "true")
	r.Header.Set("X-Feedback-Admin", uidAdmin)
	w2 := httptest.NewRecorder()
	e.h.ServeHTTP(w2, r)
	if w2.Code != http.StatusForbidden {
		t.Errorf("summary with an admin header/query: want 403, got %d", w2.Code)
	}
}

// A normal user's list is exactly their own records — not Bob's, and not the legacy ones.
func TestNormalUserSeesOnlyTheirOwnRecords(t *testing.T) {
	legacy := `[{"id":"legacy-1","createdAt":1700000000,"updatedAt":1700000000,` +
		`"category":"ux","rating":3,"message":"legacy ownerless note","status":"new"}]`
	e := newTestEnv(t, []string{uidAdmin}, legacy)

	e.submit(t, uidAlice, "alice one")
	e.submit(t, uidAlice, "alice two")
	e.submit(t, uidBob, "bob secret")

	items, unread, admin := decodeList(t, e.do(t, "GET", "/feedback", session(t, uidAlice), ""))
	if admin {
		t.Error("a normal user must not be flagged admin")
	}
	if len(items) != 2 {
		t.Fatalf("want 2 own records, got %d: %+v", len(items), items)
	}
	for _, f := range items {
		if !strings.HasPrefix(f.Message, "alice") {
			t.Errorf("leaked a record that is not the caller's: %q", f.Message)
		}
	}
	if unread != 2 {
		t.Errorf("unread must count only the caller's own new records, got %d", unread)
	}

	// Explicitly: neither Bob's record nor the legacy one appears anywhere in the raw response.
	raw := e.do(t, "GET", "/feedback", session(t, uidAlice), "").Body.String()
	for _, forbidden := range []string{"bob secret", "legacy ownerless note", "legacy-1"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("normal user's list contains %q", forbidden)
		}
	}
}

// Status/category filters narrow the caller's own set — they never widen it.
func TestFiltersCannotWidenAUsersScope(t *testing.T) {
	legacy := `[{"id":"legacy-1","createdAt":1700000000,"updatedAt":1700000000,` +
		`"category":"bug","rating":3,"message":"legacy ownerless note","status":"new"}]`
	e := newTestEnv(t, []string{uidAdmin}, legacy)
	e.submit(t, uidAlice, "alice bug")
	e.submit(t, uidBob, "bob bug")

	for _, q := range []string{"?status=new", "?category=bug", "?status=new&category=bug"} {
		items, _, _ := decodeList(t, e.do(t, "GET", "/feedback"+q, session(t, uidAlice), ""))
		if len(items) != 1 || items[0].Message != "alice bug" {
			t.Errorf("filter %q widened scope: %+v", q, items)
		}
	}
}

// A signed-in non-admin is refused the whole triage surface — and refused BEFORE any lookup, so the
// response cannot be used to discover which ids exist.
func TestNonAdminIsRefusedTheTriageSurface(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, "")
	own := e.submit(t, uidAlice, "alice own")
	foreign := e.submit(t, uidBob, "bob own")

	if w := e.do(t, "GET", "/feedback/summary", session(t, uidAlice), ""); w.Code != http.StatusForbidden {
		t.Errorf("non-admin summary: want 403, got %d", w.Code)
	}

	// Same 403 for a record that exists and is the caller's, one that belongs to someone else, and
	// one that does not exist at all. No existence oracle.
	for _, id := range []string{own, foreign, "does-not-exist-0000"} {
		if w := e.do(t, "PATCH", "/feedback/"+id, session(t, uidAlice), `{"status":"resolved"}`); w.Code != http.StatusForbidden {
			t.Errorf("non-admin PATCH %s: want 403, got %d", id, w.Code)
		}
		if w := e.do(t, "DELETE", "/feedback/"+id, session(t, uidAlice), ""); w.Code != http.StatusForbidden {
			t.Errorf("non-admin DELETE %s: want 403, got %d", id, w.Code)
		}
	}

	// Nothing was mutated or destroyed by any of it.
	if rec, ok := e.srv.store.Get(own); !ok || rec.Status != "new" {
		t.Error("a refused PATCH/DELETE still changed the caller's own record")
	}
	if _, ok := e.srv.store.Get(foreign); !ok {
		t.Error("a refused DELETE destroyed another user's record")
	}
}

// The configured admin gets the whole surface, including the legacy records nobody owns.
func TestConfiguredAdminHasFullAccess(t *testing.T) {
	legacy := `[{"id":"legacy-1","createdAt":1700000000,"updatedAt":1700000000,` +
		`"category":"ux","rating":3,"message":"legacy ownerless note","status":"new"}]`
	e := newTestEnv(t, []string{uidAdmin}, legacy)
	aliceID := e.submit(t, uidAlice, "alice one")
	bobID := e.submit(t, uidBob, "bob one")

	items, _, admin := decodeList(t, e.do(t, "GET", "/feedback", session(t, uidAdmin), ""))
	if !admin {
		t.Error("the allow-listed uid must be flagged admin")
	}
	if len(items) != 3 {
		t.Fatalf("admin must see all 3 records (2 owned + 1 legacy), got %d", len(items))
	}

	w := e.do(t, "GET", "/feedback/summary", session(t, uidAdmin), "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin summary: want 200, got %d", w.Code)
	}
	var sum struct {
		Total int `json:"total"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sum)
	if sum.Total != 3 {
		t.Errorf("summary total: want 3, got %d", sum.Total)
	}

	if w := e.do(t, "PATCH", "/feedback/"+aliceID, session(t, uidAdmin), `{"status":"reviewed"}`); w.Code != http.StatusOK {
		t.Errorf("admin PATCH: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if rec, _ := e.srv.store.Get(aliceID); rec.Status != "reviewed" {
		t.Errorf("admin PATCH did not apply: %q", rec.Status)
	}
	// Triage must not re-own the record it touches.
	if rec, _ := e.srv.store.Get(aliceID); rec.Owner != uidAlice {
		t.Errorf("admin triage reassigned ownership: %q", rec.Owner)
	}

	if w := e.do(t, "DELETE", "/feedback/"+bobID, session(t, uidAdmin), ""); w.Code != http.StatusOK {
		t.Errorf("admin DELETE: want 200, got %d", w.Code)
	}
	if _, ok := e.srv.store.Get(bobID); ok {
		t.Error("admin DELETE did not remove the record")
	}
	// A genuinely missing id is 404 for an admin — who is allowed to know.
	if w := e.do(t, "DELETE", "/feedback/nope", session(t, uidAdmin), ""); w.Code != http.StatusNotFound {
		t.Errorf("admin DELETE of a missing id: want 404, got %d", w.Code)
	}
}

// The default configuration — an empty allow-list — grants nobody admin, while leaving ordinary use
// working. Fail closed, not fail shut.
func TestEmptyAllowListGrantsNoAdmin(t *testing.T) {
	e := newTestEnv(t, nil, "")
	id := e.submit(t, uidAlice, "still submittable")

	for _, uid := range []string{uidAlice, uidAdmin} {
		c := session(t, uid)
		if _, _, admin := decodeList(t, e.do(t, "GET", "/feedback", c, "")); admin {
			t.Errorf("%s was flagged admin with an empty allow-list", uid)
		}
		if w := e.do(t, "GET", "/feedback/summary", c, ""); w.Code != http.StatusForbidden {
			t.Errorf("%s summary with an empty allow-list: want 403, got %d", uid, w.Code)
		}
		if w := e.do(t, "PATCH", "/feedback/"+id, c, `{"status":"reviewed"}`); w.Code != http.StatusForbidden {
			t.Errorf("%s PATCH with an empty allow-list: want 403, got %d", uid, w.Code)
		}
		if w := e.do(t, "DELETE", "/feedback/"+id, c, ""); w.Code != http.StatusForbidden {
			t.Errorf("%s DELETE with an empty allow-list: want 403, got %d", uid, w.Code)
		}
	}

	// Submitting and reading your own still work — an unconfigured deployment is not a broken one.
	if w := e.do(t, "POST", "/feedback", session(t, uidAlice), `{"category":"idea","message":"second"}`); w.Code != http.StatusCreated {
		t.Errorf("submission must still work with no admin configured: got %d", w.Code)
	}
	items, _, _ := decodeList(t, e.do(t, "GET", "/feedback", session(t, uidAlice), ""))
	if len(items) != 2 {
		t.Errorf("own reads must still work with no admin configured: got %d records", len(items))
	}
	if _, ok := e.srv.store.Get(id); !ok {
		t.Error("a refused DELETE destroyed the record")
	}
}

// The allow-list is exact: a near-miss uid is not an admin. (Blank and whitespace entries are dropped
// by splitCSV, so they can never match the "" of a guest.)
func TestAllowListMatchesExactly(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, "")
	for _, near := range []string{uidAdmin + "x", strings.ToUpper(uidAdmin), " " + uidAdmin, uidAdmin[:len(uidAdmin)-1]} {
		if e.srv.isAdmin(near) {
			t.Errorf("%q must not match the allow-list entry %q", near, uidAdmin)
		}
	}
	if e.srv.isAdmin("") {
		t.Error("a guest must never be an admin")
	}
	if !e.srv.isAdmin(uidAdmin) {
		t.Error("the exact allow-listed uid must be an admin")
	}
}

// splitCSV is what turns the env var into the allow-list: trimmed, blanks removed.
func TestAdminAllowListParsing(t *testing.T) {
	got := splitCSV("  uid-a , ,uid-b,,  uid-c  ")
	want := []string{"uid-a", "uid-b", "uid-c"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: want %q, got %q", i, want[i], got[i])
		}
	}
	if len(splitCSV("")) != 0 || len(splitCSV("  ,  , ")) != 0 {
		t.Error("an empty or blank-only value must yield no admins")
	}
}

// ───────────────────────────────── CORS ─────────────────────────────────

// An allow-listed origin is reflected with credentials; anything else gets no CORS headers at all,
// and the wildcard never appears (browsers reject "*" alongside credentials anyway).
func TestCORSIsCredentialSafe(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, "")

	allowedOrigin := "http://localhost:5173"
	w := e.do2(t, "GET", "/feedback", session(t, uidAlice), allowedOrigin)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != allowedOrigin {
		t.Errorf("allow-listed origin must be reflected: want %q, got %q", allowedOrigin, got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("credentials must be enabled, got %q", got)
	}
	if got := w.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary: Origin must be set for cache correctness, got %q", got)
	}

	for _, bad := range []string{"http://evil.example", "https://localhost:5173", "http://localhost:5174", "null"} {
		w := e.do2(t, "GET", "/feedback", session(t, uidAlice), bad)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin %q must not be reflected, got %q", bad, got)
		}
		if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("origin %q must not get credentials, got %q", bad, got)
		}
	}

	// No response, for any origin, may carry a wildcard.
	for _, origin := range []string{allowedOrigin, "http://evil.example", ""} {
		w := e.do2(t, "GET", "/health", "", origin)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got == "*" {
			t.Errorf("wildcard CORS returned for origin %q", origin)
		}
	}
}

// Preflight must succeed without a session — the browser sends no cookie on OPTIONS, and if the
// preflight 401s the real credentialed request is never attempted.
func TestPreflightNeedsNoSession(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, "")
	for _, path := range []string{"/feedback", "/feedback/summary", "/feedback/some-id"} {
		w := e.do2(t, "OPTIONS", path, "", "http://localhost:5173")
		if w.Code != http.StatusNoContent {
			t.Errorf("OPTIONS %s: want 204, got %d", path, w.Code)
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
			t.Errorf("OPTIONS %s: origin not reflected, got %q", path, got)
		}
		methods := w.Header().Get("Access-Control-Allow-Methods")
		for _, m := range []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"} {
			if !strings.Contains(methods, m) {
				t.Errorf("OPTIONS %s: %s missing from %q", path, m, methods)
			}
		}
		if got := w.Header().Get("Access-Control-Allow-Headers"); got != "Content-Type" {
			t.Errorf("OPTIONS %s: allowed headers must stay minimal, got %q", path, got)
		}
	}
}

// do2 issues a request carrying an Origin header.
func (e *testEnv) do2(t *testing.T, method, path, cookie, origin string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}
