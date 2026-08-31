package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// onboarding_test.go — the guided first session and the events it authorises (Step 11).
//
// Everything here runs with NO network, NO model and NO API key: onboarding state, activation
// verification, and the event log are all journal-local files (invariant #1).
//
// The properties under test are the ones that decide whether the activation metric means anything:
//
//   - completion requires DURABLE records, not a client saying it finished;
//   - a model-derived "evidence" record can never satisfy the evidence condition;
//   - progress survives leaving and coming back, at every step;
//   - a guest can start, and can never activate;
//   - no event ever carries a claim, an excerpt, a note, a rationale, or a raw uid.

// ---- harness ----

// newMetricsEnv is newTestEnv plus a live analytics sink pointed at the same temp dir, wired BEFORE
// routes are registered (registration is what hands the sink its level source). The sink is a
// process-wide handle, so it is reset afterwards and no other lane's test inherits it.
func newMetricsEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	analytics = openAnalyticsSink(dir, "test-salt", []string{"demo-user"})
	t.Cleanup(func() { analytics = nil })

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

// readEvents returns every event written so far, oldest first.
func readEvents(t *testing.T, dir string) []analyticsEvent {
	t.Helper()
	events, err := loadAnalyticsEvents(filepath.Join(dir, analyticsDirName))
	if err != nil {
		t.Fatalf("loadAnalyticsEvents: %v", err)
	}
	return events
}

// rawEventLines returns the log verbatim — used by the privacy test, which must inspect the BYTES
// that were written rather than a struct that could have dropped a field on decode.
func rawEventLines(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	entries, err := os.ReadDir(filepath.Join(dir, analyticsDirName))
	if err != nil {
		return nil
	}
	for _, e := range entries {
		f, err := os.Open(filepath.Join(dir, analyticsDirName, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				out = append(out, line)
			}
		}
		f.Close()
	}
	return out
}

func eventsNamed(events []analyticsEvent, name string) []analyticsEvent {
	var out []analyticsEvent
	for _, ev := range events {
		if ev.Event == name {
			out = append(out, ev)
		}
	}
	return out
}

func hasEvent(events []analyticsEvent, name string) bool { return len(eventsNamed(events, name)) > 0 }

// newsEvidence is a complete, valid `news` capture with LIVE provenance. No network: the record is
// just a description of a source the user already saw.
func newsEvidence(url, title string) map[string]any {
	return map[string]any{
		"ticker": "NVDA", "kind": "news",
		"title":       title,
		"excerpt":     "Datacenter revenue guidance was raised for the third consecutive quarter.",
		"provider":    "marketaux",
		"sourceUrl":   url,
		"sourceTime":  time.Now().Add(-48 * time.Hour).Unix(),
		"dataState":   "live",
		"attribution": "Marketaux",
	}
}

