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

// evidence_test.go — the gateway's one non-negotiable job in this layer: PROVENANCE IS SERVER-STAMPED.
//
// The journal validates that a record's provenance is COMPLETE. Only the gateway can establish that it
// is TRUE, because only the gateway saw the upstream response. These tests pin that boundary: a
// selector in, a fully-attributed record out, and nothing the browser says about source, provider or
// data state surviving the hop.
//
// Upstreams are httptest stubs, so this runs with no network, no keys and no model.

// ---- harness ----

type stubs struct {
	analysis   *httptest.Server
	llm        *httptest.Server
	journal    *httptest.Server
	lastToJRNL map[string]any // the body the journal actually received
	journalErr int            // when non-zero, the journal stub replies with this status
}

func newStubs(t *testing.T, analysisBody, llmBody map[string]any) *stubs {
	t.Helper()
	s := &stubs{}

	s.analysis = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(analysisBody)
	}))
	s.llm = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(llmBody)
	}))
	s.journal = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		s.lastToJRNL = got
		w.Header().Set("Content-Type", "application/json")
		if s.journalErr != 0 {
			w.WriteHeader(s.journalErr)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "missing required provenance", "code": "provenance_incomplete",
				"missing": []string{"sourceTime"},
			})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"evidence": got, "deduplicated": false})
	}))

	t.Cleanup(func() {
		s.analysis.Close()
		s.llm.Close()
		s.journal.Close()
	})
	return s
}

func newTestGateway(t *testing.T, st *stubs) (*Server, http.Handler) {
	t.Helper()
	cfg := loadConfig()
	cfg.AnalysisURL = st.analysis.URL
	cfg.LLMURL = st.llm.URL
	cfg.JournalURL = st.journal.URL
	cfg.Secret = "test-secret"
	cfg.CookieName = "nvda_session"
	// Stubs answer instantly; a short client timeout keeps a hung test from taking 130s.
	srv := &Server{cfg: cfg, cache: newCache(), http: &http.Client{Timeout: 5 * time.Second}}
	mux := http.NewServeMux()
	srv.registerEvidenceRoutes(mux)
	return srv, mux
}

func gatewaySession(srv *Server, uid string) string {
	raw, _ := json.Marshal(sessionPayload{
		UID: uid, IAT: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix(),
	})
	body := b64.EncodeToString(raw)
	return srv.cfg.CookieName + "=" + body + "." + signPayload(srv.cfg.Secret, body)
}

func postFromFact(t *testing.T, srv *Server, h http.Handler, uid string, body map[string]any) (int, map[string]any) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/evidence/from-fact", strings.NewReader(string(buf)))
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

// analysisWith builds an /analysis response with a chosen price source.
func analysisWith(source string, synthetic bool) map[string]any {
	return map[string]any{
		"ticker": "NVDA", "timeframe": "1D", "source": source, "sourceIsSynthetic": synthetic,
		"price": map[string]any{"lastClose": 162.4, "changePct": 1.2, "asOf": "2026-07-29"},
		"indicators": map[string]any{"latest": map[string]any{
			"rsi": 71.2, "sma50": 158.9, "atr": 4.1, "adx": 28.4, "vwap": nil,
		}},
		"regime": map[string]any{
			"trend": "uptrend", "momentum": "strong", "lastClose": 162.4,
			"keyLevels": map[string]any{"support": []any{158.2, 151.0}, "resistance": []any{176.4}},
		},
	}
}

// ---- provenance is server-stamped ----

