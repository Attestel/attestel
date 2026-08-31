package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// decisions_test.go — the decision/outcome layer's guarantees, through the real HTTP surface.
//
// Everything here runs with NO network, NO model and NO API key (invariant #1). The one test that
// needs a price source stands up a tiny local stub rather than reaching for a provider.
//
// The properties under test are the ones that make a review honest months later:
//
//   - the snapshot does not move when the thesis moves (otherwise the record rewrites the past);
//   - a citation always points at something the caller actually owns;
//   - a deliberate non-action is a real, storable decision;
//   - a review can never edit the decision it reviews;
//   - paper and live never collapse into one number;
//   - nothing model-shaped can write a user-authored field.

// ---- helpers -------------------------------------------------------------------------------------

// decisionPayload is a complete, valid, offline decision on `thesisID`.
func decisionPayload(thesisID string, extra map[string]any) map[string]any {
	body := map[string]any{
		"ticker":              "NVDA",
		"thesisId":            thesisID,
		"intent":              "act",
		"action":              map[string]any{"kind": "open", "side": "long"},
		"horizon":             "months",
		"rationale":           "Datacenter backlog is visible through FY27 and the pullback took it back to the 50DMA.",
		"userConfidence":      60,
		"conditionsToMonitor": []any{"Hyperscaler capex guidance for the next two quarters"},
		"openQuestions":       []any{"How much of the backlog is non-cancellable?"},
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func createDecision(t *testing.T, e *testEnv, uid string, payload map[string]any) map[string]any {
	t.Helper()
	code, body := e.do(t, uid, "POST", "/decisions", payload)
	if code != http.StatusCreated {
		t.Fatalf("POST /decisions: got %d, body %v", code, body)
	}
	dec, _ := body["decision"].(map[string]any)
	if dec == nil {
		t.Fatalf("POST /decisions: no decision in %v", body)
	}
	return dec
}

// makeTrade records a manual trade and returns it.
func makeTrade(t *testing.T, e *testEnv, uid, mode string) map[string]any {
	t.Helper()
	code, body := e.do(t, uid, "POST", "/trades", map[string]any{
		"ticker": "NVDA", "side": "long", "mode": mode, "origin": "manual",
		"entry": map[string]any{"date": "2026-07-01", "price": 160.0, "size": 10, "timeframe": "1D"},
		"exit":  map[string]any{"date": "2026-07-20", "price": 176.0},
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /trades (%s): got %d, body %v", mode, code, body)
	}
	tr, _ := body["trade"].(map[string]any)
	return tr
}

func snapshotOf(t *testing.T, dec map[string]any) map[string]any {
	t.Helper()
	snap, _ := dec["snapshot"].(map[string]any)
	if snap == nil {
		t.Fatalf("decision has no snapshot: %v", dec)
	}
	return snap
}

// ---- the snapshot is write-once ------------------------------------------------------------------

// The load-bearing test: the thesis is edited AFTER the decision, and the decision's snapshot is
// byte-identical. If this ever fails, every stored decision silently starts describing a belief the
// user did not hold when they decided.
func TestDecisionSnapshotStableAfterThesisEdit(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "Datacenter demand outruns supply through FY27.")

	// Give the thesis structure worth snapshotting.
	code, _ := e.do(t, "u1", "PATCH", "/theses/"+thesisID, map[string]any{
		"confidence":             60,
		"horizon":                "months",
		"assumptions":            []any{map[string]any{"text": "Hyperscaler capex keeps growing."}},
		"invalidationConditions": []any{map[string]any{"text": "Two consecutive quarters of capex cuts."}},
	})
	if code != http.StatusOK {
		t.Fatalf("prepare thesis: got %d", code)
	}

	dec := createDecision(t, e, "u1", decisionPayload(thesisID, nil))
	before, err := json.Marshal(snapshotOf(t, dec))
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	// Now rewrite the thesis substantially: a new claim, a different confidence, a replaced
	// assumption, and a lifecycle change.
	code, patched := e.do(t, "u1", "PATCH", "/theses/"+thesisID, map[string]any{
		"claim":       "Actually, supply catches up in FY27 and pricing compresses.",
		"confidence":  20,
		"assumptions": []any{map[string]any{"text": "Competition lands meaningful share."}},
	})
	if code != http.StatusOK {
		t.Fatalf("edit thesis: got %d, body %v", code, patched)
	}

	code, body := e.do(t, "u1", "GET", "/decisions/"+dec["id"].(string), nil)
	if code != http.StatusOK {
		t.Fatalf("GET decision: got %d", code)
	}
	after, err := json.Marshal(snapshotOf(t, body["decision"].(map[string]any)))
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("snapshot changed after a thesis edit.\nbefore: %s\nafter:  %s", before, after)
	}

	// And it is the OLD wording that survived, not merely "some" wording.
	snap := snapshotOf(t, body["decision"].(map[string]any))
	if snap["claim"] != "Datacenter demand outruns supply through FY27." {
		t.Errorf("snapshot claim should hold the wording at decision time, got %q", snap["claim"])
	}
	if snap["confidence"].(float64) != 60 {
		t.Errorf("snapshot confidence should be the value at decision time (60), got %v", snap["confidence"])
	}
	asm, _ := snap["assumptions"].([]any)
	if len(asm) != 1 || asm[0].(map[string]any)["text"] != "Hyperscaler capex keeps growing." {
		t.Errorf("snapshot assumptions should be the ones at decision time, got %v", asm)
	}
}

// Every field except tradeId is refused BY NAME. Silently ignoring an edit would let a client believe
// the record had been rewritten — the failure mode this rule exists to prevent.
func TestDecisionIsWriteOnceExceptTradeLink(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "Claim.")
	dec := createDecision(t, e, "u1", decisionPayload(thesisID, nil))
	id := dec["id"].(string)

	for field, value := range map[string]any{
		"rationale":      "rewritten after the fact",
		"intent":         "do_not_act",
		"userConfidence": 95,
		"snapshot":       map[string]any{"claim": "forged"},
		"evidenceIds":    []any{"ev_forged"},
		"decidedAt":      1,
		"thesisId":       "another",
	} {
		code, body := e.do(t, "u1", "PATCH", "/decisions/"+id, map[string]any{field: value})
		if code != http.StatusBadRequest {
			t.Errorf("PATCH %s: expected 400, got %d (%v)", field, code, body)
			continue
		}
		if body["code"] != "immutable_field" {
			t.Errorf("PATCH %s: expected code immutable_field, got %v", field, body["code"])
		}
	}

	// The record is unchanged on disk.
	_, body := e.do(t, "u1", "GET", "/decisions/"+id, nil)
	got := body["decision"].(map[string]any)
	if got["rationale"] != dec["rationale"] || got["intent"] != "act" {
		t.Errorf("record mutated by a refused patch: %v", got)
	}
}

