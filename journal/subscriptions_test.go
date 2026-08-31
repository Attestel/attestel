package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// subscriptions_test.go — the follow graph's guarantees, exercised through the real HTTP surface.
//
// Everything here runs with NO network, NO model and NO API key. That is not incidental: a follow is
// a filesystem record in the caller's own directory, which is exactly what makes invariant #1 (the
// stack runs with zero keys and zero network) hold for this surface by construction.
//
// The properties under test are the ones whose failure would be silent and expensive: idempotency
// (a double-follow must not fork the graph), the closed enums (a stored surprise is worse than a
// 400), the deterministic order (the frontend does not sort this list again), per-user isolation,
// and D-06 erasure.

// ---- harness ----

// newFollowEnvAt builds a server over an EXISTING directory. `newTestEnv` (evidence_test.go) always
// mints a fresh temp dir; the erasure and restart tests need a second server over the same one, so
// they can observe what is actually on disk rather than what a warm cache remembers.
func newFollowEnvAt(t *testing.T, dir string) *testEnv {
	t.Helper()
	cfg := Config{TradesDir: dir, Secret: "test-secret", CookieName: "nvda_session"}
	store, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	theses, err := openThesisStore(dir)
	if err != nil {
		t.Fatalf("openThesisStore: %v", err)
	}
	srv := &Server{cfg: cfg, store: store, theses: theses, http: &http.Client{Timeout: time.Second}}
	return &testEnv{srv: srv, handler: srv.routes(), dir: dir}
}

// follow posts a subscription and returns the record plus the status code.
func follow(t *testing.T, e *testEnv, uid string, body map[string]any) (int, map[string]any) {
	t.Helper()
	code, out := e.do(t, uid, "POST", "/subscriptions", body)
	sub, _ := out["subscription"].(map[string]any)
	return code, sub
}

// subsOf reads the caller's follow graph in the order the server returned it.
func subsOf(t *testing.T, e *testEnv, uid string) []map[string]any {
	t.Helper()
	code, out := e.do(t, uid, "GET", "/subscriptions", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /subscriptions = %d, body %v", code, out)
	}
	raw, ok := out["subscriptions"].([]any)
	if !ok {
		t.Fatalf("GET /subscriptions: no subscriptions array in %v", out)
	}
	list := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		list = append(list, m)
	}
	return list
}

func tickerList(subs []map[string]any) []string {
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		t, _ := s["ticker"].(string)
		out = append(out, t)
	}
	return out
}

// ---- round trip ----

func TestSubscriptionRoundTripOffline(t *testing.T) {
	e := newTestEnv(t)

	code, sub := follow(t, e, "u1", map[string]any{"ticker": "nvda "})
	if code != http.StatusOK {
		t.Fatalf("POST /subscriptions = %d, want 200", code)
	}
	// Shape, byte for byte against contract §2: camelCase names, unix-second integers, the defaults.
	id, _ := sub["id"].(string)
	if !strings.HasPrefix(id, "sub_") || len(id) != len("sub_")+16 {
		t.Fatalf("id = %q, want sub_ + 16 hex", id)
	}
	if sub["ticker"] != "NVDA" {
		t.Fatalf("ticker = %v, want NVDA (uppercased and trimmed on write)", sub["ticker"])
	}
	if sub["priority"] != float64(0) {
		t.Fatalf("priority = %v, want 0", sub["priority"])
	}
	if sub["notificationsEnabled"] != true {
		t.Fatalf("notificationsEnabled = %v, want true", sub["notificationsEnabled"])
	}
	if sub["notificationLevel"] != "material" {
		t.Fatalf("notificationLevel = %v, want material", sub["notificationLevel"])
	}
	if sub["source"] != "manual" {
		t.Fatalf("source = %v, want manual", sub["source"])
	}
	createdAt, ok := sub["createdAt"].(float64)
	if !ok || createdAt <= 0 || createdAt != float64(int64(createdAt)) {
		t.Fatalf("createdAt = %v, want a positive unix-second integer", sub["createdAt"])
	}
	// Nothing in a subscription record implies a position or an action (invariant #2).
	for _, forbidden := range []string{"side", "size", "quantity", "entry", "stop", "target", "action", "signal"} {
		if _, present := sub[forbidden]; present {
			t.Fatalf("subscription carries %q — a follow is not a position", forbidden)
		}
	}

	if got := tickerList(subsOf(t, e, "u1")); len(got) != 1 || got[0] != "NVDA" {
		t.Fatalf("GET after create = %v, want [NVDA]", got)
	}

	code, out := e.do(t, "u1", "PATCH", "/subscriptions/"+id, map[string]any{
		"priority": 3, "notificationsEnabled": false, "notificationLevel": "earnings",
	})
	if code != http.StatusOK {
		t.Fatalf("PATCH = %d, body %v", code, out)
	}
	patched, _ := out["subscription"].(map[string]any)
	if patched["priority"] != float64(3) || patched["notificationsEnabled"] != false ||
		patched["notificationLevel"] != "earnings" {
		t.Fatalf("PATCH did not apply: %v", patched)
	}
	if patched["source"] != "manual" || patched["ticker"] != "NVDA" {
		t.Fatalf("PATCH changed a field it may not touch: %v", patched)
	}

	code, out = e.do(t, "u1", "DELETE", "/subscriptions/"+id, nil)
	if code != http.StatusOK || out["deleted"] != true {
		t.Fatalf("DELETE = %d, body %v, want 200 {\"deleted\": true}", code, out)
	}
	if got := subsOf(t, e, "u1"); len(got) != 0 {
		t.Fatalf("after DELETE the graph is %v, want empty", got)
	}
}

