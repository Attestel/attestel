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

// lens_test.go — the gateway's non-negotiable job for lenses: THE SERVER RESOLVES EVERY ID.
//
// The llm validates that a review is well-formed and properly cited. Only the gateway can establish
// that the cited evidence is the CALLER'S, because only the gateway holds the session cookie and
// talks to the journal. These tests pin that boundary:
//
//   · a foreign or unknown id ends the request with a 404 and NO model call ever happens;
//   · what reaches the llm is the server's resolved records, never anything the browser sent;
//   · the deterministic chart context in the saved review is the server's, not the model's;
//   · the review is persisted and the STORED record is what comes back.
//
// Upstreams are httptest stubs: no network, no keys, no model.

// ---- harness ----

type lensStubs struct {
	analysis *httptest.Server
	llm      *httptest.Server
	journal  *httptest.Server

	llmCalls    int              // how many times the llm was hit — 0 proves "no model call"
	llmLastBody map[string]any   // what the gateway actually sent the llm
	savedReview map[string]any   // what the gateway posted to /reviews
	ownedEvidence []map[string]any
	ownedThesis map[string]any
	llmReply    map[string]any
}

func newLensStubs(t *testing.T) *lensStubs {
	t.Helper()
	s := &lensStubs{
		ownedThesis: map[string]any{
			"id": "9f2c1b7a4e0d5a83", "ticker": "NVDA",
			"claim": "Networking attach lifts blended ASPs.",
			"assumptions": []any{map[string]any{"id": "asm_1111111111111111", "text": "Attach above 60%"}},
			"catalysts": []any{}, "risks": []any{},
			"invalidationConditions": []any{}, "nextQuestions": []any{},
		},
		ownedEvidence: []map[string]any{
			{"id": "ev_owned0000000001", "ticker": "NVDA", "kind": "news", "title": "Owned record"},
			{"id": "ev_owned0000000002", "ticker": "NVDA", "kind": "fundamental", "title": "Second owned record"},
		},
		llmReply: map[string]any{
			"findings": []any{map[string]any{
				"id": "fnd_1", "statement": "A cited point.", "kind": "fact",
				"citedEvidenceIds": []any{"ev_owned0000000001"},
			}},
			"agreements": []any{}, "disagreements": []any{},
			"missingEvidence": []any{}, "openQuestions": []any{},
			"citedEvidenceIds": []any{"ev_owned0000000001"},
			"lensConfidence":   60,
			"chartContext":     map[string]any{"support": 1.0, "resistance": 2.0}, // the model lying
			"stub":             false,
		},
	}

	s.analysis = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"source": "synthetic",
			"regime": map[string]any{
				"trend": "up", "momentum": "neutral",
				"keyLevels": map[string]any{"support": []any{158.2}, "resistance": []any{176.4}},
			},
		})
	}))

	s.llm = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/lenses") {
			_ = json.NewEncoder(w).Encode(map[string]any{"lenses": []any{
				map[string]any{"id": "builtin:skeptic", "name": "Devil's Advocate", "builtin": true},
			}})
			return
		}
		s.llmCalls++
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &s.llmLastBody)
		_ = json.NewEncoder(w).Encode(s.llmReply)
	}))

	s.journal = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/theses/"):
			id := strings.TrimPrefix(r.URL.Path, "/theses/")
			if id != s.ownedThesis["id"] {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "thesis not found"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"thesis": s.ownedThesis})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/evidence"):
			list := make([]any, 0, len(s.ownedEvidence))
			for _, e := range s.ownedEvidence {
				list = append(list, e)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"evidence": list})
		case r.Method == http.MethodPost && r.URL.Path == "/reviews":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &s.savedReview)
			stored := map[string]any{}
			for k, v := range s.savedReview {
				stored[k] = v
			}
			stored["id"] = "rev_stored000000001"
			stored["savedAt"] = 1750000000
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"review": stored})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "not found"})
		}
	}))

	t.Cleanup(func() {
		s.analysis.Close()
		s.llm.Close()
		s.journal.Close()
	})
	return s
}

func newLensGateway(t *testing.T, st *lensStubs) (*Server, http.Handler) {
	t.Helper()
	cfg := loadConfig()
	cfg.AnalysisURL = st.analysis.URL
	cfg.LLMURL = st.llm.URL
	cfg.JournalURL = st.journal.URL
	cfg.Secret = "test-secret"
	cfg.CookieName = "nvda_session"
	srv := &Server{cfg: cfg, cache: newCache(), http: &http.Client{Timeout: 5 * time.Second}}
	mux := http.NewServeMux()
	srv.registerLensRoutes(mux)
	return srv, mux
}