// runOnboardingToActivation performs the whole guided sequence for uid and returns the thesis id.
func runOnboardingToActivation(t *testing.T, e *testEnv, uid string) string {
	t.Helper()
	post(t, e, uid, map[string]any{"step": stepChooseCompany, "ticker": "NVDA", "level": "standard"})
	post(t, e, uid, map[string]any{"step": stepOrientSnapshot})

	thesisID := e.makeThesis(t, uid, "NVDA", "Networking attach lifts blended ASPs through FY27.")
	post(t, e, uid, map[string]any{"step": stepWriteClaim, "thesisId": thesisID})

	code, body := e.do(t, uid, "PATCH", "/theses/"+thesisID, map[string]any{
		"invalidationConditions": []any{map[string]any{"text": "Attach rate falls below 40% for two quarters."}},
	})
	if code != http.StatusOK {
		t.Fatalf("PATCH thesis: %d %v", code, body)
	}
	post(t, e, uid, map[string]any{"step": stepNameCondition})

	rec := mustCreate(t, e, uid, newsEvidence("https://example.com/nvda-guidance", "NVDA raises datacenter guidance"))
	code, body = e.do(t, uid, "POST", "/evidence/links", map[string]any{
		"thesisId": thesisID, "evidenceId": rec["id"], "bearing": "supports",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /evidence/links: %d %v", code, body)
	}
	post(t, e, uid, map[string]any{"step": stepAttachEvidence})

	createReview(t, e, uid, thesisID)
	post(t, e, uid, map[string]any{"step": stepChallengeReview})
	return thesisID
}

func post(t *testing.T, e *testEnv, uid string, body map[string]any) map[string]any {
	t.Helper()
	code, resp := e.do(t, uid, "POST", "/onboarding", body)
	if code != http.StatusOK {
		t.Fatalf("POST /onboarding %v: got %d, body %v", body, code, resp)
	}
	ob, _ := resp["onboarding"].(map[string]any)
	return ob
}

func stateOf(ob map[string]any) map[string]any {
	st, _ := ob["state"].(map[string]any)
	return st
}

// ---- the load-bearing rule: completion is a fact about durable records ----

// A client cannot declare itself activated. Walking every guided step without creating anything
// leaves the record in progress, and no onboarding_completed event exists.
func TestOnboardingCompletionRequiresDurableRecords(t *testing.T) {
	e := newMetricsEnv(t)

	for _, step := range onboardingSteps {
		ob := post(t, e, "u1", map[string]any{"step": step, "ticker": "NVDA"})
		if got := stateOf(ob)["status"]; got != onboardingInProgress {
			t.Fatalf("after step %s status is %v, want in_progress", step, got)
		}
	}
	if hasEvent(readEvents(t, e.dir), evOnboardingCompleted) {
		t.Fatal("onboarding completed with no thesis, no evidence and no review")
	}

	// A thesis alone is not activation.
	id := e.makeThesis(t, "u1", "NVDA", "Networking attach lifts blended ASPs.")
	ob := post(t, e, "u1", map[string]any{"thesisId": id})
	if stateOf(ob)["status"] != onboardingInProgress {
		t.Fatal("a thesis on its own must not complete onboarding")
	}
	if ob["hasEvidenceLink"] != false || ob["hasChallenge"] != false {
		t.Fatalf("activation conditions misreported: %v", ob)
	}

	// Evidence linked, still no challenge review.
	rec := mustCreate(t, e, "u1", newsEvidence("https://example.com/a", "A"))
	if code, body := e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": id, "evidenceId": rec["id"], "bearing": "supports",
	}); code != http.StatusCreated {
		t.Fatalf("link: %d %v", code, body)
	}
	ob = post(t, e, "u1", map[string]any{"step": stepAttachEvidence})
	if stateOf(ob)["status"] != onboardingInProgress {
		t.Fatal("thesis + evidence without a challenge review must not complete onboarding")
	}

	// The challenge review closes it — and only then.
	createReview(t, e, "u1", id)
	ob = post(t, e, "u1", map[string]any{"step": stepCommitNext})
	if stateOf(ob)["status"] != onboardingCompleted {
		t.Fatalf("all three durable records exist but status is %v", stateOf(ob)["status"])
	}
	events := readEvents(t, e.dir)
	if got := len(eventsNamed(events, evOnboardingCompleted)); got != 1 {
		t.Fatalf("expected exactly 1 onboarding_completed, got %d", got)
	}
}

