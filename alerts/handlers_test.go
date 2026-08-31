package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// handlers_test.go — the API surface: rule ownership, the D-10 precondition being server-derived,
// the due-review endpoint, and the additive event filters.

func testAPI(t *testing.T) *API {
	t.Helper()
	store, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	return newAPI(store, Config{
		Secret: "test-secret", CookieName: "nvda_session",
		CORSOrigins: []string{"http://localhost:5173"},
	})
}

// sessionFor mints a cookie the service's own verifier accepts, so the tests exercise the real auth
// path rather than a bypass.
func sessionFor(t *testing.T, cfg Config, uid string) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(sessionPayload{UID: uid, IAT: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := b64.EncodeToString(payload)
	return &http.Cookie{Name: cfg.CookieName, Value: body + "." + signPayload(cfg.Secret, body)}
}

func do(t *testing.T, a *API, method, path, body string, uid string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if uid != "" {
		r.AddCookie(sessionFor(t, a.cfg, uid))
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return out
}

// ---------------------------------------------------------------- ownership

// TestRuleOwnershipIsEnforced — a rule belongs to the session that created it. Another user's rule
// is 404, never 403: confirming existence across users is itself a leak.
func TestRuleOwnershipIsEnforced(t *testing.T) {
	a := testAPI(t)

	w := do(t, a, "POST", "/rules", `{"ticker":"NVDA","type":"price_cross",
		"params":{"level":130,"direction":"above"},"thesisId":"`+testThesisID+`"}`, "alice")
	if w.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%s)", w.Code, w.Body.String())
	}
	rule, _ := decode(t, w)["rule"].(map[string]any)
	id, _ := rule["id"].(string)

	t.Run("bob cannot see alice's rule", func(t *testing.T) {
		got := decode(t, do(t, a, "GET", "/rules", "", "bob"))
		if rules, _ := got["rules"].([]any); len(rules) != 0 {
			t.Fatalf("bob must see no rules, got %v", rules)
		}
	})
	t.Run("bob cannot patch it", func(t *testing.T) {
		if w := do(t, a, "PATCH", "/rules/"+id, `{"active":false}`, "bob"); w.Code != http.StatusNotFound {
			t.Errorf("want 404, got %d", w.Code)
		}
	})
	t.Run("bob cannot delete it", func(t *testing.T) {
		if w := do(t, a, "DELETE", "/rules/"+id, "", "bob"); w.Code != http.StatusNotFound {
			t.Errorf("want 404, got %d", w.Code)
		}
	})
	t.Run("a guest cannot mutate at all", func(t *testing.T) {
		if w := do(t, a, "POST", "/rules", `{"ticker":"NVDA","type":"trend_flip"}`, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("want 401, got %d", w.Code)
		}
	})
	t.Run("alice still owns it", func(t *testing.T) {
		got := decode(t, do(t, a, "GET", "/rules", "", "alice"))
		if rules, _ := got["rules"].([]any); len(rules) != 1 {
			t.Fatalf("alice must see her one rule, got %v", rules)
		}
	})
}

// TestOwnerIsServerAssigned — a client cannot claim another user's id by sending one.
func TestOwnerIsServerAssigned(t *testing.T) {
	a := testAPI(t)
	do(t, a, "POST", "/rules", `{"ticker":"NVDA","type":"trend_flip","userId":"bob"}`, "alice")

	if got := decode(t, do(t, a, "GET", "/rules", "", "bob")); len(got["rules"].([]any)) != 0 {
		t.Error("a client-supplied userId must be ignored")
	}
	if got := decode(t, do(t, a, "GET", "/rules", "", "alice")); len(got["rules"].([]any)) != 1 {
		t.Error("the rule must belong to the session that created it")
	}
}

// ---------------------------------------------------------------- D-10 precondition