func postLens(t *testing.T, mux http.Handler, cookie string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/lens", strings.NewReader(string(buf)))
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func challengeBody(evidenceIDs ...string) map[string]any {
	ids := make([]any, 0, len(evidenceIDs))
	for _, id := range evidenceIDs {
		ids = append(ids, id)
	}
	return map[string]any{
		"lensId": "builtin:skeptic", "task": "challenge_thesis",
		"target": map[string]any{"type": "thesis", "id": "9f2c1b7a4e0d5a83"},
		"ticker": "NVDA", "evidenceIds": ids,
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON response: %v — %s", err, rec.Body.String())
	}
	return out
}

// ---- auth ----

func TestLensRequiresASession(t *testing.T) {
	st := newLensStubs(t)
	_, mux := newLensGateway(t, st)

	rec := postLens(t, mux, "", challengeBody())
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("guest lens run: want 401, got %d", rec.Code)
	}
	if st.llmCalls != 0 {
		t.Fatalf("a guest request must not reach the model, got %d calls", st.llmCalls)
	}
}

// ---- authorized context resolution ----

func TestForeignEvidenceIDIs404AndNeverReachesTheModel(t *testing.T) {
	st := newLensStubs(t)
	srv, mux := newLensGateway(t, st)

	rec := postLens(t, mux, gatewaySession(srv, "u_test"), challengeBody("ev_owned0000000001", "ev_someone_else"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign evidence id: want 404, got %d — %s", rec.Code, rec.Body.String())
	}
	// The whole point: the run does not start.
	if st.llmCalls != 0 {
		t.Fatalf("model was called %d times despite an unauthorized evidence id", st.llmCalls)
	}
	body := decode(t, rec)
	if body["code"] != "not_found" {
		t.Fatalf("want code not_found, got %v", body["code"])
	}
	// The response names which ids failed, rather than silently dropping them.
	ids, _ := body["evidenceIds"].([]any)
	if len(ids) != 1 || ids[0] != "ev_someone_else" {
		t.Fatalf("want the offending id reported, got %v", body["evidenceIds"])
	}
}

func TestForeignThesisIs404AndNeverReachesTheModel(t *testing.T) {
	st := newLensStubs(t)
	srv, mux := newLensGateway(t, st)

	b := challengeBody()
	b["target"] = map[string]any{"type": "thesis", "id": "deadbeefdeadbeef"}
	rec := postLens(t, mux, gatewaySession(srv, "u_test"), b)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign thesis: want 404, got %d", rec.Code)
	}
	if st.llmCalls != 0 {
		t.Fatalf("model was called despite an unauthorized target")
	}
}

func TestServerResolvedEvidenceIsWhatReachesTheModel(t *testing.T) {
	st := newLensStubs(t)
	srv, mux := newLensGateway(t, st)

	rec := postLens(t, mux, gatewaySession(srv, "u_test"), challengeBody("ev_owned0000000001"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}

	ctx, _ := st.llmLastBody["context"].(map[string]any)
	if ctx == nil {
		t.Fatal("no context reached the llm")
	}
	ev, _ := ctx["evidence"].([]any)
	if len(ev) != 1 {
		t.Fatalf("want exactly the 1 resolved record, got %d", len(ev))
	}
	rec0, _ := ev[0].(map[string]any)
	// It is the JOURNAL's record (with its title), not an id the browser echoed.
	if rec0["title"] != "Owned record" {
		t.Fatalf("evidence did not come from the journal: %v", rec0)
	}
	// And the resolved thesis travelled too, so the lens reviews the real claim.
	th, _ := ctx["thesis"].(map[string]any)
	if th == nil || th["claim"] != "Networking attach lifts blended ASPs." {
		t.Fatalf("thesis not resolved into the context: %v", th)
	}
}

func TestClientSuppliedEvidenceTextIsIgnored(t *testing.T) {
	st := newLensStubs(t)
	srv, mux := newLensGateway(t, st)

	b := challengeBody("ev_owned0000000001")
	// A hostile client tries to smuggle in content and a foreign record.
	b["evidence"] = []any{map[string]any{"id": "ev_injected", "title": "Fabricated proof"}}
	rec := postLens(t, mux, gatewaySession(srv, "u_test"), b)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}

	blob, _ := json.Marshal(st.llmLastBody)
	if strings.Contains(string(blob), "Fabricated proof") || strings.Contains(string(blob), "ev_injected") {
		t.Fatalf("client-supplied evidence reached the model: %s", blob)
	}
}

// ---- deterministic chart context ----