// ---- references ----------------------------------------------------------------------------------

// A citation must point at something the CALLER owns. Another user's thesis, evidence or review is
// indistinguishable from a nonexistent one — existence is never confirmed across users.
func TestDecisionRejectsForeignAndUnknownReferences(t *testing.T) {
	e := newTestEnv(t)
	mine := e.makeThesis(t, "u1", "NVDA", "My claim.")
	theirs := e.makeThesis(t, "u2", "NVDA", "Their claim.")
	theirEvidence := mustCreate(t, e, "u2", deterministicFact("2026-07-29"))
	theirReview := createReview(t, e, "u2", theirs)

	cases := []struct {
		name    string
		payload map[string]any
		field   string
	}{
		{"foreign thesis", decisionPayload(theirs, nil), "thesisId"},
		{"unknown thesis", decisionPayload("does-not-exist", nil), "thesisId"},
		{"foreign evidence", decisionPayload(mine, map[string]any{"evidenceIds": []any{theirEvidence["id"]}}), "evidenceIds"},
		{"unknown evidence", decisionPayload(mine, map[string]any{"evidenceIds": []any{"ev_0000000000000000"}}), "evidenceIds"},
		{"foreign review", decisionPayload(mine, map[string]any{"reviewIds": []any{theirReview["id"]}}), "reviewIds"},
		{"unknown review", decisionPayload(mine, map[string]any{"reviewIds": []any{"rev_0000000000000000"}}), "reviewIds"},
		{"unknown trade", decisionPayload(mine, map[string]any{"tradeId": "nope"}), "tradeId"},
		{"unknown version", decisionPayload(mine, map[string]any{"thesisVersionId": "ver_forged"}), "thesisVersionId"},
	}
	for _, tc := range cases {
		code, body := e.do(t, "u1", "POST", "/decisions", tc.payload)
		if code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d (%v)", tc.name, code, body)
			continue
		}
		if body["code"] != "unknown_reference" {
			t.Errorf("%s: expected code unknown_reference, got %v", tc.name, body["code"])
		}
		if body["field"] != tc.field {
			t.Errorf("%s: expected field %q, got %v", tc.name, tc.field, body["field"])
		}
	}

	// Own records are accepted, and the citation survives the round trip.
	myEvidence := mustCreate(t, e, "u1", deterministicFact("2026-07-29"))
	myReview := createReview(t, e, "u1", mine)
	dec := createDecision(t, e, "u1", decisionPayload(mine, map[string]any{
		"evidenceIds": []any{myEvidence["id"]},
		"reviewIds":   []any{myReview["id"]},
	}))
	if ids, _ := dec["evidenceIds"].([]any); len(ids) != 1 || ids[0] != myEvidence["id"] {
		t.Errorf("cited evidence not stored: %v", dec["evidenceIds"])
	}
	snap := snapshotOf(t, dec)
	ev, _ := snap["evidence"].([]any)
	if len(ev) != 1 {
		t.Fatalf("snapshot should carry the cited evidence metadata, got %v", ev)
	}
	item := ev[0].(map[string]any)
	if item["provider"] != "analysis" || item["dataState"] != "synthetic" {
		t.Errorf("snapshot evidence lost its provenance: %v", item)
	}
	// D-08: the excerpt cap lives in exactly one place, so the snapshot must not carry one.
	if _, present := item["excerpt"]; present {
		t.Errorf("snapshot evidence must not copy the excerpt: %v", item)
	}
}