// A model-derived record is not source-backed evidence (§2.7), so it cannot satisfy activation.
func TestInferenceEvidenceDoesNotActivate(t *testing.T) {
	e := newMetricsEnv(t)
	id := e.makeThesis(t, "u1", "NVDA", "Networking attach lifts blended ASPs.")
	createReview(t, e, "u1", id)

	rec := mustCreate(t, e, "u1", map[string]any{
		"ticker": "NVDA", "kind": "user_note",
		"note":        "The skeptic lens thinks the attach assumption has no floor.",
		"provider":    "llm",
		"dataState":   "live",
		"attribution": "qwen2.5:7b via the skeptic lens",
		"inference":   true,
		"modelUsed":   "qwen2.5:7b",
		"sourceRef": map[string]any{
			"store": "journal-review", "id": "rev_x", "locator": "fnd_aaaa",
		},
	})
	if code, body := e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": id, "evidenceId": rec["id"], "bearing": "supports",
	}); code != http.StatusCreated {
		t.Fatalf("link: %d %v", code, body)
	}

	ob := post(t, e, "u1", map[string]any{"step": stepAttachEvidence})
	if stateOf(ob)["status"] == onboardingCompleted {
		t.Fatal("model-derived evidence satisfied the source-backed evidence condition")
	}
	if ob["hasEvidenceLink"] != false {
		t.Fatalf("an inference record was counted as source-backed evidence: %v", ob)
	}
}

// Activation reached OUTSIDE the guide still activates: a user who did the work in Research and never
// touched the guidance is activated, and the read path is what notices.
func TestActivationDetectedWithoutTouchingTheGuide(t *testing.T) {
	e := newMetricsEnv(t)
	post(t, e, "u1", map[string]any{"step": stepChooseCompany, "ticker": "NVDA"})

	id := e.makeThesis(t, "u1", "NVDA", "Networking attach lifts blended ASPs.")
	rec := mustCreate(t, e, "u1", newsEvidence("https://example.com/b", "B"))
	e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": id, "evidenceId": rec["id"], "bearing": "contradicts",
	})
	createReview(t, e, "u1", id)

	code, body := e.do(t, "u1", "GET", "/onboarding", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /onboarding: %d", code)
	}
	ob, _ := body["onboarding"].(map[string]any)
	if stateOf(ob)["status"] != onboardingCompleted {
		t.Fatalf("activation outside the guide was not detected: %v", ob)
	}
	act, _ := stateOf(ob)["activation"].(map[string]any)
	if act == nil || act["thesisId"] != id || act["evidenceId"] != rec["id"] {
		t.Fatalf("activation proof does not name the durable records: %v", act)
	}
}

// ---- resumability ----

// Interrupting at each step and coming back preserves the step and the completed set, across a full
// restart of the store (nothing lives only in memory).
func TestOnboardingResumesAtEveryStep(t *testing.T) {
	e := newMetricsEnv(t)

	for i, step := range onboardingSteps {
		post(t, e, "u1", map[string]any{"step": step, "ticker": "NVDA"})

		// "Leaving": a brand-new server over the same directory, as a restart would be.
		store, _ := openStore(e.dir)
		theses, _ := openThesisStore(e.dir)
		fresh := &testEnv{dir: e.dir}
		fresh.srv = &Server{cfg: e.srv.cfg, store: store, theses: theses, http: &http.Client{}}
		fresh.handler = fresh.srv.routes()

		code, body := fresh.do(t, "u1", "GET", "/onboarding", nil)
		if code != http.StatusOK {
			t.Fatalf("resume at %s: %d", step, code)
		}
		st := stateOf(body["onboarding"].(map[string]any))
		if st["step"] != step {
			t.Fatalf("resumed at %v, expected %s", st["step"], step)
		}
		done, _ := st["completedSteps"].([]any)
		if len(done) != i+1 {
			t.Fatalf("after %s expected %d completed steps, got %v", step, i+1, done)
		}
		e = fresh
	}
}