func TestChartContextIsReimposedFromTheServer(t *testing.T) {
	st := newLensStubs(t)
	srv, mux := newLensGateway(t, st)

	b := map[string]any{
		"lensId": "builtin:technician", "task": "review_chart_context",
		"target": map[string]any{"type": "chart_context", "id": "NVDA"},
		"ticker": "NVDA", "timeframe": "1D", "evidenceIds": []any{},
	}
	rec := postLens(t, mux, gatewaySession(srv, "u_test"), b)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}

	chart, _ := st.savedReview["chartContext"].(map[string]any)
	if chart == nil {
		t.Fatal("no chart context on the saved review")
	}
	sup, _ := chart["support"].([]any)
	if len(sup) != 1 || sup[0].(float64) != 158.2 {
		t.Fatalf("server support level not re-imposed, got %v", chart["support"])
	}
	// The model's fabricated {support:1.0, resistance:2.0} must be gone.
	if _, isNumber := chart["support"].(float64); isNumber {
		t.Fatalf("the model's chart context survived: %v", chart)
	}
	// Synthetic data stays labelled so a review built on it is never mistaken for live.
	if chart["dataState"] != "synthetic" {
		t.Fatalf("dataState not carried through, got %v", chart["dataState"])
	}
}

func TestChallengeThesisDoesNotPullChartContext(t *testing.T) {
	// A thesis challenge should not silently drag in a chart the user never asked about.
	st := newLensStubs(t)
	srv, mux := newLensGateway(t, st)

	rec := postLens(t, mux, gatewaySession(srv, "u_test"), challengeBody("ev_owned0000000001"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d", rec.Code)
	}
	if st.savedReview["chartContext"] != nil {
		t.Fatalf("challenge_thesis pulled chart context: %v", st.savedReview["chartContext"])
	}
}

// ---- validation ----

func TestUnknownTaskAndTargetAreRejectedBeforeAnyModelCall(t *testing.T) {
	st := newLensStubs(t)
	srv, mux := newLensGateway(t, st)
	cookie := gatewaySession(srv, "u_test")

	b := challengeBody()
	b["task"] = "do_whatever"
	if rec := postLens(t, mux, cookie, b); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown task: want 400, got %d", rec.Code)
	}

	b = challengeBody()
	b["target"] = map[string]any{"type": "spaceship", "id": "x"}
	if rec := postLens(t, mux, cookie, b); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown target type: want 400, got %d", rec.Code)
	}

	if st.llmCalls != 0 {
		t.Fatalf("invalid requests reached the model %d times", st.llmCalls)
	}
}

func TestDecisionTargetIsAcceptedButNotYetAvailable(t *testing.T) {
	// §3.2: Step 07 must accept the type and 404 until Step 09 builds the store — not 400, which
	// would say the type is invalid.
	st := newLensStubs(t)
	srv, mux := newLensGateway(t, st)

	b := challengeBody()
	b["target"] = map[string]any{"type": "decision", "id": "dec_123"}
	rec := postLens(t, mux, gatewaySession(srv, "u_test"), b)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("decision target: want 404, got %d", rec.Code)
	}
	if st.llmCalls != 0 {
		t.Fatal("decision target reached the model")
	}
}

func TestTooMuchEvidenceIsRejected(t *testing.T) {
	st := newLensStubs(t)
	srv, mux := newLensGateway(t, st)

	ids := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		ids = append(ids, "ev_x")
	}
	rec := postLens(t, mux, gatewaySession(srv, "u_test"), challengeBody(ids...))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-cap evidence: want 400, got %d", rec.Code)
	}
}

// ---- persistence ----

func TestReviewIsPersistedAndTheStoredRecordIsReturned(t *testing.T) {
	st := newLensStubs(t)
	srv, mux := newLensGateway(t, st)

	rec := postLens(t, mux, gatewaySession(srv, "u_test"), challengeBody("ev_owned0000000001"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	if st.savedReview == nil {
		t.Fatal("review was never posted to the journal")
	}
	out := decode(t, rec)
	stored, _ := out["review"].(map[string]any)
	if stored == nil || stored["id"] != "rev_stored000000001" {
		t.Fatalf("caller did not receive the STORED record: %v", out)
	}
	if stored["savedAt"] == nil {
		t.Fatal("stored record has no savedAt")
	}
}

func TestJournalFailureSurfacesInsteadOfPretendingItSaved(t *testing.T) {
	st := newLensStubs(t)
	st.journal.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/theses/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"thesis": st.ownedThesis})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/evidence"):
			_ = json.NewEncoder(w).Encode(map[string]any{"evidence": []any{}})
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "stored reviews could not be read", "code": "store_unreadable"})
		}
	})
	srv, mux := newLensGateway(t, st)

	rec := postLens(t, mux, gatewaySession(srv, "u_test"), challengeBody())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("journal store failure: want the journal's 503 to survive, got %d — %s",
			rec.Code, rec.Body.String())
	}
}