// The headline test. The client sends deliberately false provenance for a value it does not control;
// the gateway resolves the real thing and the journal receives the SERVER's account of it.
func TestFromFactIgnoresClientSuppliedProvenance(t *testing.T) {
	st := newStubs(t, analysisWith("tiingo", false), nil)
	srv, h := newTestGateway(t, st)

	code, _ := postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "deterministic_fact", "timeframe": "1D",
		"selector": map[string]any{"field": "rsi"},
		// Everything below is a lie the client is not permitted to tell.
		"provider":    "sec-edgar",
		"dataState":   "live",
		"attribution": "Audited financial statements",
		"sourceUrl":   "https://sec.gov/definitely-real",
		"sourceTime":  1,
		"excerpt":     "text the browser typed",
		"fact":        map[string]any{"key": "rsi", "value": 5, "asOf": "1999-01-01", "timeframe": "1D"},
		"capturedAt":  1,
		"inference":   true,
	})
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d", code)
	}

	got := st.lastToJRNL
	if got["provider"] != "analysis" {
		t.Errorf("provider must be the server's: got %v", got["provider"])
	}
	if got["attribution"] == "Audited financial statements" {
		t.Error("client attribution reached the store")
	}
	if !strings.Contains(got["attribution"].(string), "tiingo") {
		t.Errorf("attribution must name the real price feed, got %q", got["attribution"])
	}
	if got["sourceUrl"] != "" {
		t.Errorf("client sourceUrl reached the store: %v", got["sourceUrl"])
	}
	if got["excerpt"] != "" {
		t.Errorf("client excerpt reached the store: %q", got["excerpt"])
	}
	if _, present := got["inference"]; present {
		t.Error("a client must not be able to set the inference flag through from-fact")
	}
	fact, _ := got["fact"].(map[string]any)
	if fact["value"].(float64) != 71.2 {
		t.Errorf("fact value must come from the analysis service, got %v", fact["value"])
	}
	if fact["asOf"] != "2026-07-29" {
		t.Errorf("fact must be dated by the bar, not by the client: got %v", fact["asOf"])
	}
	// The user's own note IS theirs, and does survive.
	if got["note"] != "" {
		t.Errorf("unexpected note: %v", got["note"])
	}
}

// Synthetic bars must be labelled synthetic all the way into the record. Invariant #1's visible half
// depends on this never being softened by a client hint.
func TestFromFactPreservesSyntheticDataState(t *testing.T) {
	st := newStubs(t, analysisWith("synthetic", true), nil)
	srv, h := newTestGateway(t, st)

	code, _ := postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "deterministic_fact", "timeframe": "1D",
		"selector": map[string]any{"field": "rsi"}, "dataState": "live",
	})
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d", code)
	}
	if st.lastToJRNL["dataState"] != "synthetic" {
		t.Errorf("synthetic bars must produce dataState synthetic, got %v", st.lastToJRNL["dataState"])
	}
	attr, _ := st.lastToJRNL["attribution"].(string)
	if !strings.Contains(strings.ToUpper(attr), "SYNTHETIC") {
		t.Errorf("the attribution must say the data is synthetic, got %q", attr)
	}
}

// A user note is captured against whatever the prices on screen actually were, so a note written
// against demo bars can never later read as if it were written against the market.
func TestFromFactUserNoteInheritsSyntheticState(t *testing.T) {
	st := newStubs(t, analysisWith("synthetic", true), nil)
	srv, h := newTestGateway(t, st)

	code, _ := postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "user_note", "timeframe": "1D", "note": "watching the ramp",
	})
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d", code)
	}
	got := st.lastToJRNL
	if got["dataState"] != "synthetic" || got["provider"] != "user" {
		t.Errorf("user note provenance: got provider=%v dataState=%v", got["provider"], got["dataState"])
	}
	if got["note"] != "watching the ramp" {
		t.Errorf("the note is the user's and must survive verbatim, got %q", got["note"])
	}
}

// ---- selectors are a closed set ----

func TestFromFactRejectsUnknownSelector(t *testing.T) {
	st := newStubs(t, analysisWith("tiingo", false), nil)
	srv, h := newTestGateway(t, st)

	code, body := postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "deterministic_fact", "timeframe": "1D",
		"selector": map[string]any{"field": "secretAlphaScore"},
	})
	if code != http.StatusBadRequest || body["code"] != "unknown_selector" {
		t.Errorf("want 400/unknown_selector, got %d %v", code, body)
	}
}