// A long pause is a RESUME, and a resume is one event — not one per request.
func TestResumeAfterAGapEmitsExactlyOneResume(t *testing.T) {
	e := newMetricsEnv(t)
	post(t, e, "u1", map[string]any{"step": stepChooseCompany, "ticker": "NVDA"})

	events := readEvents(t, e.dir)
	if got := len(eventsNamed(events, evOnboardingStarted)); got != 1 {
		t.Fatalf("expected 1 onboarding_started, got %d", got)
	}
	if hasEvent(events, evOnboardingResumed) {
		t.Fatal("a first write must not be a resume")
	}

	// Continuing inside the window is not a resume.
	post(t, e, "u1", map[string]any{"step": stepOrientSnapshot})
	if hasEvent(readEvents(t, e.dir), evOnboardingResumed) {
		t.Fatal("continuing in the same sitting counted as a resume")
	}

	// Backdate the record past the gap, as leaving for an hour would.
	path := filepath.Join(e.dir, "u1", onboardingFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read onboarding.json: %v", err)
	}
	var st OnboardingState
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("parse onboarding.json: %v", err)
	}
	st.LastActiveAt = time.Now().Unix() - onboardingResumeGapSec - 60
	nb, _ := json.Marshal(st)
	if err := os.WriteFile(path, nb, 0o644); err != nil {
		t.Fatalf("write onboarding.json: %v", err)
	}

	// Fresh server so the backdated file is read rather than the cached copy.
	store, _ := openStore(e.dir)
	theses, _ := openThesisStore(e.dir)
	fresh := &testEnv{dir: e.dir}
	fresh.srv = &Server{cfg: e.srv.cfg, store: store, theses: theses, http: &http.Client{}}
	fresh.handler = fresh.srv.routes()

	post(t, fresh, "u1", map[string]any{"step": stepWriteClaim})
	if got := len(eventsNamed(readEvents(t, e.dir), evOnboardingResumed)); got != 1 {
		t.Fatalf("expected exactly 1 onboarding_resumed, got %d", got)
	}
}

// ---- guest identity ----

// A guest can start onboarding and is attributed by a HASHED session id — never the raw value, never
// a durable record, and never an activation event (every activation write requires auth).
func TestGuestOnboardingStartsButCannotActivate(t *testing.T) {
	e := newMetricsEnv(t)

	code, body := e.do(t, "", "POST", "/onboarding", map[string]any{
		"sessionId": "guestsession123456", "step": stepChooseCompany, "ticker": "NVDA",
	})
	if code != http.StatusOK {
		t.Fatalf("guest start: %d %v", code, body)
	}
	ob, _ := body["onboarding"].(map[string]any)
	if ob["guest"] != true {
		t.Fatalf("guest response not marked as guest: %v", ob)
	}

	// No guest partition was created — a guest has no durable research record anywhere.
	if _, err := os.Stat(filepath.Join(e.dir, "guestsession123456")); !os.IsNotExist(err) {
		t.Fatal("a guest session created a durable partition")
	}

	events := readEvents(t, e.dir)
	started := eventsNamed(events, evOnboardingStarted)
	if len(started) != 1 {
		t.Fatalf("expected 1 guest onboarding_started, got %d", len(started))
	}
	if started[0].Actor.Type != "guest" {
		t.Fatalf("guest event attributed as %q", started[0].Actor.Type)
	}
	if started[0].Actor.IDHash == "guestsession123456" || len(started[0].Actor.IDHash) != 16 {
		t.Fatalf("guest identity is not a 16-char hash: %q", started[0].Actor.IDHash)
	}

	// The resume variant is the only other event a guest can produce.
	e.do(t, "", "POST", "/onboarding", map[string]any{
		"sessionId": "guestsession123456", "step": stepOrientSnapshot, "resumed": true,
	})
	events = readEvents(t, e.dir)
	for _, ev := range events {
		if ev.Actor.Type != "guest" {
			continue
		}
		if ev.Event != evOnboardingStarted && ev.Event != evOnboardingResumed {
			t.Fatalf("a guest emitted %q, which §6.3 does not permit", ev.Event)
		}
	}

	// A guest write with no session id is a 400 rather than an unattributable event.
	if code, _ := e.do(t, "", "POST", "/onboarding", map[string]any{"step": stepChooseCompany}); code != http.StatusBadRequest {
		t.Fatalf("guest write without a session id returned %d, want 400", code)
	}
}

