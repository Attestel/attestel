package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// changes_test.go — the deterministic change set (PARALLEL_CONTRACTS.md §4.4) and its optional
// summary.
//
// What these tests pin, in order of importance:
//
//   · the checkpoint is the boundary, and "strictly after" means strictly after;
//   · a thesis that was never reviewed falls back to its creation time and SAYS so (basis);
//   · no changes produces an HONEST empty result — never a filler narrative;
//   · change ids are deterministic, so the same boundary always yields the same set;
//   · the optional summary CANNOT introduce a change id that is not in the set;
//   · a foreign thesis is a 404 and nothing is computed.

const chgThesisID = "9f2c1b7a4e0d5a83"

// changeStubs stands in for the journal, alerts, analysis and llm services.
type changeStubs struct {
	journal, alerts, analysis, llm *httptest.Server

	thesis   map[string]any
	links    []any
	reviews  []any
	events   []any
	llmReply string
	llmCalls int
}

func newChangeStubs(t *testing.T) *changeStubs {
	t.Helper()
	s := &changeStubs{
		thesis: map[string]any{
			"id": chgThesisID, "ticker": "NVDA", "claim": "Networking attach lifts blended ASPs.",
			"createdAt": 1_700_000_000, "versions": []any{},
			"lastReview": map[string]any{
				"checkpointId": "chk_71e0a9c4b2d8f356", "at": 1_750_000_000,
				"itemCount": 7, "source": "user",
			},
		},
		links: []any{}, reviews: []any{}, events: []any{},
		llmReply: "Nothing notable.",
	}

	s.journal = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/theses/"):
			if strings.TrimPrefix(r.URL.Path, "/theses/") != chgThesisID {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "thesis not found"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"thesis": s.thesis})
		case strings.HasPrefix(r.URL.Path, "/evidence/links"):
			_ = json.NewEncoder(w).Encode(map[string]any{"links": s.links})
		case strings.HasPrefix(r.URL.Path, "/reviews"):
			_ = json.NewEncoder(w).Encode(map[string]any{"reviews": s.reviews})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "not found"})
		}
	}))

	s.alerts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"events": s.events, "unread": 0})
	}))

	// A flat, dateless series: no material market move, so market_context contributes nothing
	// unless a test deliberately makes it.
	s.analysis = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"source": "tiingo", "sourceIsSynthetic": false,
			"price":      map[string]any{"ohlcv": []any{}},
			"indicators": map[string]any{"series": map[string]any{}},
		})
	}))

	s.llm = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/transcript/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"analyses": []any{}})
			return
		}
		s.llmCalls++
		_, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"reply": s.llmReply, "modelUsed": "qwen2.5:7b"})
	}))

	t.Cleanup(func() {
		s.journal.Close()
		s.alerts.Close()
		s.analysis.Close()
		s.llm.Close()
	})
	return s
}

func newChangeGateway(t *testing.T, st *changeStubs) (*Server, http.Handler) {
	t.Helper()
	cfg := loadConfig()
	cfg.JournalURL = st.journal.URL
	cfg.AlertsURL = st.alerts.URL
	cfg.AnalysisURL = st.analysis.URL
	cfg.LLMURL = st.llm.URL
	cfg.Secret = "test-secret"
	cfg.CookieName = "nvda_session"
	srv := &Server{cfg: cfg, cache: newCache(), http: &http.Client{Timeout: 5 * time.Second}}
	mux := http.NewServeMux()
	srv.registerChangeRoutes(mux)
	return srv, mux
}