// TestSubscriptionPatchDistinguishesAbsentFromZero is the whole reason the patch struct uses
// pointers: `priority: 0` is a real value a user can choose, and an omitted field must not silently
// reset the other two.
func TestSubscriptionPatchDistinguishesAbsentFromZero(t *testing.T) {
	e := newTestEnv(t)
	_, sub := follow(t, e, "u1", map[string]any{"ticker": "AMD", "notificationLevel": "macro"})
	id := sub["id"].(string)

	if _, out := e.do(t, "u1", "PATCH", "/subscriptions/"+id, map[string]any{"priority": 7}); out == nil {
		t.Fatal("PATCH returned no body")
	}
	_, out := e.do(t, "u1", "PATCH", "/subscriptions/"+id, map[string]any{"priority": 0})
	got, _ := out["subscription"].(map[string]any)
	if got["priority"] != float64(0) {
		t.Fatalf("priority = %v after an explicit 0, want 0", got["priority"])
	}
	if got["notificationLevel"] != "macro" {
		t.Fatalf("notificationLevel = %v, want macro — an absent field is 'leave alone'", got["notificationLevel"])
	}
	if got["notificationsEnabled"] != true {
		t.Fatalf("notificationsEnabled = %v, want true — an absent field is not 'set to zero'", got["notificationsEnabled"])
	}
}

// ---- ordering ----

func TestSubscriptionOrderingIsPriorityThenTicker(t *testing.T) {
	e := newTestEnv(t)
	for _, tk := range []string{"tsla", "NVDA", "AMD", "GOOGL"} {
		if code, _ := follow(t, e, "u1", map[string]any{"ticker": tk}); code != http.StatusOK {
			t.Fatalf("follow %s = %d", tk, code)
		}
	}
	// All at priority 0 -> alphabetical.
	if got := tickerList(subsOf(t, e, "u1")); strings.Join(got, ",") != "AMD,GOOGL,NVDA,TSLA" {
		t.Fatalf("default order = %v, want AMD,GOOGL,NVDA,TSLA", got)
	}
	// Give TSLA a lower priority and it sorts first, regardless of its name.
	for _, s := range subsOf(t, e, "u1") {
		if s["ticker"] == "TSLA" {
			e.do(t, "u1", "PATCH", "/subscriptions/"+s["id"].(string), map[string]any{"priority": -1})
		}
	}
	if got := tickerList(subsOf(t, e, "u1")); strings.Join(got, ",") != "TSLA,AMD,GOOGL,NVDA" {
		t.Fatalf("order after priority change = %v, want TSLA,AMD,GOOGL,NVDA", got)
	}
}

// ---- idempotency ----

func TestFollowingAnAlreadyFollowedTickerIsIdempotent(t *testing.T) {
	e := newTestEnv(t)
	code1, first := follow(t, e, "u1", map[string]any{"ticker": "NVDA"})
	code2, second := follow(t, e, "u1", map[string]any{"ticker": "nvda"})
	code3, third := follow(t, e, "u1", map[string]any{"ticker": "  NvDa  ", "notificationLevel": "all"})

	for i, c := range []int{code1, code2, code3} {
		if c != http.StatusOK {
			t.Fatalf("POST #%d = %d, want 200 every time (idempotent, never 409)", i+1, c)
		}
	}
	if first["id"] != second["id"] || second["id"] != third["id"] {
		t.Fatalf("ids differ across idempotent follows: %v %v %v", first["id"], second["id"], third["id"])
	}
	// The stored record wins: a re-follow returns what is stored and does not quietly re-configure it.
	if third["notificationLevel"] != "material" {
		t.Fatalf("re-follow changed notificationLevel to %v; it must return the stored record", third["notificationLevel"])
	}
	if got := subsOf(t, e, "u1"); len(got) != 1 {
		t.Fatalf("three follows of one company produced %d records, want 1", len(got))
	}
}