// Change-item and monitoring-rule ids are stored as OPAQUE references: they belong to the monitoring
// lane, and resolving them here would be a second monitoring representation. They must round-trip
// untouched, including when the referenced change set does not exist in this worktree yet.
func TestDecisionStoresMonitoringReferencesOpaquely(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "Claim.")
	dec := createDecision(t, e, "u1", decisionPayload(thesisID, map[string]any{
		"changeItemIds":     []any{"chg_1111111111111111", "chg_2222222222222222"},
		"monitoringRuleIds": []any{"rule-abc"},
		"checkpointId":      "chk_9999999999999999",
	}))
	if ids, _ := dec["changeItemIds"].([]any); len(ids) != 2 {
		t.Errorf("changeItemIds not preserved: %v", dec["changeItemIds"])
	}
	if ids, _ := dec["monitoringRuleIds"].([]any); len(ids) != 1 || ids[0] != "rule-abc" {
		t.Errorf("monitoringRuleIds not preserved: %v", dec["monitoringRuleIds"])
	}
	if dec["checkpointId"] != "chk_9999999999999999" {
		t.Errorf("checkpointId not preserved: %v", dec["checkpointId"])
	}
}

// ---- deliberate non-action -----------------------------------------------------------------------

// The record `Trade` cannot express. A non-action carries no side, no price, no size — and is still a
// first-class, countable decision.
func TestDeliberateNonActionIsAccepted(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "Claim.")

	for _, intent := range []string{"do_not_act", "wait"} {
		dec := createDecision(t, e, "u1", decisionPayload(thesisID, map[string]any{
			"intent":    intent,
			"action":    map[string]any{"kind": "none"},
			"rationale": "Position is already sized; the setup does not add anything.",
		}))
		if dec["intent"] != intent {
			t.Errorf("intent not stored: %v", dec["intent"])
		}
		act, _ := dec["action"].(map[string]any)
		if act["kind"] != "none" {
			t.Errorf("%s should record action.kind none, got %v", intent, act)
		}
		if act["side"] != nil {
			t.Errorf("%s must not carry a side, got %v", intent, act["side"])
		}
		if dec["tradeId"] != nil {
			t.Errorf("%s must not carry a trade link, got %v", intent, dec["tradeId"])
		}
	}

	// The two directions of the rule: a non-action cannot hide an action, and "act" cannot record
	// nothing done.
	code, body := e.do(t, "u1", "POST", "/decisions", decisionPayload(thesisID, map[string]any{
		"intent": "do_not_act", "action": map[string]any{"kind": "open", "side": "long"},
	}))
	if code != http.StatusBadRequest || body["field"] != "action.kind" {
		t.Errorf("non-action with an action should be refused, got %d %v", code, body)
	}
	code, body = e.do(t, "u1", "POST", "/decisions", decisionPayload(thesisID, map[string]any{
		"intent": "act", "action": map[string]any{"kind": "none"},
	}))
	if code != http.StatusBadRequest || body["field"] != "action.kind" {
		t.Errorf("act with no action should be refused, got %d %v", code, body)
	}

	// A rationale is not optional — the reasoning IS the record.
	code, _ = e.do(t, "u1", "POST", "/decisions", decisionPayload(thesisID, map[string]any{"rationale": "   "}))
	if code != http.StatusBadRequest {
		t.Errorf("a decision without a rationale should be refused, got %d", code)
	}
}

