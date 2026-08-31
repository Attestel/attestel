package main

import (
	"net/http"
	"testing"
)

// outcomes_test.go — the outcome-review guarantees that decisions_test.go does not already assert.
//
// decisions_test.go covers the load-bearing properties end to end: a review never edits its decision,
// one review per decision (409 outcome_exists), the PATCH surface is the four user-authored fields,
// reasoning quality is structurally independent of P&L, and a foreign id reads back as 404. What is
// NOT covered there, and is covered here, is the refusal path around outcome CREATION:
//
//   - citing another user's decision must be refused as an unknown reference, never resolved;
//   - reasoningQuality is a bounded 1..5 scale on both create and revise — an out-of-range value is
//     rejected rather than clamped, because a silently clamped self-assessment is a fabricated one;
//   - the list surface is scoped to the caller even when the caller names a decision id they can see
//     the existence of by guessing.
//
// All of it runs offline: no network, no model, no API key (invariant #1).

// A decision id that belongs to someone else must behave exactly like one that does not exist. If it
// resolved, one user could attach a review — and a reasoning grade — to another user's record.
func TestOutcomeRejectsAnotherUsersDecision(t *testing.T) {
	e := newTestEnv(t)

	thesisID := e.makeThesis(t, "u1", "NVDA", "Claim.")
	dec := createDecision(t, e, "u1", decisionPayload(thesisID, nil))
	decID := dec["id"].(string)

	code, body := e.do(t, "u2", "POST", "/outcomes", map[string]any{
		"decisionId":      decID,
		"observedOutcome": "Trying to review a decision that is not mine.",
	})
	if code != http.StatusBadRequest || body["code"] != "unknown_reference" {
		t.Fatalf("reviewing another user's decision: expected 400 unknown_reference, got %d %v", code, body)
	}

	// And nothing was written: the owner's decision still has no review, so u2 could not create one
	// out from under them.
	_, list := e.do(t, "u1", "GET", "/outcomes?decisionId="+decID, nil)
	if got, _ := list["outcomes"].([]any); len(got) != 0 {
		t.Errorf("owner's decision picked up a review from another user: %v", got)
	}

	// A guest cannot write one either.
	code, _ = e.do(t, "", "POST", "/outcomes", map[string]any{
		"decisionId": decID, "observedOutcome": "guest",
	})
	if code != http.StatusUnauthorized {
		t.Errorf("guest outcome write: expected 401, got %d", code)
	}
}

// reasoningQuality is the user's own 1..5 assessment of their PROCESS (§5.4/§5.5). Out of range is a
// refusal on both the create and the revise path — never a clamp, and never a derived value.
func TestOutcomeReasoningQualityRangeIsEnforced(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "Claim.")

	for _, bad := range []int{0, 6, -1, 99} {
		dec := createDecision(t, e, "u1", decisionPayload(thesisID, nil))
		code, body := e.do(t, "u1", "POST", "/outcomes", map[string]any{
			"decisionId":       dec["id"],
			"observedOutcome":  "Something happened.",
			"reasoningQuality": bad,
		})
		if code != http.StatusBadRequest || body["field"] != "reasoningQuality" {
			t.Errorf("POST reasoningQuality=%d: expected 400 on reasoningQuality, got %d %v", bad, code, body)
		}
	}

	// The bounds themselves are valid, and a review with no grade at all is a legitimate half-step.
	for _, ok := range []any{1, 5, nil} {
		dec := createDecision(t, e, "u1", decisionPayload(thesisID, nil))
		payload := map[string]any{
			"decisionId":      dec["id"],
			"observedOutcome": "Something happened.",
		}
		if ok != nil {
			payload["reasoningQuality"] = ok
		}
		code, body := e.do(t, "u1", "POST", "/outcomes", payload)
		if code != http.StatusCreated {
			t.Fatalf("POST reasoningQuality=%v: expected 201, got %d %v", ok, code, body)
		}
		out := body["outcome"].(map[string]any)
		if ok == nil {
			if out["reasoningQuality"] != nil {
				t.Errorf("an ungraded review must stay ungraded, got %v", out["reasoningQuality"])
			}
			continue
		}
		if out["reasoningQuality"].(float64) != float64(ok.(int)) {
			t.Errorf("reasoningQuality stored as %v, authored %v", out["reasoningQuality"], ok)
		}

		// The same bound holds on revision.
		id := out["id"].(string)
		code, body = e.do(t, "u1", "PATCH", "/outcomes/"+id, map[string]any{"reasoningQuality": 7})
		if code != http.StatusBadRequest || body["field"] != "reasoningQuality" {
			t.Errorf("PATCH reasoningQuality=7: expected 400 on reasoningQuality, got %d %v", code, body)
		}
	}
}

// The list surface is the caller's own, filter or no filter. Naming another user's decision id
// returns an empty list rather than their review.
func TestOutcomeListIsScopedToTheCaller(t *testing.T) {
	e := newTestEnv(t)

	mineThesis := e.makeThesis(t, "u1", "NVDA", "Mine.")
	mine := createDecision(t, e, "u1", decisionPayload(mineThesis, nil))
	code, body := e.do(t, "u1", "POST", "/outcomes", map[string]any{
		"decisionId": mine["id"], "observedOutcome": "Mine, reviewed.",
	})
	if code != http.StatusCreated {
		t.Fatalf("seed u1 outcome: %d %v", code, body)
	}

	theirsThesis := e.makeThesis(t, "u2", "NVDA", "Theirs.")
	theirs := createDecision(t, e, "u2", decisionPayload(theirsThesis, nil))
	code, body = e.do(t, "u2", "POST", "/outcomes", map[string]any{
		"decisionId": theirs["id"], "observedOutcome": "Theirs, reviewed.",
	})
	if code != http.StatusCreated {
		t.Fatalf("seed u2 outcome: %d %v", code, body)
	}

	_, list := e.do(t, "u1", "GET", "/outcomes", nil)
	got, _ := list["outcomes"].([]any)
	if len(got) != 1 {
		t.Fatalf("u1 should see exactly their own review, got %d: %v", len(got), got)
	}
	if got[0].(map[string]any)["observedOutcome"] != "Mine, reviewed." {
		t.Errorf("u1 saw the wrong review: %v", got[0])
	}

	// Filtering by a decision id the caller does not own yields nothing, not a leak.
	_, list = e.do(t, "u1", "GET", "/outcomes?decisionId="+theirs["id"].(string), nil)
	if got, _ := list["outcomes"].([]any); len(got) != 0 {
		t.Errorf("filtering by another user's decision id leaked a review: %v", got)
	}
}