// The guest -> signup handoff: the account's own first state write is its own onboarding_started, and
// the guest's events are never attributed to the account.
func TestGuestThenSignupKeepsIdentitiesSeparate(t *testing.T) {
	e := newMetricsEnv(t)
	e.do(t, "", "POST", "/onboarding", map[string]any{"sessionId": "sess0000", "step": stepChooseCompany})
	post(t, e, "u1", map[string]any{"step": stepChooseCompany, "ticker": "NVDA"})

	started := eventsNamed(readEvents(t, e.dir), evOnboardingStarted)
	if len(started) != 2 {
		t.Fatalf("expected a guest start and an account start, got %d", len(started))
	}
	var guest, user analyticsEvent
	for _, ev := range started {
		if ev.Actor.Type == "guest" {
			guest = ev
		} else {
			user = ev
		}
	}
	if guest.Actor.IDHash == "" || user.Actor.IDHash == "" {
		t.Fatalf("both identities must be present: %+v %+v", guest, user)
	}
	if guest.Actor.IDHash == user.Actor.IDHash {
		t.Fatal("guest activity was attributed to the signed-in account")
	}
	if guest.EventID == user.EventID {
		t.Fatal("guest and account starts collapsed into one event id")
	}
}

// ---- the whole loop, and what it writes ----