func getChanges(t *testing.T, srv *Server, mux http.Handler, uid, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if uid != "" {
		req.Header.Set("Cookie", gatewaySession(srv, uid))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// ---------------------------------------------------------------- boundary + empty

// TestNoChangesIsHonestlyEmpty — §4.4. An empty answer is the feature; a filler narrative would
// teach the user to distrust the non-empty ones.
func TestNoChangesIsHonestlyEmpty(t *testing.T) {
	st := newChangeStubs(t)
	srv, mux := newChangeGateway(t, st)

	code, body := getChanges(t, srv, mux, "alice", "/api/theses/"+chgThesisID+"/changes")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}
	if empty, _ := body["empty"].(bool); !empty {
		t.Errorf("with nothing changed, empty must be true: %v", body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 0 {
		t.Errorf("want no items, got %v", items)
	}
	if items == nil {
		t.Error("items must serialize as [] rather than null")
	}
}

// TestCheckpointIsTheBoundary — only changes strictly after the checkpoint count.
func TestCheckpointIsTheBoundary(t *testing.T) {
	st := newChangeStubs(t)
	const cp = 1_750_000_000
	st.thesis["versions"] = []any{
		map[string]any{"id": "ver_before", "at": cp - 100, "actor": "user", "changedFields": []any{"claim"}},
		map[string]any{"id": "ver_at", "at": cp, "actor": "user", "changedFields": []any{"claim"}},
		map[string]any{"id": "ver_after", "at": cp + 100, "actor": "user", "changedFields": []any{"confidence"}},
	}
	srv, mux := newChangeGateway(t, st)

	_, body := getChanges(t, srv, mux, "alice", "/api/theses/"+chgThesisID+"/changes")
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("only the version AFTER the checkpoint counts, got %d: %v", len(items), body)
	}
	got := items[0].(map[string]any)
	if ref, _ := got["sourceRef"].(map[string]any); ref["id"] != "ver_after" {
		t.Errorf("wrong version selected: %v", ref)
	}
	since, _ := body["since"].(map[string]any)
	if since["basis"] != "checkpoint" || int64(since["at"].(float64)) != cp {
		t.Errorf("since must report the checkpoint boundary, got %v", since)
	}
}

// TestNeverReviewedFallsBackToCreation — §1.8. A thesis with no checkpoint compares against its
// creation time and labels the basis so the UI can say "since you created this thesis".
func TestNeverReviewedFallsBackToCreation(t *testing.T) {
	st := newChangeStubs(t)
	st.thesis["lastReview"] = nil
	st.thesis["createdAt"] = 1_700_000_000
	st.thesis["versions"] = []any{
		map[string]any{"id": "ver_1", "at": 1_700_000_500, "actor": "user", "changedFields": []any{"claim"}},
	}
	srv, mux := newChangeGateway(t, st)

	_, body := getChanges(t, srv, mux, "alice", "/api/theses/"+chgThesisID+"/changes")
	since, _ := body["since"].(map[string]any)
	if since["basis"] != "created" {
		t.Errorf("an unreviewed thesis must report basis=created, got %v", since)
	}
	if since["checkpointId"] != "" {
		t.Errorf("there is no checkpoint to name, got %v", since["checkpointId"])
	}
	if items, _ := body["items"].([]any); len(items) != 1 {
		t.Errorf("the edit after creation must be listed, got %v", items)
	}
}

// ---------------------------------------------------------------- determinism

// TestChangeIDsAreDeterministic — the same boundary must always yield the same ids. This is what
// makes the set testable and what lets Step 09 reference a change item without this lane persisting.
func TestChangeIDsAreDeterministic(t *testing.T) {
	st := newChangeStubs(t)
	st.thesis["versions"] = []any{
		map[string]any{"id": "ver_a", "at": 1_750_000_100, "actor": "user", "changedFields": []any{"claim"}},
	}
	st.links = []any{map[string]any{
		"id": "lnk_1111111111111111", "thesisId": chgThesisID, "evidenceId": "ev_1",
		"bearing": "contradicts", "thesisItemId": "asm_1111111111111111",
		"linkedAt": 1_750_000_200, "authoredBy": "user", "revoked": false,
	}}
	srv, mux := newChangeGateway(t, st)

	idsOf := func() []string {
		_, body := getChanges(t, srv, mux, "alice", "/api/theses/"+chgThesisID+"/changes")
		items, _ := body["items"].([]any)
		out := make([]string, 0, len(items))
		for _, it := range items {
			out = append(out, it.(map[string]any)["id"].(string))
		}
		return out
	}
	first, second := idsOf(), idsOf()
	if len(first) != 2 {
		t.Fatalf("want 2 items, got %d", len(first))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("change ids must be stable across recomputes: %v vs %v", first, second)
		}
		if !strings.HasPrefix(first[i], "chg_") {
			t.Errorf("change id must carry the chg_ prefix, got %q", first[i])
		}
	}
}

// TestUnlinkIsItsOwnChange — a revoked link is `evidence_unlinked` and asserts no bearing. It only
// appears because the collector asks for includeRevoked=true.
func TestUnlinkIsItsOwnChange(t *testing.T) {
	st := newChangeStubs(t)
	st.links = []any{map[string]any{
		"id": "lnk_1111111111111111", "thesisId": chgThesisID, "evidenceId": "ev_1",
		"bearing": "supports", "linkedAt": 1_700_000_000, // BEFORE the checkpoint
		"authoredBy": "user", "revoked": true, "revokedAt": 1_750_000_300, // after
	}}
	srv, mux := newChangeGateway(t, st)

	_, body := getChanges(t, srv, mux, "alice", "/api/theses/"+chgThesisID+"/changes")
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("only the unlink is after the checkpoint, got %d: %v", len(items), body)
	}
	got := items[0].(map[string]any)
	if got["kind"] != "evidence_unlinked" {
		t.Errorf("want evidence_unlinked, got %v", got["kind"])
	}
	if got["bearing"] != nil {
		t.Errorf("removing a link asserts nothing about the thesis, got bearing %v", got["bearing"])
	}
}