// TestAutoLinkFlagIsServerDerived — a client cannot set userCreatedFromItem directly. It is true iff
// the caller's own body named the item, which is the assertion D-10 requires.
func TestAutoLinkFlagIsServerDerived(t *testing.T) {
	a := testAPI(t)

	t.Run("claiming the flag without naming an item does not grant it", func(t *testing.T) {
		w := do(t, a, "POST", "/rules", `{"ticker":"NVDA","type":"price_cross",
			"params":{"level":130,"direction":"above"},"thesisId":"`+testThesisID+`",
			"userCreatedFromItem":true}`, "alice")
		rule, _ := decode(t, w)["rule"].(map[string]any)
		if got, _ := rule["userCreatedFromItem"].(bool); got {
			t.Error("the flag must be derived from the body naming an item, not accepted from the wire")
		}
	})

	t.Run("naming an invalidation condition grants it", func(t *testing.T) {
		w := do(t, a, "POST", "/rules", `{"ticker":"NVDA","type":"price_cross",
			"params":{"level":130,"direction":"below"},"thesisId":"`+testThesisID+`",
			"thesisItemId":"inv_1111111111111111","intent":"invalidation"}`, "alice")
		if w.Code != http.StatusCreated {
			t.Fatalf("want 201, got %d (%s)", w.Code, w.Body.String())
		}
		rule, _ := decode(t, w)["rule"].(map[string]any)
		if got, _ := rule["userCreatedFromItem"].(bool); !got {
			t.Error("naming the condition IS the user asserting the causal link")
		}
	})
}

func TestClearingTheItemRevokesTheAutoLinkPermission(t *testing.T) {
	a := testAPI(t)
	w := do(t, a, "POST", "/rules", `{"ticker":"NVDA","type":"price_cross",
		"params":{"level":130,"direction":"below"},"thesisId":"`+testThesisID+`",
		"thesisItemId":"inv_1111111111111111","intent":"invalidation"}`, "alice")
	rule, _ := decode(t, w)["rule"].(map[string]any)
	id, _ := rule["id"].(string)

	w = do(t, a, "PATCH", "/rules/"+id, `{"thesisItemId":"","intent":""}`, "alice")
	if w.Code != http.StatusOK {
		t.Fatalf("patch: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	updated, _ := decode(t, w)["rule"].(map[string]any)
	if got, _ := updated["userCreatedFromItem"].(bool); got {
		t.Error("clearing the item must clear the assertion — that is how a user revokes auto-linking")
	}
}

// TestInvalidLinkageIsRefusedBeforePersisting — a bad edit must not corrupt the stored rule.
func TestInvalidLinkageIsRefusedBeforePersisting(t *testing.T) {
	a := testAPI(t)
	w := do(t, a, "POST", "/rules", `{"ticker":"NVDA","type":"price_cross",
		"params":{"level":130,"direction":"above"},"thesisId":"`+testThesisID+`"}`, "alice")
	rule, _ := decode(t, w)["rule"].(map[string]any)
	id, _ := rule["id"].(string)

	// intent=catalyst with an inv_ item is the mismatch §4.1 forbids.
	if w := do(t, a, "PATCH", "/rules/"+id,
		`{"intent":"catalyst","thesisItemId":"inv_1111111111111111"}`, "alice"); w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%s)", w.Code, w.Body.String())
	}
	got := decode(t, do(t, a, "GET", "/rules", "", "alice"))
	stored := got["rules"].([]any)[0].(map[string]any)
	if stored["intent"] != "" || stored["thesisItemId"] != "" {
		t.Errorf("a refused patch must leave the rule untouched, got %v", stored)
	}
}

// ---------------------------------------------------------------- due reviews