// TestNoSubscriptionCap: free-tier pressure is a provider-budget problem (AD-10), not a reason to
// limit what a user may follow. Inventing a cap here would be a contract amendment.
func TestNoSubscriptionCap(t *testing.T) {
	e := newTestEnv(t)
	for i := 0; i < 60; i++ {
		tk := "T" + string(rune('A'+i/26)) + string(rune('A'+i%26))
		if code, _ := follow(t, e, "u1", map[string]any{"ticker": tk}); code != http.StatusOK {
			t.Fatalf("follow #%d (%s) = %d, want 200 — there is no cap", i+1, tk, code)
		}
	}
	if got := subsOf(t, e, "u1"); len(got) != 60 {
		t.Fatalf("followed 60 companies, graph has %d", len(got))
	}
}

// TestFollowingATickerOutsideTICKERS: the gateway's TICKERS env gates the NVDA-only seeded surfaces.
// It is not a constraint on the follow graph.
func TestFollowingATickerOutsideTICKERS(t *testing.T) {
	e := newTestEnv(t)
	if code, sub := follow(t, e, "u1", map[string]any{"ticker": "BRK.B"}); code != http.StatusOK || sub["ticker"] != "BRK.B" {
		t.Fatalf("follow BRK.B = %d %v, want 200 and the record", code, sub)
	}
}

// ---- validation ----

func TestSubscriptionValidation(t *testing.T) {
	e := newTestEnv(t)
	_, existing := follow(t, e, "u1", map[string]any{"ticker": "NVDA"})
	id := existing["id"].(string)

	cases := []struct {
		name   string
		method string
		path   string
		body   map[string]any
		want   int
	}{
		{"empty ticker", "POST", "/subscriptions", map[string]any{"ticker": ""}, http.StatusBadRequest},
		{"blank ticker", "POST", "/subscriptions", map[string]any{"ticker": "   "}, http.StatusBadRequest},
		{"missing ticker", "POST", "/subscriptions", map[string]any{}, http.StatusBadRequest},
		{"malformed ticker", "POST", "/subscriptions", map[string]any{"ticker": "NV DA"}, http.StatusBadRequest},
		{"overlong ticker", "POST", "/subscriptions", map[string]any{"ticker": "ABCDEFGHIJKLMNOP"}, http.StatusBadRequest},
		{"bad notificationLevel", "POST", "/subscriptions",
			map[string]any{"ticker": "AMD", "notificationLevel": "urgent"}, http.StatusBadRequest},
		{"bad source", "POST", "/subscriptions",
			map[string]any{"ticker": "AMD", "source": "imported"}, http.StatusBadRequest},
		{"good source explore", "POST", "/subscriptions",
			map[string]any{"ticker": "AMD", "source": "explore"}, http.StatusOK},
		{"good level none", "POST", "/subscriptions",
			map[string]any{"ticker": "AVGO", "notificationLevel": "none"}, http.StatusOK},
		{"patch bad level", "PATCH", "/subscriptions/" + id,
			map[string]any{"notificationLevel": "urgent"}, http.StatusBadRequest},
		{"patch nothing", "PATCH", "/subscriptions/" + id, map[string]any{}, http.StatusBadRequest},
		{"patch unknown id", "PATCH", "/subscriptions/sub_0000000000000abc",
			map[string]any{"priority": 1}, http.StatusNotFound},
		{"delete unknown id", "DELETE", "/subscriptions/sub_0000000000000abc", nil, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := e.do(t, "u1", tc.method, tc.path, tc.body)
			if code != tc.want {
				t.Fatalf("%s %s = %d, want %d (body %v)", tc.method, tc.path, code, tc.want, body)
			}
		})
	}
}

// ---- guests ----

