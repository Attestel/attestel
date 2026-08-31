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

// decisions_test.go — the gateway's two additions to the decision surface, and their limits.
//
//  1. A decision records whether a VALIDATED signal existed — presence and reason only. The direction,
//     probability and confidence must never reach the record: that is the difference between an audit
//     trail and a stored recommendation.
//  2. The cited recap is assembled in code from stored records, refuses an id the caller does not own,
//     and writes nothing.
//
// Upstreams are httptest stubs: no network, no keys, no model.

// decisionStubs stands in for the prediction service and the journal, and remembers what each
// received so the test can assert on the exact bytes that crossed the boundary.
type decisionStubs struct {
	prediction  *httptest.Server
	journal     *httptest.Server
	lastToJRNL  map[string]any
	journalCode int
	decisions   map[string]map[string]any // id -> {decision, outcome} as the journal would answer
	writes      int                       // any non-GET the journal saw during a summary
}

func newDecisionStubs(t *testing.T, predictBody map[string]any) *decisionStubs {
	t.Helper()
	s := &decisionStubs{journalCode: http.StatusCreated, decisions: map[string]map[string]any{}}

	s.prediction = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(predictBody)
	}))

	s.journal = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			s.writes++
			body, _ := io.ReadAll(r.Body)
			var got map[string]any
			_ = json.Unmarshal(body, &got)
			s.lastToJRNL = got
			w.WriteHeader(s.journalCode)
			_ = json.NewEncoder(w).Encode(map[string]any{"decision": got})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/decisions/")
		rec, ok := s.decisions[id]
		if !ok {
			// Exactly what the journal answers for an id that is not the caller's.
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "decision not found", "code": "not_found"})
			return
		}
		_ = json.NewEncoder(w).Encode(rec)
	}))

	t.Cleanup(func() {
		s.prediction.Close()
		s.journal.Close()
	})
	return s
}

func newDecisionGateway(t *testing.T, st *decisionStubs) (*Server, http.Handler) {
	t.Helper()
	cfg := loadConfig()
	cfg.PredictionURL = st.prediction.URL
	cfg.JournalURL = st.journal.URL
	cfg.Secret = "test-secret"
	cfg.CookieName = "nvda_session"
	srv := &Server{cfg: cfg, cache: newCache(), http: &http.Client{Timeout: 5 * time.Second}}
	mux := http.NewServeMux()
	srv.registerDecisionRoutes(mux)
	return srv, mux
}