// The full research loop emits each catalog event exactly where its durable write happened, and the
// event log contains none of the user's research text.
func TestFullLoopEmitsCatalogEventsAndNoPrivateText(t *testing.T) {
	e := newMetricsEnv(t)
	thesisID := runOnboardingToActivation(t, e, "u1")

	// Decide and review the outcome, closing the loop.
	code, body := e.do(t, "u1", "POST", "/decisions", map[string]any{
		"ticker": "NVDA", "thesisId": thesisID, "intent": "wait",
		"action":    map[string]any{"kind": "none"},
		"rationale": "Waiting for the next attach-rate datapoint before sizing anything.",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /decisions: %d %v", code, body)
	}
	dec, _ := body["decision"].(map[string]any)
	code, body = e.do(t, "u1", "POST", "/outcomes", map[string]any{
		"decisionId": dec["id"], "observedOutcome": "Attach rate held; I never sized it.",
		"reasoningQuality": 4, "retrospective": "Waiting was right for the wrong reason.",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /outcomes: %d %v", code, body)
	}

	// Rate the review and accept one citation.
	code, listBody := e.do(t, "u1", "GET", "/reviews", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /reviews: %d", code)
	}
	revs, _ := listBody["reviews"].([]any)
	if len(revs) == 0 {
		t.Fatal("no saved review to rate")
	}
	rev := revs[0].(map[string]any)
	if code, b := e.do(t, "u1", "PATCH", "/reviews/"+rev["id"].(string), map[string]any{
		"rating":           "useful",
		"citationFeedback": []any{map[string]any{"findingId": "fnd_bbbb", "accepted": true}},
	}); code != http.StatusOK {
		t.Fatalf("PATCH /reviews: %d %v", code, b)
	}

	// Mark the thesis reviewed, then close it.
	if code, b := e.do(t, "u1", "POST", "/theses/"+thesisID+"/review-checkpoint", map[string]any{
		"itemCount": 3, "source": "user",
	}); code != http.StatusOK {
		t.Fatalf("review-checkpoint: %d %v", code, b)
	}
	if code, b := e.do(t, "u1", "PATCH", "/theses/"+thesisID, map[string]any{
		"status": "closed", "closeReason": "played_out",
	}); code != http.StatusOK {
		t.Fatalf("close thesis: %d %v", code, b)
	}

	events := readEvents(t, e.dir)
	for _, want := range []string{
		evOnboardingStarted, evOnboardingCompleted, evThesisCreated, evThesisActivated,
		evThesisClosed, evEvidenceSaved, evEvidenceLinked, evChallengeReviewCompleted,
		evThesisReviewed, evDecisionRecorded, evOutcomeReviewed, evReviewRated, evCitationFeedback,
	} {
		if !hasEvent(events, want) {
			t.Errorf("missing event %q from the durable loop", want)
		}
	}

	// Every event name must be in the closed catalog of 14.
	for _, ev := range events {
		if _, ok := analyticsCatalog[ev.Event]; !ok {
			t.Errorf("event %q is not in the closed catalog", ev.Event)
		}
	}

	// PRIVACY: the raw log must contain no research text, no raw uid, and no source URL.
	forbidden := []string{
		"Networking attach lifts blended ASPs",       // thesis claim
		"Attach rate falls below 40%",                // invalidation condition text
		"Datacenter revenue guidance was raised",     // evidence excerpt
		"NVDA raises datacenter guidance",            // evidence title
		"https://example.com/nvda-guidance",          // source url
		"Waiting for the next attach-rate datapoint", // decision rationale
		"Attach rate held; I never sized it",         // outcome observation
		"Waiting was right for the wrong reason",     // retrospective
		"The attach-rate assumption has no floor",    // review finding
		"What is the signed commitment?",             // review open question
		"\"u1\"",                                     // raw uid
		"nvda_session",                               // cookie name
	}
	for _, line := range rawEventLines(t, e.dir) {
		for _, bad := range forbidden {
			if strings.Contains(line, bad) {
				t.Errorf("event log leaked %q:\n%s", bad, line)
			}
		}
	}
}

// ---- durable-write authority ----

// A REJECTED write emits nothing. The event follows the durable record, not the request.
func TestRejectedWritesEmitNothing(t *testing.T) {
	e := newMetricsEnv(t)

	// An invalid thesis (no claim) is a 400.
	if code, _ := e.do(t, "u1", "POST", "/theses", map[string]any{"ticker": "NVDA"}); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a claimless thesis, got %d", code)
	}
	// An evidence save with incomplete provenance is a 400.
	if code, _ := e.do(t, "u1", "POST", "/evidence", map[string]any{
		"ticker": "NVDA", "kind": "news", "title": "no url, no provider",
	}); code != http.StatusBadRequest {
		t.Fatal("expected 400 for incomplete provenance")
	}
	if evs := readEvents(t, e.dir); len(evs) != 0 {
		t.Fatalf("rejected writes emitted %d events: %+v", len(evs), evs)
	}

	// A dedupe hit writes no new record, so it emits no evidence_saved (§6.2).
	e.makeThesis(t, "u1", "NVDA", "Networking attach lifts blended ASPs.")
	payload := newsEvidence("https://example.com/dupe", "Dupe")
	mustCreate(t, e, "u1", payload)
	code, body := e.do(t, "u1", "POST", "/evidence", payload)
	if code != http.StatusOK || body["deduplicated"] != true {
		t.Fatalf("second identical save: %d %v", code, body)
	}
	if got := len(eventsNamed(readEvents(t, e.dir), evEvidenceSaved)); got != 1 {
		t.Fatalf("a dedupe hit was counted as a new capture (%d evidence_saved events)", got)
	}
}

// A replayed identical transition produces the SAME event id, so the reader collapses it (§6.3).
func TestReplayedTransitionsDeduplicate(t *testing.T) {
	e := newMetricsEnv(t)
	id := e.makeThesis(t, "u1", "NVDA", "Networking attach lifts blended ASPs.")
	rec := mustCreate(t, e, "u1", newsEvidence("https://example.com/c", "C"))

	for i := 0; i < 3; i++ {
		if code, b := e.do(t, "u1", "POST", "/evidence/links", map[string]any{
			"thesisId": id, "evidenceId": rec["id"], "bearing": "supports",
		}); code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("link %d: %d %v", i, code, b)
		}
	}

	linked := eventsNamed(readEvents(t, e.dir), evEvidenceLinked)
	if len(linked) != 3 {
		t.Fatalf("expected 3 raw link events, got %d", len(linked))
	}
	ids := map[string]bool{}
	for _, ev := range linked {
		ids[ev.EventID] = true
	}
	if len(ids) != 1 {
		t.Fatalf("the same link/bearing produced %d distinct event ids", len(ids))
	}

	// The readout de-duplicates them back to one.
	r := buildReadout(readEvents(t, e.dir), readoutOptions{Now: time.Now()})
	if r.EventCounts[evEvidenceLinked] != 1 {
		t.Fatalf("readout counted %d evidence_linked, want 1", r.EventCounts[evEvidenceLinked])
	}
	if r.DuplicateEvents != 2 {
		t.Fatalf("readout reported %d duplicates, want 2", r.DuplicateEvents)
	}

	// Changing the bearing IS a new transition.
	if code, b := e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": id, "evidenceId": rec["id"], "bearing": "contradicts",
	}); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("re-bearing: %d %v", code, b)
	}
	r = buildReadout(readEvents(t, e.dir), readoutOptions{Now: time.Now()})
	if r.EventCounts[evEvidenceLinked] != 2 {
		t.Fatalf("a bearing change did not register: %d", r.EventCounts[evEvidenceLinked])
	}
}