func TestSubscriptionEndpointsRejectGuests(t *testing.T) {
	e := newTestEnv(t)
	_, sub := follow(t, e, "u1", map[string]any{"ticker": "NVDA"})
	id := sub["id"].(string)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/subscriptions"},
		{"POST", "/subscriptions"},
		{"POST", "/subscriptions/migrate"},
		{"PATCH", "/subscriptions/" + id},
		{"DELETE", "/subscriptions/" + id},
	} {
		code, _ := e.do(t, "", tc.method, tc.path, map[string]any{"ticker": "NVDA"})
		if code != http.StatusUnauthorized {
			t.Fatalf("%s %s as a guest = %d, want 401", tc.method, tc.path, code)
		}
	}
}

// ---- isolation ----

func TestSubscriptionUserIsolation(t *testing.T) {
	e := newTestEnv(t)
	_, mine := follow(t, e, "u1", map[string]any{"ticker": "NVDA"})
	follow(t, e, "u2", map[string]any{"ticker": "AMD"})

	if got := tickerList(subsOf(t, e, "u2")); len(got) != 1 || got[0] != "AMD" {
		t.Fatalf("u2 sees %v, want [AMD]", got)
	}
	// A foreign id is 404, never 403: existence must not be confirmable across users.
	if code, _ := e.do(t, "u2", "PATCH", "/subscriptions/"+mine["id"].(string), map[string]any{"priority": 1}); code != http.StatusNotFound {
		t.Fatalf("u2 patching u1's subscription = %d, want 404", code)
	}
	if code, _ := e.do(t, "u2", "DELETE", "/subscriptions/"+mine["id"].(string), nil); code != http.StatusNotFound {
		t.Fatalf("u2 deleting u1's subscription = %d, want 404", code)
	}
	if got := tickerList(subsOf(t, e, "u1")); len(got) != 1 || got[0] != "NVDA" {
		t.Fatalf("u1's graph was touched by u2: %v", got)
	}
}

// ---- migration marker (§2.2, resolved by §9.39) ----

// migrateAs calls the §9.39 endpoint and returns the status and body.
func migrateAs(t *testing.T, e *testEnv, uid string, body any) (int, map[string]any) {
	t.Helper()
	return e.do(t, uid, "POST", "/subscriptions/migrate", body)
}

// TestMigrationMarkerIsTheFileItself proves the marker exists, is the FILE, and cannot fire twice.
//
// A flag file would be a second erasure risk and `_`-prefixed names are reserved, so the file's own
// existence is the marker. §9.39 is what makes that marker reachable: the gateway cannot see it
// through §2.1's response shape — an empty array means both "never migrated" and "unfollowed
// everything" — so journal is asked the question directly and answers it.
func TestMigrationMarkerIsTheFileItself(t *testing.T) {
	e := newTestEnv(t)
	path := filepath.Join(e.dir, "u1", subscriptionsFile)

	if _, err := os.Stat(path); err == nil {
		t.Fatal("subscriptions.json exists before anything was written")
	}

	// A READ does not lay the marker down. If it did, a GET arriving before the migrate call would
	// permanently answer "already migrated" and the seed could never run.
	if got := subsOf(t, e, "u1"); len(got) != 0 {
		t.Fatalf("first GET = %v, want an empty graph", got)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("GET /subscriptions created subscriptions.json — the file is created on first WRITE (§9.39.2)")
	}

	code, out := migrateAs(t, e, "u1", map[string]any{"activeTicker": "NVDA"})
	if code != http.StatusOK || out["migrated"] != true {
		t.Fatalf("first migrate = %d %v, want 200 {\"migrated\": true}", code, out)
	}
	sub, _ := out["subscription"].(map[string]any)
	if sub["ticker"] != "NVDA" || sub["source"] != "migrated" || sub["priority"] != float64(0) {
		t.Fatalf("seeded subscription = %v, want NVDA / source migrated / priority 0", sub)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("migrate did not write the marker: %v", err)
	}
	var stored []Subscription
	if err := json.Unmarshal(b, &stored); err != nil || len(stored) != 1 {
		t.Fatalf("marker file = %q, want one stored subscription", string(b))
	}

	// A second call sees the file the first one wrote. Idempotent, so the gateway may call it once
	// per session without tracking whether it already has.
	code2, out2 := migrateAs(t, e, "u1", map[string]any{"activeTicker": "NVDA"})
	if code2 != http.StatusOK || out2["migrated"] != false {
		t.Fatalf("second migrate = %d %v, want 200 {\"migrated\": false}", code2, out2)
	}
	if _, ok := out2["subscription"]; ok {
		t.Fatalf("a no-op migrate returned a subscription: %v", out2)
	}
	if got := subsOf(t, e, "u1"); len(got) != 1 {
		t.Fatalf("graph has %d records after two migrate calls, want 1", len(got))
	}

	// The marker survives a restart, so a fresh process does not treat this user as un-migrated.
	fresh := newFollowEnvAt(t, e.dir)
	if got := tickerList(subsOf(t, fresh, "u1")); len(got) != 1 || got[0] != "NVDA" {
		t.Fatalf("after restart u1's graph = %v, want [NVDA]", got)
	}
	if _, out3 := migrateAs(t, fresh, "u1", map[string]any{"activeTicker": "NVDA"}); out3["migrated"] != false {
		t.Fatalf("migrate after restart = %v, want a no-op", out3)
	}
}