// ---- outcome reviews -----------------------------------------------------------------------------

// A review observes; it never edits. This asserts it structurally: after a full review — including a
// deliberately harsh reasoning score — the decision record is byte-identical.
func TestOutcomeReviewCannotChangeTheDecision(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "Claim.")
	code, _ := e.do(t, "u1", "PATCH", "/theses/"+thesisID, map[string]any{
		"assumptions": []any{map[string]any{"text": "Hyperscaler capex keeps growing."}},
	})
	if code != http.StatusOK {
		t.Fatalf("prepare thesis: %d", code)
	}
	dec := createDecision(t, e, "u1", decisionPayload(thesisID, nil))
	id := dec["id"].(string)
	before, _ := json.Marshal(dec)

	asmID := snapshotOf(t, dec)["assumptions"].([]any)[0].(map[string]any)["id"].(string)

	code, body := e.do(t, "u1", "POST", "/outcomes", map[string]any{
		"decisionId":       id,
		"observedOutcome":  "The position is up 9% but capex guidance was cut, so I was right for the wrong reason.",
		"horizonElapsed":   true,
		"reasoningQuality": 2,
		"assumptions":      []any{map[string]any{"thesisItemId": asmID, "verdict": "failed", "note": "Capex growth stalled."}},
		"processLessons":   []any{"I did not write down what would falsify this."},
		"retrospective":    "Sizing was luck, not process.",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /outcomes: got %d, body %v", code, body)
	}
	out := body["outcome"].(map[string]any)
	if out["reasoningQuality"].(float64) != 2 {
		t.Errorf("reasoningQuality not stored as authored: %v", out["reasoningQuality"])
	}

	_, after := e.do(t, "u1", "GET", "/decisions/"+id, nil)
	afterJSON, _ := json.Marshal(after["decision"])
	if string(before) != string(afterJSON) {
		t.Errorf("the decision changed when it was reviewed.\nbefore: %s\nafter:  %s", before, afterJSON)
	}

	// One review per decision.
	code, body = e.do(t, "u1", "POST", "/outcomes", map[string]any{
		"decisionId": id, "observedOutcome": "second attempt",
	})
	if code != http.StatusConflict || body["code"] != "outcome_exists" {
		t.Errorf("a second review should 409 outcome_exists, got %d %v", code, body)
	}

	// A revision touches the four user fields only.
	code, body = e.do(t, "u1", "PATCH", "/outcomes/"+out["id"].(string), map[string]any{
		"reasoningQuality": 4, "retrospective": "On reflection the process was better than I credited.",
	})
	if code != http.StatusOK {
		t.Fatalf("PATCH /outcomes: got %d, body %v", code, body)
	}
	if body["outcome"].(map[string]any)["reasoningQuality"].(float64) != 4 {
		t.Errorf("revision not applied: %v", body["outcome"])
	}
	code, body = e.do(t, "u1", "PATCH", "/outcomes/"+out["id"].(string), map[string]any{
		"marketOutcome": map[string]any{"pnlPct": 999},
	})
	if code != http.StatusBadRequest || body["code"] != "immutable_field" {
		t.Errorf("a client must not assert marketOutcome, got %d %v", code, body)
	}

	// An assumption that was not on the thesis at decision time cannot be graded.
	dec2 := createDecision(t, e, "u1", decisionPayload(thesisID, nil))
	code, body = e.do(t, "u1", "POST", "/outcomes", map[string]any{
		"decisionId": dec2["id"], "observedOutcome": "x",
		"assumptions": []any{map[string]any{"thesisItemId": "asm_never_existed", "verdict": "held"}},
	})
	if code != http.StatusBadRequest || body["code"] != "unknown_reference" {
		t.Errorf("grading an unknown assumption should be refused, got %d %v", code, body)
	}
}