func TestDueReviewsAreScopedAndOrdered(t *testing.T) {
	a := testAPI(t)

	mk := func(uid string, ageDays int64) string {
		w := do(t, a, "POST", "/rules", `{"ticker":"NVDA","type":"thesis_review",
			"thesisId":"`+testThesisID+`","params":{"everyDays":30}}`, uid)
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d (%s)", w.Code, w.Body.String())
		}
		rule, _ := decode(t, w)["rule"].(map[string]any)
		id, _ := rule["id"].(string)
		seedRuleCreatedAt(t, a.store, id, time.Now().Unix()-ageDays*86400)
		return id
	}
	mk("alice", 60) // very overdue
	mk("alice", 40) // overdue
	mk("alice", 1)  // not due
	mk("bob", 90)   // another user's — must never appear for alice

	got := decode(t, do(t, a, "GET", "/monitoring/due", "", "alice"))
	due, _ := got["due"].([]any)
	if len(due) != 2 {
		t.Fatalf("alice has two due reviews, got %d: %s", len(due), mustJSON(t, got))
	}
	first := due[0].(map[string]any)
	second := due[1].(map[string]any)
	if first["dueAt"].(float64) >= second["dueAt"].(float64) {
		t.Error("the most overdue review must come first")
	}
	if od, _ := first["overdueDays"].(float64); od < 25 {
		t.Errorf("a 60-day-old 30-day cadence is ~30 days overdue, got %v", od)
	}
	if link, _ := first["researchLink"].(map[string]any); link["hash"] != "#research/thesis" {
		t.Errorf("a due review must link into Research, got %v", link)
	}

	t.Run("a guest sees nothing", func(t *testing.T) {
		got := decode(t, do(t, a, "GET", "/monitoring/due", "", ""))
		if due, _ := got["due"].([]any); len(due) != 0 {
			t.Errorf("a guest must see no reviews, got %v", due)
		}
	})
}

// ---------------------------------------------------------------- additive event filters

// TestEventFiltersAreAdditive — the unfiltered response must be byte-compatible with the old one.
func TestEventFiltersAreAdditive(t *testing.T) {
	a := testAPI(t)
	now := time.Now().Unix()
	mustAppend := func(ev Event) {
		if err := a.store.AppendEvent(ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	mustAppend(Event{ID: "e1", UserID: "alice", Ticker: "NVDA", Message: "old untargeted", TS: now - 500})
	mustAppend(Event{ID: "e2", UserID: "alice", Ticker: "NVDA", Message: "linked", TS: now - 100,
		ThesisID: testThesisID})
	mustAppend(Event{ID: "e3", UserID: "bob", Ticker: "NVDA", Message: "bob's", TS: now,
		ThesisID: testThesisID})

	t.Run("no filter returns everything the caller owns", func(t *testing.T) {
		got := decode(t, do(t, a, "GET", "/events", "", "alice"))
		if evs, _ := got["events"].([]any); len(evs) != 2 {
			t.Fatalf("alice owns two events, got %d", len(evs))
		}
	})
	t.Run("thesisId filters", func(t *testing.T) {
		got := decode(t, do(t, a, "GET", "/events?thesisId="+testThesisID, "", "alice"))
		evs, _ := got["events"].([]any)
		if len(evs) != 1 {
			t.Fatalf("want 1 thesis-linked event, got %d", len(evs))
		}
		if evs[0].(map[string]any)["id"] != "e2" {
			t.Errorf("wrong event: %v", evs[0])
		}
	})
	t.Run("since is strictly after the boundary", func(t *testing.T) {
		got := decode(t, do(t, a, "GET", "/events?since="+itoa(now-100), "", "alice"))
		if evs, _ := got["events"].([]any); len(evs) != 0 {
			t.Errorf("an event AT the boundary is not after it, got %v", evs)
		}
	})
	t.Run("a filter never leaks another user's events", func(t *testing.T) {
		got := decode(t, do(t, a, "GET", "/events?thesisId="+testThesisID, "", "alice"))
		for _, e := range got["events"].([]any) {
			if e.(map[string]any)["id"] == "e3" {
				t.Fatal("bob's event leaked into alice's filtered feed")
			}
		}
	})
}

func TestLegacyRulesKeepBeingEvaluatedButStayInvisible(t *testing.T) {
	a := testAPI(t)
	// A pre-accounts rule, as migrateLegacyRules leaves it (D-23: read-never, write-never).
	a.store.AddRule(Rule{UserID: "_legacy", Ticker: "NVDA", Type: TypeTrendFlip, Active: true,
		CooldownSec: 3600, Params: map[string]any{}})

	if got := decode(t, do(t, a, "GET", "/rules", "", "alice")); len(got["rules"].([]any)) != 0 {
		t.Error("a _legacy rule must not be visible to any account")
	}
	// It must still be in the evaluator's view — Step 08 must not change that either way (D-23).
	found := false
	for _, r := range a.store.Rules() {
		if r.UserID == "_legacy" {
			found = true
		}
	}
	if !found {
		t.Error("a _legacy rule must keep being evaluated — Step 08 must not silently drop it")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
	return string(b)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