// TestMigrateIsANoOpAgainstAnExistingEmptyFile is the whole point of §9.39, in one test.
//
// "Migrated and follows nothing" and "never migrated" are both an empty array through §2.1. Only the
// file tells them apart, and only journal can see the file. A gateway that seeded on empty would
// hand this user back, on every single page load, the ticker they just removed.
func TestMigrateIsANoOpAgainstAnExistingEmptyFile(t *testing.T) {
	e := newTestEnv(t)
	if err := os.MkdirAll(filepath.Join(e.dir, "u1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(e.dir, "u1", subscriptionsFile)
	if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, out := migrateAs(t, e, "u1", map[string]any{"activeTicker": "NVDA"})
	if code != http.StatusOK || out["migrated"] != false {
		t.Fatalf("migrate against an existing empty file = %d %v, want 200 {\"migrated\": false}", code, out)
	}
	if got := subsOf(t, e, "u1"); len(got) != 0 {
		t.Fatalf("migrate seeded over an existing empty graph: %v", got)
	}
	if b, err := os.ReadFile(path); err != nil || strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("the existing file was rewritten: %q (%v)", string(b), err)
	}
}

// TestSubscriptionsFileSurvivesDeletingTheLastSubscription is the other half of §9.39.2. Unlinking
// the file when the graph empties would resurrect the bug the endpoint exists to fix: the next
// migrate call would see no marker and seed the ticker the user had just unfollowed.
func TestSubscriptionsFileSurvivesDeletingTheLastSubscription(t *testing.T) {
	e := newTestEnv(t)
	path := filepath.Join(e.dir, "u1", subscriptionsFile)

	if _, out := migrateAs(t, e, "u1", map[string]any{"activeTicker": "NVDA"}); out["migrated"] != true {
		t.Fatalf("migrate = %v, want a seed", out)
	}
	subs := subsOf(t, e, "u1")
	if len(subs) != 1 {
		t.Fatalf("graph = %v, want one record", subs)
	}

	if code, out := e.do(t, "u1", "DELETE", "/subscriptions/"+subs[0]["id"].(string), nil); code != http.StatusOK || out["deleted"] != true {
		t.Fatalf("DELETE = %d %v", code, out)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("removing the last subscription deleted subscriptions.json: %v", err)
	}
	var empty []Subscription
	if err := json.Unmarshal(b, &empty); err != nil || len(empty) != 0 {
		t.Fatalf("file after removing the last subscription = %q, want an empty JSON array", string(b))
	}

	// And the marker still holds across a restart: the unfollowed ticker does not come back.
	fresh := newFollowEnvAt(t, e.dir)
	if _, out := migrateAs(t, fresh, "u1", map[string]any{"activeTicker": "NVDA"}); out["migrated"] != false {
		t.Fatalf("migrate after unfollowing everything = %v, want a no-op — the removed ticker was re-seeded", out)
	}
	if got := subsOf(t, fresh, "u1"); len(got) != 0 {
		t.Fatalf("graph after restart = %v, want empty", got)
	}
}

// TestMigrateWithNoActiveTicker: a user with nothing to carry over is still migrated. The marker is
// laid down so the question is asked once, and the answer is `migrated: false` because nothing was
// seeded — the two are different facts and the response reports the second one.
func TestMigrateWithNoActiveTicker(t *testing.T) {
	e := newTestEnv(t)
	path := filepath.Join(e.dir, "u1", subscriptionsFile)

	for _, body := range []any{map[string]any{}, map[string]any{"activeTicker": ""}, nil} {
		code, out := migrateAs(t, e, "u1", body)
		if code != http.StatusOK || out["migrated"] != false {
			t.Fatalf("migrate with no activeTicker (%v) = %d %v, want 200 {\"migrated\": false}", body, code, out)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("migrate with no activeTicker did not lay down the marker: %v", err)
	}
	if strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("marker file = %q, want an empty JSON array", string(b))
	}
}

// TestMigrateRejectsAMalformedTicker: the gateway supplies the value, but journal still validates it.
// A malformed symbol is a 400, never a stored surprise — the same rule POST /subscriptions applies.
func TestMigrateRejectsAMalformedTicker(t *testing.T) {
	e := newTestEnv(t)
	code, _ := migrateAs(t, e, "u1", map[string]any{"activeTicker": "NOT A TICKER"})
	if code != http.StatusBadRequest {
		t.Fatalf("migrate with a malformed ticker = %d, want 400", code)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "u1", subscriptionsFile)); err == nil {
		t.Fatal("a rejected migrate still wrote the marker")
	}
}

