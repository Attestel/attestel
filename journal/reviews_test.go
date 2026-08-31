package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// reviews_test.go — the saved-review store's guarantees, through the real HTTP surface.
//
// The properties here are the ones that make a research log trustworthy to look back at months
// later: a review is an immutable snapshot except for the user's own reaction to it, it belongs to
// exactly one user, and — the load-bearing one — SAVING A REVIEW NEVER TOUCHES THE USER'S THESIS.
//
// No network, no model, no keys.

func sampleReview(thesisID string) map[string]any {
	return map[string]any{
		"schemaVersion": 1,
		"ticker":        "NVDA",
		"target":        map[string]any{"type": "thesis", "id": thesisID},
		"thesisId":      thesisID,
		"task":          "challenge_thesis",
		"lens": map[string]any{
			"id": "builtin:skeptic", "name": "Devil's Advocate", "builtin": true,
			"version": "2026-07-30T00:00:00Z",
		},
		"findings": []any{
			map[string]any{
				"id": "fnd_aaaa", "statement": "The attach-rate assumption has no floor.",
				"kind": "inference", "citedEvidenceIds": []any{},
			},
			map[string]any{
				"id": "fnd_bbbb", "statement": "Margin came in at 71.5%.",
				"kind": "fact", "citedEvidenceIds": []any{"ev_1111"},
			},
		},
		"agreements": []any{}, "disagreements": []any{},
		"missingEvidence": []any{"A quarter-by-quarter series."},
		"openQuestions":   []any{"What is the signed commitment?"},
		"citedEvidenceIds": []any{"ev_1111"},
		"lensConfidence":   60,
		"modelUsed":        "qwen2.5:7b",
		"stub":             false,
		"disclaimer":       "Contextual research support. Not investment advice, not a recommendation.",
	}
}

func createReview(t *testing.T, e *testEnv, uid, thesisID string) map[string]any {
	t.Helper()
	code, body := e.do(t, uid, "POST", "/reviews", sampleReview(thesisID))
	if code != http.StatusCreated {
		t.Fatalf("create review: got %d, body %v", code, body)
	}
	rev, _ := body["review"].(map[string]any)
	if rev == nil {
		t.Fatal("create review: no review returned")
	}
	return rev
}

// ---- the rule that matters most ----

func TestSavingAReviewNeverMutatesTheThesis(t *testing.T) {
	e := newTestEnv(t)
	id := e.makeThesis(t, "u1", "NVDA", "Networking attach lifts blended ASPs.")

	_, before := e.do(t, "u1", "GET", "/theses/"+id, nil)
	beforeJSON, _ := json.Marshal(before["thesis"])

	createReview(t, e, "u1", id)

	_, after := e.do(t, "u1", "GET", "/theses/"+id, nil)
	afterJSON, _ := json.Marshal(after["thesis"])

	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("saving a review changed the thesis:\n before: %s\n  after: %s", beforeJSON, afterJSON)
	}

	// And specifically: no review text leaked into a user-owned field.
	th, _ := after["thesis"].(map[string]any)
	blob, _ := json.Marshal(th)
	for _, phrase := range []string{"attach-rate assumption has no floor", "signed commitment", "71.5"} {
		if contains(string(blob), phrase) {
			t.Fatalf("review text %q leaked into the thesis: %s", phrase, blob)
		}
	}

	// The review is also not a version event — it is not a belief change.
	versions, _ := th["versions"].([]any)
	if len(versions) != 0 {
		t.Fatalf("saving a review appended %d version events", len(versions))
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		})()
}

// ---- identity + immutability ----

func TestReviewGetsServerAssignedIdentity(t *testing.T) {
	e := newTestEnv(t)
	id := e.makeThesis(t, "u1", "NVDA", "claim")
	rev := createReview(t, e, "u1", id)

	rid, _ := rev["id"].(string)
	if len(rid) < 5 || rid[:4] != "rev_" {
		t.Fatalf("want a rev_ id, got %q", rid)
	}
	if rev["savedAt"] == nil {
		t.Fatal("no savedAt stamped")
	}
	if rev["rating"] != nil {
		t.Fatalf("a new review must start unrated, got %v", rev["rating"])
	}
}