// ---- synthetic / demo exclusion ----

// Evidence captured from SYNTHETIC prices is flagged and excluded from the default readout; a
// server-flagged demo account is excluded whatever it does.
func TestSyntheticAndDemoActivityExcluded(t *testing.T) {
	e := newMetricsEnv(t)

	// A real user, on synthetic bars (the zero-key path).
	id := e.makeThesis(t, "u1", "NVDA", "Momentum is extended on the daily.")
	rec := mustCreate(t, e, "u1", deterministicFact("2026-07-29"))
	e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": id, "evidenceId": rec["id"], "bearing": "context",
	})

	// The demo account, on LIVE data — flagged anyway, because the account is the demo.
	demoID := e.makeThesis(t, "demo-user", "NVDA", "Demo thesis for the walkthrough.")
	demoRec := mustCreate(t, e, "demo-user", newsEvidence("https://example.com/demo", "Demo"))
	e.do(t, "demo-user", "POST", "/evidence/links", map[string]any{
		"thesisId": demoID, "evidenceId": demoRec["id"], "bearing": "supports",
	})

	events := readEvents(t, e.dir)
	for _, ev := range events {
		switch {
		case ev.Event == evEvidenceSaved && ev.DataState != nil && *ev.DataState == "synthetic":
			if !ev.Synthetic {
				t.Error("a synthetic-data capture was not flagged synthetic")
			}
		}
	}
	for _, ev := range events {
		if ev.Actor.IDHash == analytics.hashIdentity("demo-user") && !ev.Synthetic {
			t.Errorf("demo-account event %q was not flagged for exclusion", ev.Event)
		}
	}

	excluded := buildReadout(events, readoutOptions{Now: time.Now()})
	included := buildReadout(events, readoutOptions{Now: time.Now(), IncludeSynthetic: true})
	if excluded.SyntheticEvents == 0 {
		t.Fatal("no synthetic/demo events were identified")
	}
	if excluded.CountedEvents >= included.CountedEvents {
		t.Fatalf("the default readout did not exclude anything (%d vs %d)",
			excluded.CountedEvents, included.CountedEvents)
	}
	if excluded.EventCounts[evEvidenceSaved] != 0 {
		t.Fatalf("synthetic captures survived the default readout: %d", excluded.EventCounts[evEvidenceSaved])
	}
}

// ---- the store never touches the unattributed legacy bucket ----

func TestLegacyBucketIsNeverMeasured(t *testing.T) {
	e := newMetricsEnv(t)
	analytics.emit(eventInput{
		Event: evThesisCreated, UID: legacyBucket, TransitionKey: "created",
		ObjectID: "abc0000000000000", Ticker: "NVDA",
	})
	if evs := readEvents(t, e.dir); len(evs) != 0 {
		t.Fatalf("_legacy activity produced %d events", len(evs))
	}
}