// Reasoning quality and market outcome are structurally independent: a losing trade reviewed as good
// reasoning, and a winning one reviewed as bad, must both store and read back exactly as authored.
func TestReasoningQualityIsIndependentOfPnl(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "Claim.")

	winner := makeTrade(t, e, "u1", "live") // +10% by construction
	dec := createDecision(t, e, "u1", decisionPayload(thesisID, map[string]any{"tradeId": winner["id"]}))
	code, body := e.do(t, "u1", "POST", "/outcomes", map[string]any{
		"decisionId": dec["id"], "observedOutcome": "Made money on a thesis that was wrong.",
		"reasoningQuality": 1,
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /outcomes: %d %v", code, body)
	}
	out := body["outcome"].(map[string]any)
	if out["reasoningQuality"].(float64) != 1 {
		t.Errorf("a profitable decision must be allowed a reasoningQuality of 1, got %v", out["reasoningQuality"])
	}
	mo, _ := out["marketOutcome"].(map[string]any)
	if mo == nil {
		t.Fatal("a linked trade should produce a derived marketOutcome")
	}
	if mo["pnlPct"].(float64) <= 0 {
		t.Errorf("expected a positive derived pnlPct, got %v", mo["pnlPct"])
	}
	if mo["tradeMode"] != "live" {
		t.Errorf("marketOutcome must carry the trade mode, got %v", mo["tradeMode"])
	}

	// A deliberate non-action has no trade and therefore no market outcome — not a zero.
	dec2 := createDecision(t, e, "u1", decisionPayload(thesisID, map[string]any{
		"intent": "do_not_act", "action": map[string]any{"kind": "none"},
	}))
	code, body = e.do(t, "u1", "POST", "/outcomes", map[string]any{
		"decisionId": dec2["id"], "observedOutcome": "It ran without me. Staying out was still the right call at that size.",
		"reasoningQuality": 5,
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /outcomes for a non-action: %d %v", code, body)
	}
	if body["outcome"].(map[string]any)["marketOutcome"] != nil {
		t.Errorf("a non-action must have a null marketOutcome, got %v", body["outcome"].(map[string]any)["marketOutcome"])
	}
}

// ---- trade link and mode separation --------------------------------------------------------------

// Linking is the one permitted mutation, and it copies the mode so a decision-level metric can group
// by it. Paper and live are both linkable and never merged.
func TestTradeLinkCopiesModeAndKeepsPaperSeparate(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "Claim.")
	live := makeTrade(t, e, "u1", "live")
	paper := makeTrade(t, e, "u1", "paper")

	dec := createDecision(t, e, "u1", decisionPayload(thesisID, nil))
	code, body := e.do(t, "u1", "PATCH", "/decisions/"+dec["id"].(string), map[string]any{"tradeId": live["id"]})
	if code != http.StatusOK {
		t.Fatalf("link live trade: %d %v", code, body)
	}
	if body["decision"].(map[string]any)["tradeMode"] != "live" {
		t.Errorf("tradeMode should be copied at link time, got %v", body["decision"])
	}

	decP := createDecision(t, e, "u1", decisionPayload(thesisID, map[string]any{"tradeId": paper["id"]}))
	if decP["tradeMode"] != "paper" {
		t.Errorf("a paper-linked decision must record tradeMode paper, got %v", decP["tradeMode"])
	}

	// Unlinking is expressible without rewriting anything else.
	code, body = e.do(t, "u1", "PATCH", "/decisions/"+dec["id"].(string), map[string]any{"tradeId": nil})
	if code != http.StatusOK {
		t.Fatalf("unlink: %d %v", code, body)
	}
	got := body["decision"].(map[string]any)
	if got["tradeId"] != nil || got["tradeMode"] != "" {
		t.Errorf("unlink should clear both fields, got %v", got)
	}

	// The trade ledger's own separation is untouched: stats stay per-mode.
	_, liveStats := e.do(t, "u1", "GET", "/stats?mode=live", nil)
	_, paperStats := e.do(t, "u1", "GET", "/stats?mode=paper", nil)
	_, allStats := e.do(t, "u1", "GET", "/stats?mode=all", nil)
	if liveStats["count"].(float64) != 1 || paperStats["count"].(float64) != 1 {
		t.Errorf("modes must be countable separately: live=%v paper=%v", liveStats["count"], paperStats["count"])
	}
	if allStats["count"].(float64) != 2 {
		t.Errorf("mode=all should still aggregate both, got %v", allStats["count"])
	}
	if liveStats["totalRealizedPnl"] == paperStats["totalRealizedPnl"] &&
		allStats["totalRealizedPnl"] == liveStats["totalRealizedPnl"] {
		t.Error("live and paper P&L must not collapse into one number")
	}
}