func TestReviewBodyIsImmutable(t *testing.T) {
	e := newTestEnv(t)
	id := e.makeThesis(t, "u1", "NVDA", "claim")
	rev := createReview(t, e, "u1", id)
	rid, _ := rev["id"].(string)

	// Try to rewrite the analysis through PATCH. Only rating/citationFeedback are honoured, so every
	// content field must come back untouched.
	code, body := e.do(t, "u1", "PATCH", "/reviews/"+rid, map[string]any{
		"rating":   "useful",
		"findings": []any{map[string]any{"id": "fnd_x", "statement": "rewritten", "kind": "fact"}},
		"task":     "explain_evidence",
		"lens":     map[string]any{"id": "builtin:plain"},
		"ticker":   "AAPL",
	})
	if code != http.StatusOK {
		t.Fatalf("patch: got %d, body %v", code, body)
	}
	got, _ := body["review"].(map[string]any)

	if got["rating"] != "useful" {
		t.Fatalf("rating not applied: %v", got["rating"])
	}
	if got["task"] != "challenge_thesis" {
		t.Fatalf("task was mutated to %v", got["task"])
	}
	if got["ticker"] != "NVDA" {
		t.Fatalf("ticker was mutated to %v", got["ticker"])
	}
	findings, _ := got["findings"].([]any)
	if len(findings) != 2 {
		t.Fatalf("findings were mutated: %v", findings)
	}
	f0, _ := findings[0].(map[string]any)
	if f0["statement"] != "The attach-rate assumption has no floor." {
		t.Fatalf("finding text was rewritten: %v", f0["statement"])
	}
}

func TestRatingAcceptsOnlyKnownValues(t *testing.T) {
	e := newTestEnv(t)
	id := e.makeThesis(t, "u1", "NVDA", "claim")
	rid, _ := createReview(t, e, "u1", id)["id"].(string)

	code, _ := e.do(t, "u1", "PATCH", "/reviews/"+rid, map[string]any{"rating": "amazing"})
	if code != http.StatusBadRequest {
		t.Fatalf("bad rating: want 400, got %d", code)
	}
	// Clearing back to null is allowed.
	code, body := e.do(t, "u1", "PATCH", "/reviews/"+rid, map[string]any{"rating": ""})
	if code != http.StatusOK {
		t.Fatalf("clear rating: got %d", code)
	}
	got, _ := body["review"].(map[string]any)
	if got["rating"] != nil {
		t.Fatalf("rating not cleared: %v", got["rating"])
	}
}