// A value the analysis service does not have yet is a clear refusal, not a zero or a null stored as
// if it were a measurement.
func TestFromFactRefusesUnavailableValue(t *testing.T) {
	st := newStubs(t, analysisWith("tiingo", false), nil)
	srv, h := newTestGateway(t, st)

	code, body := postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "deterministic_fact", "timeframe": "1D",
		"selector": map[string]any{"field": "vwap"}, // present in the stub, but null
	})
	if code != http.StatusUnprocessableEntity || body["code"] != "value_unavailable" {
		t.Errorf("want 422/value_unavailable, got %d %v", code, body)
	}
}

// A computed level is captured with the level's own value, its computation date and its timeframe.
func TestFromFactChartContextLevel(t *testing.T) {
	st := newStubs(t, analysisWith("tiingo", false), nil)
	srv, h := newTestGateway(t, st)

	code, _ := postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "chart_context", "timeframe": "1D",
		"selector": map[string]any{"level": "support", "index": 1},
	})
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d", code)
	}
	fact, _ := st.lastToJRNL["fact"].(map[string]any)
	if fact["value"].(float64) != 151.0 {
		t.Errorf("wrong level captured: %v", fact["value"])
	}
	if fact["key"] != "keyLevels.support" || fact["timeframe"] != "1D" || fact["asOf"] != "2026-07-29" {
		t.Errorf("level fact metadata: %v", fact)
	}

	code, body := postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "chart_context", "timeframe": "1D",
		"selector": map[string]any{"level": "support", "index": 9},
	})
	if code != http.StatusUnprocessableEntity || body["code"] != "value_unavailable" {
		t.Errorf("a level that no longer exists must be refused, got %d %v", code, body)
	}
}

// ---- transcript quotations ----

func transcriptStub() map[string]any {
	return map[string]any{
		"ticker": "NVDA",
		"analyses": []any{map[string]any{
			"label": "Q1 FY2027", "key": "Q1-FY2027", "timestamp": "2026-05-28T21:00:00Z",
			"modelUsed": "qwen2.5:7b",
			"structured": map[string]any{
				"keyPoints": []any{"Datacenter revenue was a record", "Supply remains constrained"},
				"evasiveMoments": []any{map[string]any{
					"question": "Can you quantify the China impact?",
					"answer":   "we are not going to comment on that today",
					"why":      "declined to size it", "topic": "guidance",
				}},
				"tone":      map[string]any{"confidence": "high", "hedging": "medium", "notableLanguage": []any{"too early to say"}},
				"riskFlags": []any{"supply"},
				"summary":   "A record quarter with hedged China commentary.",
			},
		}},
	}
}

// A transcript excerpt can only be the stored snapshot's own verified bytes. The quote is not taken
// from the request, and there is no route from a pasted or fetched transcript body into the store.
func TestFromFactTranscriptExcerptUsesStoredQuote(t *testing.T) {
	st := newStubs(t, analysisWith("tiingo", false), transcriptStub())
	srv, h := newTestGateway(t, st)

	code, _ := postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "transcript_excerpt", "timeframe": "1D",
		"selector": map[string]any{"transcriptKey": "Q1-FY2027", "locator": "evasiveMoments:0:answer"},
		"excerpt":  "a quote the client made up",
	})
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d", code)
	}
	got := st.lastToJRNL
	if got["excerpt"] != "we are not going to comment on that today" {
		t.Errorf("excerpt must be the stored snapshot's own text, got %q", got["excerpt"])
	}
	ref, _ := got["sourceRef"].(map[string]any)
	if ref["store"] != "llm-transcript" || ref["id"] != "NVDA/Q1-FY2027" || ref["locator"] != "evasiveMoments:0:answer" {
		t.Errorf("sourceRef must point at the stored snapshot: %v", ref)
	}
	if got["sourceTime"].(float64) == 0 {
		t.Error("the snapshot timestamp should date the capture")
	}
	attr, _ := got["attribution"].(string)
	if !strings.Contains(attr, "verified") || !strings.Contains(attr, "not retained") {
		t.Errorf("attribution must state that the quote was verified and the source text is not kept, got %q", attr)
	}
}