// TestSystemAuthoredLinkIsMarked — D-10's (d) marking must survive into the change set.
func TestSystemAuthoredLinkIsMarked(t *testing.T) {
	st := newChangeStubs(t)
	st.links = []any{map[string]any{
		"id": "lnk_2222222222222222", "thesisId": chgThesisID, "evidenceId": "ev_2",
		"bearing": "contradicts", "linkedAt": 1_750_000_400,
		"authoredBy": "system", "revoked": false,
	}}
	srv, mux := newChangeGateway(t, st)

	_, body := getChanges(t, srv, mux, "alice", "/api/theses/"+chgThesisID+"/changes")
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %v", items)
	}
	summary, _ := items[0].(map[string]any)["summary"].(string)
	if !strings.Contains(summary, "automatically") {
		t.Errorf("a system-authored link must be distinguishable from a user-authored one, got %q", summary)
	}
}

// TestAlertEventsAreDedupedAndScoped — repeat firings in one cooldown window collapse (§4.2), and
// an event for a different thesis never enters this set.
func TestAlertEventsAreDedupedAndScoped(t *testing.T) {
	st := newChangeStubs(t)
	st.events = []any{
		map[string]any{"id": "e1", "ts": 1_750_000_500, "message": "NVDA crossed below 100.00",
			"thesisId": chgThesisID, "thesisItemId": "inv_1111111111111111",
			"bearing": "weakens", "dataState": "live", "dedupeKey": "same-window"},
		map[string]any{"id": "e2", "ts": 1_750_000_600, "message": "NVDA crossed below 100.00",
			"thesisId": chgThesisID, "bearing": "weakens", "dataState": "live", "dedupeKey": "same-window"},
		map[string]any{"id": "e3", "ts": 1_750_000_700, "message": "other thesis",
			"thesisId": "ffffffffffffffff", "dataState": "live", "dedupeKey": "other"},
	}
	srv, mux := newChangeGateway(t, st)

	_, body := getChanges(t, srv, mux, "alice", "/api/theses/"+chgThesisID+"/changes")
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("one firing must appear once, and another thesis's must not appear: got %v", items)
	}
	got := items[0].(map[string]any)
	if got["kind"] != "alert_event" || got["bearing"] != "weakens" {
		t.Errorf("the event's deterministic bearing must be carried through, got %v", got)
	}
}