func TestCitationFeedbackOnlyReferencesRealFindings(t *testing.T) {
	e := newTestEnv(t)
	id := e.makeThesis(t, "u1", "NVDA", "claim")
	rid, _ := createReview(t, e, "u1", id)["id"].(string)

	code, body := e.do(t, "u1", "PATCH", "/reviews/"+rid, map[string]any{
		"citationFeedback": []any{
			map[string]any{"findingId": "fnd_bbbb", "accepted": true},
			map[string]any{"findingId": "fnd_not_on_this_review", "accepted": false},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("patch feedback: got %d, body %v", code, body)
	}
	got, _ := body["review"].(map[string]any)
	fb, _ := got["citationFeedback"].([]any)
	if len(fb) != 1 {
		t.Fatalf("want only the real finding's feedback kept, got %v", fb)
	}
	entry, _ := fb[0].(map[string]any)
	if entry["findingId"] != "fnd_bbbb" {
		t.Fatalf("wrong feedback kept: %v", entry)
	}
}

// ---- scoping ----

func TestReviewsAreScopedPerUser(t *testing.T) {
	e := newTestEnv(t)
	id := e.makeThesis(t, "u1", "NVDA", "claim")
	rid, _ := createReview(t, e, "u1", id)["id"].(string)

	// A different user cannot see it — 404, never 403.
	code, _ := e.do(t, "u2", "GET", "/reviews/"+rid, nil)
	if code != http.StatusNotFound {
		t.Fatalf("cross-user get: want 404, got %d", code)
	}
	code, body := e.do(t, "u2", "GET", "/reviews", nil)
	if code != http.StatusOK {
		t.Fatalf("list as u2: got %d", code)
	}
	list, _ := body["reviews"].([]any)
	if len(list) != 0 {
		t.Fatalf("u2 sees %d of u1's reviews", len(list))
	}
	// And cannot modify or delete it.
	if code, _ := e.do(t, "u2", "PATCH", "/reviews/"+rid, map[string]any{"rating": "useful"}); code != http.StatusNotFound {
		t.Fatalf("cross-user patch: want 404, got %d", code)
	}
	if code, _ := e.do(t, "u2", "DELETE", "/reviews/"+rid, nil); code != http.StatusNotFound {
		t.Fatalf("cross-user delete: want 404, got %d", code)
	}
}

func TestGuestCannotWriteReviews(t *testing.T) {
	e := newTestEnv(t)
	if code, _ := e.do(t, "", "POST", "/reviews", sampleReview("x")); code != http.StatusUnauthorized {
		t.Fatalf("guest create: want 401, got %d", code)
	}
	if code, _ := e.do(t, "", "PATCH", "/reviews/rev_1", map[string]any{"rating": "useful"}); code != http.StatusUnauthorized {
		t.Fatalf("guest patch: want 401, got %d", code)
	}
	if code, _ := e.do(t, "", "DELETE", "/reviews/rev_1", nil); code != http.StatusUnauthorized {
		t.Fatalf("guest delete: want 401, got %d", code)
	}
	// A guest read is allowed and simply empty.
	code, body := e.do(t, "", "GET", "/reviews", nil)
	if code != http.StatusOK {
		t.Fatalf("guest list: want 200, got %d", code)
	}
	if list, _ := body["reviews"].([]any); len(list) != 0 {
		t.Fatalf("guest sees %d reviews", len(list))
	}
}

// ---- the read contract Library consumes ----

func TestListFiltersAndOrdering(t *testing.T) {
	e := newTestEnv(t)
	idA := e.makeThesis(t, "u1", "NVDA", "claim A")
	idB := e.makeThesis(t, "u1", "NVDA", "claim B")

	createReview(t, e, "u1", idA)
	second := sampleReview(idB)
	second["task"] = "explain_evidence"
	second["lens"] = map[string]any{"id": "builtin:plain", "name": "Plain English", "builtin": true}
	if code, _ := e.do(t, "u1", "POST", "/reviews", second); code != http.StatusCreated {
		t.Fatal("create second review")
	}

	code, body := e.do(t, "u1", "GET", "/reviews?thesisId="+idB, nil)
	if code != http.StatusOK {
		t.Fatalf("filter by thesisId: got %d", code)
	}
	list, _ := body["reviews"].([]any)
	if len(list) != 1 {
		t.Fatalf("thesisId filter returned %d", len(list))
	}

	_, body = e.do(t, "u1", "GET", "/reviews?task=explain_evidence", nil)
	if list, _ = body["reviews"].([]any); len(list) != 1 {
		t.Fatalf("task filter returned %d", len(list))
	}

	_, body = e.do(t, "u1", "GET", "/reviews?lensId=builtin:plain", nil)
	if list, _ = body["reviews"].([]any); len(list) != 1 {
		t.Fatalf("lensId filter returned %d", len(list))
	}

	_, body = e.do(t, "u1", "GET", "/reviews?ticker=NVDA", nil)
	if list, _ = body["reviews"].([]any); len(list) != 2 {
		t.Fatalf("ticker filter returned %d", len(list))
	}

	// Newest first.
	first, _ := list[0].(map[string]any)
	if first["task"] != "explain_evidence" {
		t.Fatalf("want newest first, got %v", first["task"])
	}
}

func TestReviewRequiresFindings(t *testing.T) {
	e := newTestEnv(t)
	rev := sampleReview("x")
	delete(rev, "findings")
	code, body := e.do(t, "u1", "POST", "/reviews", rev)
	if code != http.StatusBadRequest {
		t.Fatalf("review without findings: want 400, got %d (%v)", code, body)
	}
}

func TestDeleteRemovesOnlyThatReview(t *testing.T) {
	e := newTestEnv(t)
	id := e.makeThesis(t, "u1", "NVDA", "claim")
	keep, _ := createReview(t, e, "u1", id)["id"].(string)
	drop, _ := createReview(t, e, "u1", id)["id"].(string)

	if code, _ := e.do(t, "u1", "DELETE", "/reviews/"+drop, nil); code != http.StatusOK {
		t.Fatal("delete failed")
	}
	if code, _ := e.do(t, "u1", "GET", "/reviews/"+drop, nil); code != http.StatusNotFound {
		t.Fatal("deleted review still readable")
	}
	if code, _ := e.do(t, "u1", "GET", "/reviews/"+keep, nil); code != http.StatusOK {
		t.Fatal("the other review was removed too")
	}
	// Deleting a review must not touch the thesis either.
	if code, _ := e.do(t, "u1", "GET", "/theses/"+id, nil); code != http.StatusOK {
		t.Fatal("thesis disappeared with the review")
	}
}