// TestMigrateDoesNotClobberAnUnreadableFile: a file that exists but does not parse belongs to a
// migrated user. Seeding over it would be exactly the clobber the 503 guard exists to prevent.
func TestMigrateDoesNotClobberAnUnreadableFile(t *testing.T) {
	e := newTestEnv(t)
	if err := os.MkdirAll(filepath.Join(e.dir, "u1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	corrupt := []byte("{ this is not the follow graph you are looking for")
	path := filepath.Join(e.dir, "u1", subscriptionsFile)
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, out := migrateAs(t, e, "u1", map[string]any{"activeTicker": "NVDA"})
	if code != http.StatusOK || out["migrated"] != false {
		t.Fatalf("migrate against an unreadable file = %d %v, want a no-op", code, out)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(corrupt) {
		t.Fatalf("the unreadable file was modified: %q", string(got))
	}
}

// stripLineComments removes `//` comments so a source-level assertion is about CODE. The prose in
// this lane's files legitimately names `activeTicker` while explaining why journal never touches it;
// scanning the comments too would make the doc comment fail its own test.
func stripLineComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestJournalCannotTouchActiveTicker is a SOURCE-LEVEL assertion, and deliberately so.
//
// §9.5 put the activeTicker migration in the gateway because journal has no auth client; §9.39 moved
// the DECISION back here without moving the client. `activeTicker` therefore now appears in this
// lane's source, as one field of a request body the gateway fills in — and that is the whole point:
// journal is TOLD the value and decides, it never goes and reads it, and it never writes it back.
//
// So `activeTicker` leaves the forbidden list and everything that would constitute an auth client
// stays on it. Journal gains no `AUTH_URL` and makes no outbound call, which is the only way
// "activeTicker is provably unchanged in auth" can be proved from inside this service. A behavioural
// test cannot prove the absence of a client; reading the source can.
func TestJournalCannotTouchActiveTicker(t *testing.T) {
	for _, file := range []string{"subscriptions.go", "event_state.go"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		src := stripLineComments(string(b))
		for _, forbidden := range []string{"AUTH_URL", "http.Get", "http.Post", "srv.http", "s.http"} {
			if strings.Contains(src, forbidden) {
				t.Fatalf("%s mentions %q — this lane makes no outbound call and never reads or writes auth settings", file, forbidden)
			}
		}
		// The one legitimate mention is the inbound request body. It must not become a write path:
		// nothing here may PUT, PATCH or POST the setting back to auth.
		for _, forbidden := range []string{"settings", "PUT", "activeTicker =", "ActiveTicker ="} {
			if strings.Contains(src, forbidden) {
				t.Fatalf("%s mentions %q — journal reads the supplied activeTicker and nothing more", file, forbidden)
			}
		}
	}
	// The config the journal loads has no auth-service URL at all.
	if strings.Contains(strings.Join(os.Environ(), " "), "AUTH_URL=") {
		t.Skip("AUTH_URL is set in the environment; the assertion below is about journal's Config, not the env")
	}
	cfg := loadConfig()
	if cfg.AnalysisURL == "" || cfg.GatewayURL == "" {
		t.Fatalf("unexpected config shape: %+v", cfg)
	}
}

// ---- concurrency and atomicity ----

func TestConcurrentFollowsProduceExactlyOneRecord(t *testing.T) {
	e := newTestEnv(t)
	_, seed := follow(t, e, "u1", map[string]any{"ticker": "NVDA"})
	id := seed["id"].(string)

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/subscriptions", strings.NewReader(`{"ticker":"nvda"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Cookie", e.cookieFor("u1"))
			e.handler.ServeHTTP(httptest.NewRecorder(), req)

			preq := httptest.NewRequest("PATCH", "/subscriptions/"+id, strings.NewReader(`{"priority":1}`))
			preq.Header.Set("Content-Type", "application/json")
			preq.Header.Set("Cookie", e.cookieFor("u1"))
			e.handler.ServeHTTP(httptest.NewRecorder(), preq)
		}(i)
	}
	wg.Wait()

	got := subsOf(t, e, "u1")
	if len(got) != 1 {
		t.Fatalf("40 concurrent follows + patches produced %d records, want 1", len(got))
	}
	if got[0]["id"] != id {
		t.Fatalf("record id changed under concurrency: %v", got[0]["id"])
	}
	// The file on disk still parses — no torn write.
	b, err := os.ReadFile(filepath.Join(e.dir, "u1", subscriptionsFile))
	if err != nil {
		t.Fatalf("read subscriptions.json: %v", err)
	}
	var parsed []Subscription
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("subscriptions.json does not parse after concurrent writes: %v\n%s", err, string(b))
	}
	if len(parsed) != 1 || parsed[0].Priority != 1 {
		t.Fatalf("on-disk state = %+v, want one record at priority 1", parsed)
	}
}