// ---- isolation and legacy ------------------------------------------------------------------------

// One user's decisions and reviews are invisible to another. A foreign id is 404, never 403.
func TestDecisionUserIsolation(t *testing.T) {
	e := newTestEnv(t)
	mine := e.makeThesis(t, "u1", "NVDA", "Claim.")
	dec := createDecision(t, e, "u1", decisionPayload(mine, nil))
	code, body := e.do(t, "u1", "POST", "/outcomes", map[string]any{
		"decisionId": dec["id"], "observedOutcome": "It worked.",
	})
	if code != http.StatusCreated {
		t.Fatalf("seed outcome: %d %v", code, body)
	}
	outID := body["outcome"].(map[string]any)["id"].(string)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/decisions/" + dec["id"].(string)},
		{"GET", "/outcomes/" + outID},
	} {
		code, _ := e.do(t, "u2", tc.method, tc.path, nil)
		if code != http.StatusNotFound {
			t.Errorf("%s %s as another user: expected 404, got %d", tc.method, tc.path, code)
		}
	}
	code, _ = e.do(t, "u2", "PATCH", "/decisions/"+dec["id"].(string), map[string]any{"tradeId": nil})
	if code != http.StatusNotFound {
		t.Errorf("cross-user patch: expected 404, got %d", code)
	}
	code, _ = e.do(t, "u2", "DELETE", "/decisions/"+dec["id"].(string), nil)
	if code != http.StatusNotFound {
		t.Errorf("cross-user delete: expected 404, got %d", code)
	}

	_, list := e.do(t, "u2", "GET", "/decisions", nil)
	if d, _ := list["decisions"].([]any); len(d) != 0 {
		t.Errorf("another user's list must be empty, got %v", d)
	}

	// A guest may read (and sees nothing) but may not write.
	code, _ = e.do(t, "", "GET", "/decisions", nil)
	if code != http.StatusOK {
		t.Errorf("guest read should be 200, got %d", code)
	}
	code, _ = e.do(t, "", "POST", "/decisions", decisionPayload(mine, nil))
	if code != http.StatusUnauthorized {
		t.Errorf("guest write should be 401, got %d", code)
	}
}