// A locator with no string behind it is refused as unverifiable — the code the UI needs to explain
// that a quotation could not be confirmed.
func TestFromFactTranscriptRejectsUnverifiableQuote(t *testing.T) {
	st := newStubs(t, analysisWith("tiingo", false), transcriptStub())
	srv, h := newTestGateway(t, st)

	for _, locator := range []string{"keyPoints:9", "evasiveMoments:0:aside", "madeUpField:0"} {
		code, body := postFromFact(t, srv, h, "u1", map[string]any{
			"ticker": "NVDA", "kind": "transcript_excerpt", "timeframe": "1D",
			"selector": map[string]any{"transcriptKey": "Q1-FY2027", "locator": locator},
		})
		if code != http.StatusBadRequest || body["code"] != "quote_unverified" {
			t.Errorf("locator %q: want 400/quote_unverified, got %d %v", locator, code, body)
		}
	}

	code, body := postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "transcript_excerpt", "timeframe": "1D",
		"selector": map[string]any{"transcriptKey": "Q9-NEVER", "locator": "summary"},
	})
	if code != http.StatusUnprocessableEntity || body["code"] != "source_stale" {
		t.Errorf("unknown snapshot: want 422/source_stale, got %d %v", code, body)
	}
}

// A management statement needs a named speaker, because the stored analysis does not record one.
// Asking the user rather than guessing is the honest option; guessing would fabricate attribution.
func TestFromFactManagementStatementNeedsSpeaker(t *testing.T) {
	st := newStubs(t, analysisWith("tiingo", false), transcriptStub())
	srv, h := newTestGateway(t, st)

	code, body := postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "management_statement", "timeframe": "1D",
		"selector": map[string]any{"transcriptKey": "Q1-FY2027", "locator": "evasiveMoments:0:answer"},
	})
	if code != http.StatusBadRequest || body["code"] != "provenance_incomplete" {
		t.Fatalf("want 400/provenance_incomplete, got %d %v", code, body)
	}

	code, _ = postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "management_statement", "timeframe": "1D",
		"selector": map[string]any{
			"transcriptKey": "Q1-FY2027", "locator": "evasiveMoments:0:answer",
			"speaker": "Colette Kress, CFO",
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("want 201 with a speaker, got %d", code)
	}
	got := st.lastToJRNL
	if !strings.HasPrefix(got["title"].(string), "Colette Kress, CFO") {
		t.Errorf("title must carry the speaker, got %q", got["title"])
	}
	if !strings.Contains(got["attribution"].(string), "supplied by you") {
		t.Errorf("a user-supplied speaker must be labelled as such, got %q", got["attribution"])
	}
}

// ---- auth + pass-through ----

func TestFromFactRequiresSignIn(t *testing.T) {
	st := newStubs(t, analysisWith("tiingo", false), nil)
	srv, h := newTestGateway(t, st)

	code, body := postFromFact(t, srv, h, "", map[string]any{
		"ticker": "NVDA", "kind": "deterministic_fact", "timeframe": "1D",
		"selector": map[string]any{"field": "rsi"},
	})
	if code != http.StatusUnauthorized {
		t.Errorf("guest capture: want 401, got %d %v", code, body)
	}
	if st.lastToJRNL != nil {
		t.Error("a guest request must never reach the journal")
	}
}

// The journal's status and machine code survive the hop unchanged, so the UI can react to the actual
// failure rather than to a generic gateway error.
func TestFromFactPassesJournalErrorThrough(t *testing.T) {
	st := newStubs(t, analysisWith("tiingo", false), nil)
	st.journalErr = http.StatusBadRequest
	srv, h := newTestGateway(t, st)

	code, body := postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "deterministic_fact", "timeframe": "1D",
		"selector": map[string]any{"field": "rsi"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("want the journal's 400, got %d", code)
	}
	if body["code"] != "provenance_incomplete" {
		t.Errorf("the journal's error code must survive, got %v", body["code"])
	}
}