// TestSyntheticDataStateSurvivesIntoTheChangeSet — invariant #1's visible half.
func TestSyntheticDataStateSurvivesIntoTheChangeSet(t *testing.T) {
	st := newChangeStubs(t)
	st.events = []any{map[string]any{
		"id": "e1", "ts": 1_750_000_500, "message": "NVDA crossed below 100.00",
		"thesisId": chgThesisID, "dataState": "synthetic", "dedupeKey": "k",
	}}
	srv, mux := newChangeGateway(t, st)

	_, body := getChanges(t, srv, mux, "alice", "/api/theses/"+chgThesisID+"/changes")
	items, _ := body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["dataState"] != "synthetic" {
		t.Errorf("a synthetic-triggered event must stay labelled synthetic, got %v", items)
	}
}

// ---------------------------------------------------------------- isolation

func TestForeignThesisIs404AndComputesNothing(t *testing.T) {
	st := newChangeStubs(t)
	srv, mux := newChangeGateway(t, st)

	code, body := getChanges(t, srv, mux, "alice", "/api/theses/ffffffffffffffff/changes")
	if code != http.StatusNotFound {
		t.Fatalf("a thesis that is not the caller's must be 404, got %d: %v", code, body)
	}
	if st.llmCalls != 0 {
		t.Error("no model call may happen for a thesis the caller does not own")
	}
}

func TestChangesRequireASession(t *testing.T) {
	st := newChangeStubs(t)
	srv, mux := newChangeGateway(t, st)

	if code, _ := getChanges(t, srv, mux, "", "/api/theses/"+chgThesisID+"/changes"); code != http.StatusUnauthorized {
		t.Fatalf("a guest must get 401, got %d", code)
	}
	if st.llmCalls != 0 {
		t.Error("a guest request must not reach the model")
	}
}

// ---------------------------------------------------------------- the optional summary

func postSummary(t *testing.T, srv *Server, mux http.Handler, uid string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/theses/"+chgThesisID+"/changes/summary", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if uid != "" {
		req.Header.Set("Cookie", gatewaySession(srv, uid))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// withOneChange seeds exactly one change so the summary has something to compress.
func withOneChange(st *changeStubs) {
	st.thesis["versions"] = []any{
		map[string]any{"id": "ver_a", "at": 1_750_000_100, "actor": "user", "changedFields": []any{"claim"}},
	}
}

// TestSummaryCannotIntroduceUnknownChangeIDs is the central guarantee of §4.4's optional summary.
// The model invents an id; validation catches it, retries once, and then falls back to the
// deterministic stub rather than showing the user a fabricated citation.
func TestSummaryCannotIntroduceUnknownChangeIDs(t *testing.T) {
	st := newChangeStubs(t)
	withOneChange(st)
	st.llmReply = "You should look at [chg_deadbeefdeadbeef], it changed everything."
	srv, mux := newChangeGateway(t, st)

	code, body := postSummary(t, srv, mux, "alice")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}
	summary, _ := body["summary"].(string)
	if strings.Contains(summary, "chg_deadbeefdeadbeef") {
		t.Fatalf("an invented change id must never reach the user, got %q", summary)
	}
	if body["modelUsed"] != "stub:validation" {
		t.Errorf("a twice-invalid reply must fall back to the deterministic stub, got %v", body["modelUsed"])
	}
	if st.llmCalls != 2 {
		t.Errorf("the contract is ONE firmer retry then the stub, so exactly 2 calls; got %d", st.llmCalls)
	}
	for _, id := range body["citedIds"].([]any) {
		if !strings.HasPrefix(id.(string), "chg_") {
			t.Errorf("cited ids must be change ids, got %v", id)
		}
	}
}

// TestSummaryAcceptsRealIDs — the happy path: a reply citing a real id is kept as-is.
func TestSummaryAcceptsRealIDs(t *testing.T) {
	st := newChangeStubs(t)
	withOneChange(st)
	srv, mux := newChangeGateway(t, st)

	// Discover the real id, then have the model cite it.
	_, set := getChanges(t, srv, mux, "alice", "/api/theses/"+chgThesisID+"/changes")
	realID := set["items"].([]any)[0].(map[string]any)["id"].(string)
	st.llmReply = "You edited the claim [" + realID + "]."
	st.llmCalls = 0

	code, body := postSummary(t, srv, mux, "alice")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}
	if st.llmCalls != 1 {
		t.Errorf("a valid reply needs no retry, got %d calls", st.llmCalls)
	}
	if !strings.Contains(body["summary"].(string), realID) {
		t.Errorf("a valid citation must survive, got %q", body["summary"])
	}
	if inference, _ := body["inference"].(bool); !inference {
		t.Error("the summary is a model reading of the set and must be marked as inference")
	}
	cited := body["citedIds"].([]any)
	if len(cited) != 1 || cited[0] != realID {
		t.Errorf("citedIds must list the real id, got %v", cited)
	}
}