// Legacy trade records — no mode, no origin, no thesis id — still load and stay readable, and NO
// thesis or decision is ever inferred for them. `Trade.thesis` is free text, not a thesisId.
func TestLegacyTradesLoadAndAreNeverReinterpreted(t *testing.T) {
	e := newTestEnv(t)
	legacy := []map[string]any{{
		"id": "abcdef0123456789", "ticker": "NVDA", "side": "long", "status": "closed",
		"entry":  map[string]any{"date": "2025-01-06", "price": 140.0, "size": 5, "timeframe": "1D"},
		"exit":   map[string]any{"date": "2025-02-03", "price": 132.0},
		"thesis": "Blackwell ramp; bought the dip.",
		"tags":   []any{"swing"},
	}}
	buf, _ := json.Marshal(legacy)
	if err := os.MkdirAll(filepath.Join(e.dir, "u1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "u1", "trades.json"), buf, 0o644); err != nil {
		t.Fatalf("seed legacy trades: %v", err)
	}
	// Re-open so the store reads the seeded file.
	store, err := openStore(e.dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	e.srv.store = store

	code, body := e.do(t, "u1", "GET", "/trades", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /trades: %d", code)
	}
	trades, _ := body["trades"].([]any)
	if len(trades) != 1 {
		t.Fatalf("legacy trade did not load: %v", body)
	}
	tr := trades[0].(map[string]any)
	if tr["thesis"] != "Blackwell ramp; bought the dip." {
		t.Errorf("legacy free-text thesis must be preserved verbatim, got %v", tr["thesis"])
	}
	if _, present := tr["thesisId"]; present {
		t.Error("a legacy trade must never gain a thesisId")
	}
	// A record with no mode is INTERPRETED as live, exactly as before this step: the stored field
	// stays absent (nothing rewrites old records) and effectiveMode does the work at read time.
	if tr["mode"] != "" {
		t.Errorf("a legacy trade's stored mode must not be back-filled, got %v", tr["mode"])
	}
	_, liveOnly := e.do(t, "u1", "GET", "/trades?mode=live", nil)
	if got, _ := liveOnly["trades"].([]any); len(got) != 1 {
		t.Errorf("a legacy trade must be counted as live, got %v", got)
	}
	_, paperOnly := e.do(t, "u1", "GET", "/trades?mode=paper", nil)
	if got, _ := paperOnly["trades"].([]any); len(got) != 0 {
		t.Errorf("a legacy trade must never be counted as paper, got %v", got)
	}

	// And it is not visible as a decision: the decision log is empty.
	_, list := e.do(t, "u1", "GET", "/decisions", nil)
	if d, _ := list["decisions"].([]any); len(d) != 0 {
		t.Errorf("no decision may be inferred from a legacy trade, got %v", d)
	}
}

// ---- provenance ----------------------------------------------------------------------------------

// The snapshot records the DATA state it was taken under, observed from the journal's own quote
// client rather than asserted by the caller — so a decision taken on synthetic prices says so
// forever. `signalPresent` records only whether a validated signal existed, never a direction.
func TestSnapshotPreservesSyntheticAndModelMetadata(t *testing.T) {
	analysis := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"symbol": "NVDA", "price": 170.0, "source": "synthetic"})
	}))
	defer analysis.Close()

	e := newTestEnv(t)
	e.srv.cfg.AnalysisURL = analysis.URL
	e.srv.http = &http.Client{Timeout: 2 * time.Second}

	thesisID := e.makeThesis(t, "u1", "NVDA", "Claim.")
	dec := createDecision(t, e, "u1", decisionPayload(thesisID, map[string]any{
		// The gateway supplies these; a client cannot state a direction, only presence + reason.
		"modelSignalPresent": false,
		"modelSignalReason":  "backtest gate not passed",
	}))
	snap := snapshotOf(t, dec)
	if snap["dataState"] != "synthetic" {
		t.Errorf("a decision taken on synthetic prices must record it, got %v", snap["dataState"])
	}
	mc, _ := snap["modelContext"].(map[string]any)
	if mc == nil {
		t.Fatalf("snapshot has no modelContext: %v", snap)
	}
	if mc["priceIsSynthetic"] != true || mc["priceSource"] != "synthetic" {
		t.Errorf("price provenance not captured: %v", mc)
	}
	if mc["signalPresent"] != false || mc["signalReason"] != "backtest gate not passed" {
		t.Errorf("signal context not captured: %v", mc)
	}
	// A direction is not representable, by construction.
	for _, banned := range []string{"direction", "probUp", "verdict", "recommendation", "target"} {
		if _, present := mc[banned]; present {
			t.Errorf("modelContext must not carry %q: %v", banned, mc)
		}
	}

	// With no reachable price service the state is "unknown" — never a guess, never a default of live.
	e.srv.cfg.AnalysisURL = "http://127.0.0.1:1"
	dec2 := createDecision(t, e, "u1", decisionPayload(thesisID, nil))
	snap2 := snapshotOf(t, dec2)
	if snap2["dataState"] != "unknown" {
		t.Errorf("an unknown price source must read as unknown, got %v", snap2["dataState"])
	}
	if snap2["modelContext"].(map[string]any)["signalReason"] != "not recorded" {
		t.Errorf("an absent signal context must say so, got %v", snap2["modelContext"])
	}
}

// ---- AI assistance boundary ----------------------------------------------------------------------