func doDecision(t *testing.T, srv *Server, h http.Handler, uid, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader = strings.NewReader("")
	if body != nil {
		buf, _ := json.Marshal(body)
		rdr = strings.NewReader(string(buf))
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if uid != "" {
		req.Header.Set("Cookie", gatewaySession(srv, uid))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// ---- validated-signal context ---------------------------------------------------------------------

// A passing backtest is recorded as "a signal existed" — and NOTHING about what it said.
func TestDecisionCreateRecordsSignalPresenceNotDirection(t *testing.T) {
	st := newDecisionStubs(t, map[string]any{
		"ticker": "NVDA",
		"signal": map[string]any{"direction": "up", "probUp": 0.71, "confidence": 0.42},
		"backtest": map[string]any{
			"passed": true, "hitRate": 0.56, "sharpe": 1.2, "numTrades": 140,
		},
	})
	srv, h := newDecisionGateway(t, st)

	code, _ := doDecision(t, srv, h, "u1", http.MethodPost, "/api/decisions", map[string]any{
		"ticker": "NVDA", "thesisId": "t1", "intent": "act",
		"action": map[string]any{"kind": "open", "side": "long"}, "rationale": "Because.",
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /api/decisions: got %d", code)
	}
	got := st.lastToJRNL
	if got["modelSignalPresent"] != true {
		t.Errorf("a passing backtest should record signalPresent true, got %v", got["modelSignalPresent"])
	}
	reason, _ := got["modelSignalReason"].(string)
	if reason == "" {
		t.Error("a reason must always be recorded")
	}
	// The load-bearing assertion: no direction reaches the record, under any key.
	blob, _ := json.Marshal(got)
	for _, banned := range []string{"direction", "probUp", "\"up\"", "confidence\":0.42", "hitRate", "sharpe"} {
		if strings.Contains(string(blob), banned) {
			t.Errorf("decision payload leaked %q into the record: %s", banned, blob)
		}
	}
	// The caller's own fields are forwarded untouched — the journal stays the single validator.
	if got["rationale"] != "Because." || got["thesisId"] != "t1" {
		t.Errorf("client fields were not forwarded verbatim: %v", got)
	}
}

// A signal that did not pass its gate is recorded as absent, with the service's own reason.
func TestDecisionCreateRecordsWhyThereWasNoSignal(t *testing.T) {
	st := newDecisionStubs(t, map[string]any{
		"ticker": "NVDA", "signal": nil,
		"backtest": map[string]any{"passed": false},
		"reason":   "insufficient validation",
	})
	srv, h := newDecisionGateway(t, st)

	code, _ := doDecision(t, srv, h, "u1", http.MethodPost, "/api/decisions", map[string]any{
		"ticker": "NVDA", "thesisId": "t1", "intent": "wait",
		"action": map[string]any{"kind": "none"}, "rationale": "Waiting for the print.",
	})
	if code != http.StatusCreated {
		t.Fatalf("got %d", code)
	}
	if st.lastToJRNL["modelSignalPresent"] != false {
		t.Errorf("expected signalPresent false, got %v", st.lastToJRNL["modelSignalPresent"])
	}
	if st.lastToJRNL["modelSignalReason"] != "insufficient validation" {
		t.Errorf("expected the service's own reason, got %v", st.lastToJRNL["modelSignalReason"])
	}
}

// The zero-key / offline path: with the prediction service unreachable the decision still saves, and
// the record says the check did not happen rather than implying there was no signal.
func TestDecisionCreateSurvivesPredictionOutage(t *testing.T) {
	st := newDecisionStubs(t, map[string]any{})
	srv, h := newDecisionGateway(t, st)
	srv.cfg.PredictionURL = "http://127.0.0.1:1" // nothing listening

	code, _ := doDecision(t, srv, h, "u1", http.MethodPost, "/api/decisions", map[string]any{
		"ticker": "NVDA", "thesisId": "t1", "intent": "act",
		"action": map[string]any{"kind": "open"}, "rationale": "Offline.",
	})
	if code != http.StatusCreated {
		t.Fatalf("a decision must be recordable with no prediction service, got %d", code)
	}
	if st.lastToJRNL["modelSignalPresent"] != false {
		t.Errorf("expected signalPresent false, got %v", st.lastToJRNL["modelSignalPresent"])
	}
	reason, _ := st.lastToJRNL["modelSignalReason"].(string)
	if !strings.Contains(reason, "not recorded") {
		t.Errorf("an unavailable check must say so, got %q", reason)
	}
}

// The journal's status and machine code pass through unchanged, so the UI can explain a refusal
// instead of showing a generic failure.
func TestDecisionCreatePassesJournalRefusalThrough(t *testing.T) {
	st := newDecisionStubs(t, map[string]any{"backtest": map[string]any{"passed": false}})
	st.journalCode = http.StatusBadRequest
	srv, h := newDecisionGateway(t, st)

	code, _ := doDecision(t, srv, h, "u1", http.MethodPost, "/api/decisions", map[string]any{
		"ticker": "NVDA", "thesisId": "t1", "intent": "act", "rationale": "",
	})
	if code != http.StatusBadRequest {
		t.Errorf("the journal's status must survive the hop, got %d", code)
	}
}

// ---- cited recap ------------------------------------------------------------------------------------

func storedDecision(id, ticker, intent string, evidenceIds []any, outcome map[string]any) map[string]any {
	rec := map[string]any{
		"decision": map[string]any{
			"id": id, "ticker": ticker, "intent": intent,
			"action":      map[string]any{"kind": "open", "side": "long"},
			"horizon":     "months",
			"rationale":   "User-authored reasoning.",
			"evidenceIds": evidenceIds,
			"reviewIds":   []any{},
			"snapshot":    map[string]any{"dataState": "live"},
		},
	}
	if outcome != nil {
		rec["outcome"] = outcome
	}
	return rec
}

// The recap cites the ids it was built from, marks nothing as inference (it is all stored typed
// fields), and never calls a model.
func TestDecisionSummaryCitesStoredIDs(t *testing.T) {
	st := newDecisionStubs(t, map[string]any{})
	st.decisions["dec_1111111111111111"] = storedDecision(
		"dec_1111111111111111", "NVDA", "act", []any{"ev_aaaa", "ev_bbbb"},
		map[string]any{
			"id": "out_2222222222222222", "observedOutcome": "Guidance came in light.",
			"reasoningQuality": float64(2),
		})
	srv, h := newDecisionGateway(t, st)

	code, body := doDecision(t, srv, h, "u1", http.MethodPost, "/api/decisions/summary", map[string]any{
		"decisionIds": []any{"dec_1111111111111111"},
	})
	if code != http.StatusOK {
		t.Fatalf("summary: got %d, body %v", code, body)
	}
	if body["generatedBy"] != "code" {
		t.Errorf("the recap must declare it was generated in code, got %v", body["generatedBy"])
	}
	if body["modelUsed"] != nil {
		t.Errorf("no model is used, got %v", body["modelUsed"])
	}
	lines, _ := body["summary"].([]any)
	if len(lines) != 1 {
		t.Fatalf("expected one line, got %v", lines)
	}
	line := lines[0].(map[string]any)
	if line["inference"] != false {
		t.Errorf("a line assembled from stored fields is not inference, got %v", line["inference"])
	}
	cited := map[string]bool{}
	for _, id := range line["citedIds"].([]any) {
		cited[id.(string)] = true
	}
	for _, want := range []string{"dec_1111111111111111", "ev_aaaa", "ev_bbbb", "out_2222222222222222"} {
		if !cited[want] {
			t.Errorf("recap must cite %s, got %v", want, line["citedIds"])
		}
	}
	// It restates; it does not grade, and it never ties the rating to money.
	text := line["text"].(string)
	for _, banned := range []string{"buy", "sell", "should have", "good decision", "bad decision", "because you made"} {
		if strings.Contains(strings.ToLower(text), banned) {
			t.Errorf("recap must not judge: %q contains %q", text, banned)
		}
	}
	if st.writes != 0 {
		t.Errorf("a recap is a read: the journal saw %d writes", st.writes)
	}
}

// An id the caller does not own is refused outright. Dropping it silently would produce a recap that
// looks complete and is not.
func TestDecisionSummaryRejectsUnknownIDs(t *testing.T) {
	st := newDecisionStubs(t, map[string]any{})
	st.decisions["dec_1111111111111111"] = storedDecision("dec_1111111111111111", "NVDA", "act", []any{}, nil)
	srv, h := newDecisionGateway(t, st)

	code, body := doDecision(t, srv, h, "u1", http.MethodPost, "/api/decisions/summary", map[string]any{
		"decisionIds": []any{"dec_1111111111111111", "dec_not_mine"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%v)", code, body)
	}
	if body["code"] != "unknown_reference" {
		t.Errorf("expected code unknown_reference, got %v", body["code"])
	}
	unknown, _ := body["unknown"].([]any)
	if len(unknown) != 1 || unknown[0] != "dec_not_mine" {
		t.Errorf("expected the offending id to be named, got %v", unknown)
	}
	if _, present := body["summary"]; present {
		t.Error("a partial recap must not be returned alongside a refusal")
	}
}

// A decision with no review says so plainly, and a synthetic decision keeps its label.
func TestDecisionSummaryIsHonestAboutGaps(t *testing.T) {
	st := newDecisionStubs(t, map[string]any{})
	rec := storedDecision("dec_3333333333333333", "NVDA", "do_not_act", []any{}, nil)
	rec["decision"].(map[string]any)["action"] = map[string]any{"kind": "none"}
	rec["decision"].(map[string]any)["snapshot"] = map[string]any{"dataState": "synthetic"}
	st.decisions["dec_3333333333333333"] = rec
	srv, h := newDecisionGateway(t, st)

	_, body := doDecision(t, srv, h, "u1", http.MethodPost, "/api/decisions/summary", map[string]any{
		"decisionIds": []any{"dec_3333333333333333"},
	})
	text := body["summary"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "No evidence was cited") {
		t.Errorf("an uncited decision must say so, got %q", text)
	}
	if !strings.Contains(text, "No outcome review yet") {
		t.Errorf("an unreviewed decision must say so, got %q", text)
	}
	if !strings.Contains(text, "SYNTHETIC") {
		t.Errorf("a synthetic decision must keep its label, got %q", text)
	}
	if !strings.Contains(text, "deliberately did not act") {
		t.Errorf("a non-action must be described as one, got %q", text)
	}
	if body["reviewed"].(float64) != 0 || body["total"].(float64) != 1 {
		t.Errorf("review coverage wrong: %v of %v", body["reviewed"], body["total"])
	}
}

func TestDecisionSummaryRequiresIDs(t *testing.T) {
	st := newDecisionStubs(t, map[string]any{})
	srv, h := newDecisionGateway(t, st)
	code, body := doDecision(t, srv, h, "u1", http.MethodPost, "/api/decisions/summary", map[string]any{
		"decisionIds": []any{},
	})
	if code != http.StatusBadRequest || body["field"] != "decisionIds" {
		t.Errorf("expected a named validation failure, got %d %v", code, body)
	}
}

// The summary route is more specific than /{id} and must not shadow it.
func TestDecisionRoutesDoNotCollide(t *testing.T) {
	st := newDecisionStubs(t, map[string]any{})
	st.decisions["dec_4444444444444444"] = storedDecision("dec_4444444444444444", "NVDA", "act", []any{}, nil)
	srv, h := newDecisionGateway(t, st)

	code, body := doDecision(t, srv, h, "u1", http.MethodGet, "/api/decisions/dec_4444444444444444", nil)
	if code != http.StatusOK {
		t.Fatalf("GET one decision: got %d %v", code, body)
	}
	if body["decision"].(map[string]any)["id"] != "dec_4444444444444444" {
		t.Errorf("wrong record returned: %v", body)
	}
}