// TestEmptyChangeSetSpendsNoModelCall — there is nothing to summarize, so nothing is asked.
func TestEmptyChangeSetSpendsNoModelCall(t *testing.T) {
	st := newChangeStubs(t)
	srv, mux := newChangeGateway(t, st)

	code, body := postSummary(t, srv, mux, "alice")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, body)
	}
	if empty, _ := body["empty"].(bool); !empty {
		t.Errorf("want empty=true, got %v", body)
	}
	if st.llmCalls != 0 {
		t.Errorf("an empty change set must not spend a model call, got %d", st.llmCalls)
	}
	if inference, _ := body["inference"].(bool); inference {
		t.Error("a code-written empty message is not inference")
	}
}

// TestSummaryFallsBackWhenTheModelIsDown — the zero-key/offline path (invariant #1).
func TestSummaryFallsBackWhenTheModelIsDown(t *testing.T) {
	st := newChangeStubs(t)
	withOneChange(st)
	srv, mux := newChangeGateway(t, st)
	st.llm.Close() // the model is unreachable

	code, body := postSummary(t, srv, mux, "alice")
	if code != http.StatusOK {
		t.Fatalf("an offline model must not produce an error page, got %d: %v", code, body)
	}
	summary, _ := body["summary"].(string)
	if summary == "" {
		t.Error("the deterministic stub must still produce a summary")
	}
	if !strings.Contains(summary, "chg_") {
		t.Error("the stub must cite the real change ids so the UI renders identically")
	}
}

// TestSummaryDisclaimerCarriesNoVerdict — invariant #2 at the copy layer.
func TestSummaryDisclaimerCarriesNoVerdict(t *testing.T) {
	st := newChangeStubs(t)
	withOneChange(st)
	srv, mux := newChangeGateway(t, st)

	_, body := postSummary(t, srv, mux, "alice")
	d, _ := body["disclaimer"].(string)
	if d == "" {
		t.Fatal("the summary must carry a disclaimer")
	}
	if !strings.Contains(strings.ToLower(d), "authoritative") {
		t.Errorf("the disclaimer must say the change list is the authority, got %q", d)
	}
}

// TestCitedChangeIDsSplitsKnownFromUnknown is the extractor itself, since everything above depends
// on it being exact.
func TestCitedChangeIDsSplitsKnownFromUnknown(t *testing.T) {
	known := map[string]bool{"chg_1111111111111111": true}
	cited, unknown := citedChangeIDs(
		"See [chg_1111111111111111] and [chg_2222222222222222] and [chg_1111111111111111].", known)
	if len(cited) != 1 || cited[0] != "chg_1111111111111111" {
		t.Errorf("cited = %v, want the one known id, deduped", cited)
	}
	if len(unknown) != 1 || unknown[0] != "chg_2222222222222222" {
		t.Errorf("unknown = %v, want the invented id", unknown)
	}
}

// TestDegradedSourcesAreReported — a source being down degrades the answer honestly rather than
// silently shortening the list.
func TestDegradedSourcesAreReported(t *testing.T) {
	st := newChangeStubs(t)
	srv, mux := newChangeGateway(t, st)
	st.alerts.Close()

	code, body := getChanges(t, srv, mux, "alice", "/api/theses/"+chgThesisID+"/changes")
	if code != http.StatusOK {
		t.Fatalf("one unreachable source must not fail the request, got %d", code)
	}
	degraded, _ := body["degraded"].([]any)
	found := false
	for _, d := range degraded {
		if d == "alert_events" {
			found = true
		}
	}
	if !found {
		t.Errorf("an unreadable source must be named in `degraded`, got %v", degraded)
	}
}