// TestNoTempFileSurvivesASuccessfulWrite: the atomic write is `.tmp` + rename, and a leftover `.tmp`
// means the rename did not happen — the next restart would read a stale file.
func TestNoTempFileSurvivesASuccessfulWrite(t *testing.T) {
	e := newTestEnv(t)
	follow(t, e, "u1", map[string]any{"ticker": "NVDA"})
	follow(t, e, "u1", map[string]any{"ticker": "AMD"})
	e.do(t, "u1", "POST", "/event-state", map[string]any{"eventId": "evt_1a2b3c4d5e6f7081", "read": true})
	subsOf(t, e, "u1")

	entries, err := os.ReadDir(filepath.Join(e.dir, "u1"))
	if err != nil {
		t.Fatalf("read user dir: %v", err)
	}
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".tmp") {
			t.Fatalf("%s survived a successful write", en.Name())
		}
	}
}

// ---- D-06 ----

// TestErasureRemovesEverythingThisUserOwns is D-06's whole story, asserted on the FILESYSTEM rather
// than on the store's memory: `rm -rf {TRADES_DIR}/{uid}/` must leave nothing of this user anywhere
// under TRADES_DIR, and a fresh process must see an empty follow graph and empty event state.
func TestErasureRemovesEverythingThisUserOwns(t *testing.T) {
	e := newTestEnv(t)
	for _, tk := range []string{"NVDA", "AMD", "TSLA"} {
		follow(t, e, "u1", map[string]any{"ticker": tk})
	}
	for _, id := range []string{"evt_1a2b3c4d5e6f7081", "evt_bb11cc22dd33ee44"} {
		if code, out := e.do(t, "u1", "POST", "/event-state", map[string]any{"eventId": id, "saved": true}); code != http.StatusOK {
			t.Fatalf("POST /event-state = %d, body %v", code, out)
		}
	}
	// A second user, whose data must survive the erasure untouched.
	follow(t, e, "u2", map[string]any{"ticker": "GOOGL"})

	userDir := filepath.Join(e.dir, "u1")
	for _, f := range []string{subscriptionsFile, eventStateFile} {
		if _, err := os.Stat(filepath.Join(userDir, f)); err != nil {
			t.Fatalf("%s was never written: %v", f, err)
		}
	}

	if err := os.RemoveAll(userDir); err != nil {
		t.Fatalf("erase: %v", err)
	}

	// Nothing belonging to u1 exists anywhere else under TRADES_DIR — no global index, no cross-user
	// cache, no sidecar at the root. This is what makes erasure one operation.
	err := filepath.Walk(e.dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(e.dir, path)
		if strings.HasPrefix(rel, "u1"+string(filepath.Separator)) {
			t.Fatalf("%s survived the erasure", rel)
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, needle := range []string{"NVDA", "TSLA", "evt_1a2b3c4d5e6f7081", "evt_bb11cc22dd33ee44"} {
			if strings.Contains(string(b), needle) {
				t.Fatalf("%s still contains %q after u1 was erased", rel, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// A fresh process sees an empty follow graph and empty event state for u1 — and u2 intact.
	fresh := newFollowEnvAt(t, e.dir)
	if got := subsOf(t, fresh, "u1"); len(got) != 0 {
		t.Fatalf("erased user's follow graph = %v, want empty", got)
	}
	code, out := fresh.do(t, "u1", "GET", "/event-state?ids=evt_1a2b3c4d5e6f7081,evt_bb11cc22dd33ee44", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /event-state = %d", code)
	}
	if states, _ := out["states"].([]any); len(states) != 0 {
		t.Fatalf("erased user's event state = %v, want empty", states)
	}
	if got := tickerList(subsOf(t, fresh, "u2")); len(got) != 1 || got[0] != "GOOGL" {
		t.Fatalf("u2's graph = %v after u1 was erased, want [GOOGL]", got)
	}
}

// ---- §9.4 internal union ----

func TestInternalTickersIsTheAggregateSetOnly(t *testing.T) {
	e := newTestEnv(t)
	follow(t, e, "u1", map[string]any{"ticker": "NVDA"})
	follow(t, e, "u1", map[string]any{"ticker": "AMD"})
	follow(t, e, "u2", map[string]any{"ticker": "nvda"})
	follow(t, e, "u2", map[string]any{"ticker": "TSLA"})

	req := httptest.NewRequest("GET", "/_internal/tickers", nil)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /_internal/tickers = %d, want 200", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON: %s", rec.Body.String())
	}
	if len(out) != 1 {
		t.Fatalf("response has %d keys (%v); it must carry the ticker set and nothing else", len(out), out)
	}
	raw, ok := out["tickers"].([]any)
	if !ok {
		t.Fatalf("no tickers array in %v", out)
	}
	got := make([]string, 0, len(raw))
	for _, r := range raw {
		got = append(got, r.(string))
	}
	if strings.Join(got, ",") != "AMD,NVDA,TSLA" {
		t.Fatalf("tickers = %v, want the sorted union AMD,NVDA,TSLA (deduplicated across users)", got)
	}
	// No user ids and no counts, anywhere in the body.
	body := rec.Body.String()
	for _, forbidden := range []string{"u1", "u2", "count", "users", "followers"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("/_internal/tickers leaked %q: %s", forbidden, body)
		}
	}
}

// TestInternalTickersIsRefusedWithASessionCookie: it is an internal route, not a per-user route
// wearing an internal path. Any session cookie — valid or not — is refused.
func TestInternalTickersIsRefusedWithASessionCookie(t *testing.T) {
	e := newTestEnv(t)
	follow(t, e, "u1", map[string]any{"ticker": "NVDA"})

	for _, cookie := range []string{e.cookieFor("u1"), "nvda_session=garbage"} {
		req := httptest.NewRequest("GET", "/_internal/tickers", nil)
		req.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
			t.Fatalf("GET /_internal/tickers with a session cookie = %d, want 404 or 403", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "NVDA") {
			t.Fatalf("refused request still returned the ticker set: %s", rec.Body.String())
		}
	}
}

// TestInternalTickersSkipsReservedPartitions: `_`-prefixed buckets (`_legacy`, `_personas`,
// `_analytics`) are reserved and are not users.
func TestInternalTickersSkipsReservedPartitions(t *testing.T) {
	e := newTestEnv(t)
	follow(t, e, "u1", map[string]any{"ticker": "NVDA"})
	if err := os.MkdirAll(filepath.Join(e.dir, "_legacy"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "_legacy", subscriptionsFile),
		[]byte(`[{"id":"sub_00000000000000ab","ticker":"GHOST"}]`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	req := httptest.NewRequest("GET", "/_internal/tickers", nil)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "GHOST") {
		t.Fatalf("/_internal/tickers read a reserved partition: %s", rec.Body.String())
	}
}

// ---- refuse to clobber ----

// TestMalformedFollowGraphIsNeverOverwritten mirrors the evidence store's guarantee: a file that
// exists but does not parse is preserved, not replaced with an empty array.
func TestMalformedFollowGraphIsNeverOverwritten(t *testing.T) {
	e := newTestEnv(t)
	if err := os.MkdirAll(filepath.Join(e.dir, "u1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	corrupt := []byte("{ this is not the follow graph you are looking for")
	path := filepath.Join(e.dir, "u1", subscriptionsFile)
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if code, _ := e.do(t, "u1", "GET", "/subscriptions", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("GET on an unreadable store = %d, want 503", code)
	}
	if code, _ := follow(t, e, "u1", map[string]any{"ticker": "NVDA"}); code != http.StatusServiceUnavailable {
		t.Fatalf("POST on an unreadable store = %d, want 503", code)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(corrupt) {
		t.Fatalf("the unreadable file was modified: %q", string(got))
	}
}