// An inline link is forwarded so save-and-link is one request, and a bad bearing is caught before
// anything is written.
func TestFromFactForwardsInlineLink(t *testing.T) {
	st := newStubs(t, analysisWith("tiingo", false), nil)
	srv, h := newTestGateway(t, st)

	code, _ := postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "deterministic_fact", "timeframe": "1D",
		"selector": map[string]any{"field": "rsi"},
		"thesisId": "9f2c1b7a4e0d5a83", "bearing": "contradicts", "note": "cuts against the ramp",
	})
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d", code)
	}
	link, _ := st.lastToJRNL["link"].(map[string]any)
	if link == nil || link["thesisId"] != "9f2c1b7a4e0d5a83" || link["bearing"] != "contradicts" {
		t.Errorf("inline link not forwarded: %v", link)
	}

	st.lastToJRNL = nil
	code, _ = postFromFact(t, srv, h, "u1", map[string]any{
		"ticker": "NVDA", "kind": "deterministic_fact", "timeframe": "1D",
		"selector": map[string]any{"field": "rsi"},
		"thesisId": "9f2c1b7a4e0d5a83", "bearing": "bullish",
	})
	if code != http.StatusBadRequest {
		t.Errorf("bad bearing: want 400, got %d", code)
	}
	if st.lastToJRNL != nil {
		t.Error("a request with an invalid bearing must not reach the journal")
	}
}

// The eight CRUD paths are pure pass-through: method, query and status all preserved.
func TestEvidenceProxyPassesThrough(t *testing.T) {
	st := newStubs(t, nil, nil)
	var sawPath, sawMethod, sawCookie string
	st.journal.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath, sawMethod, sawCookie = r.URL.RequestURI(), r.Method, r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "still linked", "code": "evidence_linked", "linkCount": 2,
		})
	})
	srv, h := newTestGateway(t, st)

	req := httptest.NewRequest(http.MethodDelete, "/api/evidence/ev_abc?force=false", nil)
	req.Header.Set("Cookie", gatewaySession(srv, "u1"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("upstream status must pass through: got %d", rec.Code)
	}
	if sawPath != "/evidence/ev_abc?force=false" {
		t.Errorf("path/query rewriting: got %q", sawPath)
	}
	if sawMethod != http.MethodDelete {
		t.Errorf("method: got %q", sawMethod)
	}
	if !strings.Contains(sawCookie, "nvda_session=") {
		t.Error("the session cookie must be forwarded so per-user scoping applies")
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "evidence_linked" {
		t.Errorf("upstream error code must pass through: %v", body)
	}
}

// ---- unit checks ----

func TestParseEvidenceTimeNeverInventsATime(t *testing.T) {
	if parseEvidenceTime("") != 0 {
		t.Error("an empty time must stay unknown (0), never become now()")
	}
	if parseEvidenceTime("sometime last week") != 0 {
		t.Error("an unparseable time must stay unknown (0)")
	}
	if got := parseEvidenceTime("2026-07-29"); got != 1785283200 {
		t.Errorf("date parse: got %d", got)
	}
	if parseEvidenceTime("2026-05-28T21:00:00Z") == 0 {
		t.Error("RFC3339 must parse")
	}
}

func TestSECAccessionFromURL(t *testing.T) {
	got := secAccessionFromURL("https://www.sec.gov/Archives/edgar/data/1045810/000104581026000012/nvda-8k.htm")
	if got != "0001045810-26-000012" {
		t.Errorf("accession: got %q", got)
	}
	if secAccessionFromURL("https://example.com/nope") != "" {
		t.Error("an unfamiliar URL must yield no accession number rather than a guess")
	}
}

func TestNewsProvenancePerItem(t *testing.T) {
	cases := []struct {
		item         map[string]any
		isFiling     bool
		wantProvider string
		wantState    string
	}{
		{map[string]any{"source": "EDGAR"}, true, "sec-edgar", "live"},
		{map[string]any{"source": "NVIDIA / filings (seed)"}, false, "seed", "seed"},
		{map[string]any{"source": "Reuters", "score": 0.4}, false, "marketaux", "live"},
		{map[string]any{"source": "CNBC"}, false, "google-news-rss", "live"},
	}
	for _, c := range cases {
		p, d, a := newsProvenance(c.item, c.isFiling)
		if p != c.wantProvider || d != c.wantState {
			t.Errorf("item %v: got provider=%q state=%q, want %q/%q", c.item, p, d, c.wantProvider, c.wantState)
		}
		if a == "" {
			t.Errorf("item %v: attribution must never be empty", c.item)
		}
	}
}