// An optional AI summary is a READ. There is no endpoint through which a model-authored value can
// reach a user-authored field, and a client that tries — by any field name — is refused. This is the
// journal half; the gateway half (a summary may not cite an unknown id) is in gateway/decisions_test.go.
func TestModelOutputCannotWriteUserFields(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "Claim.")
	dec := createDecision(t, e, "u1", decisionPayload(thesisID, nil))
	code, body := e.do(t, "u1", "POST", "/outcomes", map[string]any{
		"decisionId": dec["id"], "observedOutcome": "Mine.", "reasoningQuality": 3,
		"retrospective": "Mine too.",
	})
	if code != http.StatusCreated {
		t.Fatalf("seed outcome: %d %v", code, body)
	}
	out := body["outcome"].(map[string]any)

	// Anything model-shaped a client might try to attach is either refused or simply not a field.
	for _, attempt := range []map[string]any{
		{"reasoningQuality": 5, "modelUsed": "qwen2.5:7b"}, // extra key ignored, user value still user-authored
		{"summary": "The model thinks you did well."},
		{"aiRetrospective": "Generated."},
	} {
		code, _ := e.do(t, "u1", "PATCH", "/outcomes/"+out["id"].(string), attempt)
		if code != http.StatusOK && code != http.StatusBadRequest {
			t.Errorf("unexpected status for %v: %d", attempt, code)
		}
	}
	_, after := e.do(t, "u1", "GET", "/outcomes/"+out["id"].(string), nil)
	got := after["outcome"].(map[string]any)
	if _, present := got["modelUsed"]; present {
		t.Errorf("a model attribution must not be storable on a user's review: %v", got)
	}
	if _, present := got["summary"]; present {
		t.Errorf("model text must not be storable on a user's review: %v", got)
	}
	if got["retrospective"] != "Mine too." {
		t.Errorf("the user's retrospective was overwritten: %v", got["retrospective"])
	}
	// The decision's own rationale is likewise unreachable.
	code, body = e.do(t, "u1", "PATCH", "/decisions/"+dec["id"].(string), map[string]any{
		"rationale": "Summarised by the assistant.",
	})
	if code != http.StatusBadRequest || body["code"] != "immutable_field" {
		t.Errorf("a rationale must not be model-writable, got %d %v", code, body)
	}
}

// ---- lifecycle -----------------------------------------------------------------------------------

// Deleting a decision takes its review with it: a review of a decision that no longer exists is a
// dangling reference, not history.
func TestDeleteDecisionCascadesItsReview(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "Claim.")
	dec := createDecision(t, e, "u1", decisionPayload(thesisID, nil))
	code, body := e.do(t, "u1", "POST", "/outcomes", map[string]any{
		"decisionId": dec["id"], "observedOutcome": "Done.",
	})
	if code != http.StatusCreated {
		t.Fatalf("seed outcome: %d %v", code, body)
	}

	code, body = e.do(t, "u1", "DELETE", "/decisions/"+dec["id"].(string), nil)
	if code != http.StatusOK {
		t.Fatalf("DELETE: %d %v", code, body)
	}
	if body["outcomesRemoved"].(float64) != 1 {
		t.Errorf("the review should have been cascaded, got %v", body["outcomesRemoved"])
	}
	_, list := e.do(t, "u1", "GET", "/outcomes", nil)
	if o, _ := list["outcomes"].([]any); len(o) != 0 {
		t.Errorf("orphaned review survived: %v", o)
	}
}

// Records survive a restart, and the list reports which decisions have been reviewed — the numerator
// of this step's metric.
func TestDecisionsPersistAndReportReviewCoverage(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "Claim.")
	reviewed := createDecision(t, e, "u1", decisionPayload(thesisID, nil))
	unreviewed := createDecision(t, e, "u1", decisionPayload(thesisID, map[string]any{
		"intent": "wait", "action": map[string]any{"kind": "none"},
	}))
	code, _ := e.do(t, "u1", "POST", "/outcomes", map[string]any{
		"decisionId": reviewed["id"], "observedOutcome": "Reviewed.",
	})
	if code != http.StatusCreated {
		t.Fatalf("seed outcome: %d", code)
	}

	// Fresh store instances over the same directory — the restart case.
	fresh := &testEnv{srv: e.srv, dir: e.dir}
	store, err := openDecisionStore(e.dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	api := &decisionAPI{srv: e.srv, store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /decisions", e.srv.optionalAuth(api.handleList))
	fresh.handler = mux

	_, list := fresh.do(t, "u1", "GET", "/decisions", nil)
	decisions, _ := list["decisions"].([]any)
	if len(decisions) != 2 {
		t.Fatalf("decisions did not survive a restart: %v", list)
	}
	ids, _ := list["reviewedDecisionIds"].([]any)
	if len(ids) != 1 || ids[0] != reviewed["id"] {
		t.Errorf("review coverage wrong: %v (unreviewed=%v)", ids, unreviewed["id"])
	}
}
