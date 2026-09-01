package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// evidence_test.go — the evidence layer's guarantees, exercised through the real HTTP surface.
//
// Every test here runs with NO network, NO model and NO API key: evidence is a journal-local concern
// by construction, which is exactly why the offline path is testable at all (invariant #1).
//
// The properties under test are the ones that would quietly corrupt research if they broke:
// provenance completeness per kind, capture immutability, per-user isolation, the dedupe rule, and
// the two deletion semantics (a link dies alone; a linked record does not die by accident).

// ---- harness ----

type testEnv struct {
	srv     *Server
	handler http.Handler
	dir     string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		TradesDir:  dir,
		Secret:     "test-secret",
		CookieName: "nvda_session",
	}
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

// cookieFor mints a valid session for uid using the same signer the service verifies with.
func (e *testEnv) cookieFor(uid string) string {
	raw, _ := json.Marshal(sessionPayload{
		UID: uid, IAT: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix(),
	})
	body := b64.EncodeToString(raw)
	return e.srv.cfg.CookieName + "=" + body + "." + signPayload(e.srv.cfg.Secret, body)
}

// do issues a request as uid ("" = guest) and decodes the JSON body.
func (e *testEnv) do(t *testing.T, uid, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(buf)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if uid != "" {
		req.Header.Set("Cookie", e.cookieFor(uid))
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: response is not JSON (%d): %s", method, path, rec.Code, rec.Body.String())
		}
	}
	return rec.Code, out
}

// makeThesis creates a thesis for uid and returns its id.
func (e *testEnv) makeThesis(t *testing.T, uid, ticker, text string) string {
	t.Helper()
	code, body := e.do(t, uid, "POST", "/theses", map[string]any{"ticker": ticker, "text": text})
	if code != http.StatusCreated {
		t.Fatalf("create thesis: got %d, body %v", code, body)
	}
	th, _ := body["thesis"].(map[string]any)
	id, _ := th["id"].(string)
	if id == "" {
		t.Fatal("create thesis: no id returned")
	}
	return id
}

// deterministicFact is a complete, valid offline capture: a computed indicator on synthetic bars.
// Nothing about it needs a provider key or a network call.
func deterministicFact(asOf string) map[string]any {
	return map[string]any{
		"ticker": "NVDA", "kind": "deterministic_fact",
		"title":     "NVDA · RSI(14) = 71.2",
		"provider":  "analysis",
		"dataState": "synthetic",
		"fact": map[string]any{
			"key": "rsi", "value": 71.2, "unit": "", "asOf": asOf, "timeframe": "1D",
		},
		"attribution": "Computed by the analysis service from SYNTHETIC price data",
	}
}

func mustCreate(t *testing.T, e *testEnv, uid string, payload map[string]any) map[string]any {
	t.Helper()
	code, body := e.do(t, uid, "POST", "/evidence", payload)
	if code != http.StatusCreated {
		t.Fatalf("POST /evidence: got %d, body %v", code, body)
	}
	rec, _ := body["evidence"].(map[string]any)
	if rec == nil {
		t.Fatalf("POST /evidence: no record in %v", body)
	}
	return rec
}

// ---- round trip + offline deterministic save ----

// A deterministic fact saved with zero providers survives a full restart of the store. This is the
// zero-key path: no model, no key, no network anywhere in the save.
func TestEvidenceRoundTripOffline(t *testing.T) {
	e := newTestEnv(t)
	rec := mustCreate(t, e, "u1", deterministicFact("2026-07-29"))

	if !strings.HasPrefix(rec["id"].(string), "ev_") {
		t.Errorf("id must be prefixed ev_, got %q", rec["id"])
	}
	if len(rec["id"].(string)) != len("ev_")+16 {
		t.Errorf("id must be ev_ + 16 hex, got %q", rec["id"])
	}
	if rec["capturedAt"].(float64) == 0 {
		t.Error("capturedAt must be server-stamped")
	}
	if rec["dedupeKey"] == "" {
		t.Error("dedupeKey must be computed")
	}
	if rec["dataState"] != "synthetic" {
		t.Errorf("dataState must be preserved verbatim, got %v", rec["dataState"])
	}

	// On disk, in the user's own partition.
	path := filepath.Join(e.dir, "u1", evidenceFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("evidence.json not written: %v", err)
	}

	// A brand-new server over the same directory reads it back identically — nothing lived only in
	// the in-memory cache.
	fresh := &testEnv{dir: e.dir}
	store, _ := openStore(e.dir)
	theses, _ := openThesisStore(e.dir)
	fresh.srv = &Server{cfg: e.srv.cfg, store: store, theses: theses, http: &http.Client{}}
	fresh.handler = fresh.srv.routes()

	code, body := fresh.do(t, "u1", "GET", "/evidence", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /evidence after reload: %d", code)
	}
	list, _ := body["evidence"].([]any)
	if len(list) != 1 {
		t.Fatalf("expected 1 record after reload, got %d", len(list))
	}
	got := list[0].(map[string]any)
	if got["id"] != rec["id"] || got["dataState"] != "synthetic" {
		t.Errorf("record changed across reload: %v", got)
	}
	fact, _ := got["fact"].(map[string]any)
	if fact["asOf"] != "2026-07-29" || fact["timeframe"] != "1D" {
		t.Errorf("computed fact lost its computation date/timeframe: %v", fact)
	}
}

// ---- user isolation ----

// One user's evidence is invisible and unreachable to another, and a foreign id is a 404 rather than
// a 403 — a 403 would confirm the record exists.
func TestEvidenceUserIsolation(t *testing.T) {
	e := newTestEnv(t)
	mine := mustCreate(t, e, "u1", deterministicFact("2026-07-29"))
	id := mine["id"].(string)

	code, body := e.do(t, "u2", "GET", "/evidence", nil)
	if code != http.StatusOK {
		t.Fatalf("u2 list: %d", code)
	}
	if list, _ := body["evidence"].([]any); len(list) != 0 {
		t.Errorf("u2 must not see u1's evidence, got %d records", len(list))
	}

	if code, _ := e.do(t, "u2", "PATCH", "/evidence/"+id, map[string]any{"note": "mine now"}); code != http.StatusNotFound {
		t.Errorf("u2 patching u1's record: want 404, got %d", code)
	}
	if code, _ := e.do(t, "u2", "DELETE", "/evidence/"+id, nil); code != http.StatusNotFound {
		t.Errorf("u2 deleting u1's record: want 404, got %d", code)
	}

	// u1's record is still intact and still u1's.
	code, body = e.do(t, "u1", "GET", "/evidence", nil)
	if list, _ := body["evidence"].([]any); code != http.StatusOK || len(list) != 1 {
		t.Errorf("u1's record was affected by u2's attempts: %d %v", code, body)
	}
}

// A guest reads an empty list and cannot write. Reads staying open (rather than 401) is what lets the
// UI render the section plus its sign-in prompt without a failed request.
func TestEvidenceGuestReadOnly(t *testing.T) {
	e := newTestEnv(t)
	code, body := e.do(t, "", "GET", "/evidence", nil)
	if code != http.StatusOK {
		t.Fatalf("guest read: want 200, got %d", code)
	}
	if list, _ := body["evidence"].([]any); len(list) != 0 {
		t.Errorf("guest must see an empty list, got %v", list)
	}
	if code, _ := e.do(t, "", "POST", "/evidence", deterministicFact("2026-07-29")); code != http.StatusUnauthorized {
		t.Errorf("guest write: want 401, got %d", code)
	}
	if code, _ := e.do(t, "", "GET", "/evidence/links", nil); code != http.StatusOK {
		t.Errorf("guest link read: want 200, got %d", code)
	}
	if code, _ := e.do(t, "", "POST", "/evidence/links", map[string]any{}); code != http.StatusUnauthorized {
		t.Errorf("guest link write: want 401, got %d", code)
	}
}

// ---- provenance requirements per kind ----

// Each kind has its own required provenance (§2.3). A save that cannot describe where its content
// came from is refused with the missing paths named, so the caller can fix it rather than guess.
func TestEvidenceProvenanceRequiredPerKind(t *testing.T) {
	e := newTestEnv(t)

	cases := []struct {
		name        string
		payload     map[string]any
		wantMissing string
	}{
		{
			name: "deterministic fact without a computation date",
			payload: map[string]any{
				"ticker": "NVDA", "kind": "deterministic_fact", "provider": "analysis", "dataState": "live",
				"fact": map[string]any{"key": "rsi", "value": 71.2, "timeframe": "1D"},
			},
			wantMissing: "fact.asOf",
		},
		{
			name: "deterministic fact without a timeframe",
			payload: map[string]any{
				"ticker": "NVDA", "kind": "deterministic_fact", "provider": "analysis", "dataState": "live",
				"fact": map[string]any{"key": "rsi", "value": 71.2, "asOf": "2026-07-29"},
			},
			wantMissing: "fact.timeframe",
		},
		{
			name: "news without a publication time",
			payload: map[string]any{
				"ticker": "NVDA", "kind": "news", "provider": "marketaux", "dataState": "live",
				"sourceUrl": "https://example.com/a", "attribution": "Marketaux", "title": "headline",
			},
			wantMissing: "sourceTime",
		},
		{
			name: "news without a source url",
			payload: map[string]any{
				"ticker": "NVDA", "kind": "news", "provider": "marketaux", "dataState": "live",
				"attribution": "Marketaux", "sourceTime": 1748400000, "title": "headline",
			},
			wantMissing: "sourceUrl",
		},
		{
			name: "filing without an accession number",
			payload: map[string]any{
				"ticker": "NVDA", "kind": "filing", "provider": "sec-edgar", "dataState": "live",
				"sourceUrl": "https://sec.gov/x", "title": "8-K",
			},
			wantMissing: "sourceRef.id",
		},
		{
			name: "management statement without a named speaker",
			payload: map[string]any{
				"ticker": "NVDA", "kind": "management_statement", "provider": "user", "dataState": "live",
				"sourceRef":   map[string]any{"store": "llm-transcript", "id": "NVDA/Q1", "locator": "keyPoints:0"},
				"attribution": "Q1 call", "excerpt": "we are supply constrained",
			},
			wantMissing: "title",
		},
		{
			name: "chart context without a timeframe",
			payload: map[string]any{
				"ticker": "NVDA", "kind": "chart_context", "provider": "analysis", "dataState": "live",
				"fact": map[string]any{"key": "keyLevels.support", "value": 158.2, "asOf": "2026-07-29"},
			},
			wantMissing: "fact.timeframe",
		},
		{
			name:        "user note with no note",
			payload:     map[string]any{"ticker": "NVDA", "kind": "user_note", "provider": "user", "dataState": "live"},
			wantMissing: "note",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := e.do(t, "u1", "POST", "/evidence", tc.payload)
			if code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (%v)", code, body)
			}
			if body["code"] != "provenance_incomplete" {
				t.Fatalf("want code provenance_incomplete, got %v", body["code"])
			}
			missing, _ := body["missing"].([]any)
			found := false
			for _, m := range missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Errorf("want %q in missing, got %v", tc.wantMissing, missing)
			}
		})
	}
}

// A catalyst is dated by EITHER the event time or the fact's asOf — one of the two is enough, neither
// is not.
func TestEvidenceCatalystAcceptsEitherDate(t *testing.T) {
	e := newTestEnv(t)
	base := func() map[string]any {
		return map[string]any{
			"ticker": "NVDA", "kind": "catalyst", "provider": "seed", "dataState": "seed",
			"title": "Vera Rubin enters full production",
		}
	}
	undated := base()
	code, body := e.do(t, "u1", "POST", "/evidence", undated)
	if code != http.StatusBadRequest || body["code"] != "provenance_incomplete" {
		t.Fatalf("an undated catalyst must be refused, got %d %v", code, body)
	}

	viaSourceTime := base()
	viaSourceTime["sourceTime"] = 1780000000
	if code, body := e.do(t, "u1", "POST", "/evidence", viaSourceTime); code != http.StatusCreated {
		t.Fatalf("catalyst dated by sourceTime: %d %v", code, body)
	}

	viaFact := base()
	viaFact["title"] = "Rubin CPX ships"
	viaFact["fact"] = map[string]any{"key": "catalyst.product", "value": "high", "asOf": "2026-05-31"}
	if code, body := e.do(t, "u1", "POST", "/evidence", viaFact); code != http.StatusCreated {
		t.Fatalf("catalyst dated by fact.asOf: %d %v", code, body)
	}
}

// Provider is a closed set, and a transcript excerpt may only reference the transcript store — the
// structural half of D-08's "gateway-fetched page text is never persisted".
func TestEvidenceRejectsUnknownProviderAndForeignSourceStore(t *testing.T) {
	e := newTestEnv(t)

	p := deterministicFact("2026-07-29")
	p["provider"] = "some-scraper"
	code, body := e.do(t, "u1", "POST", "/evidence", p)
	if code != http.StatusBadRequest || body["code"] != "unknown_provider" {
		t.Errorf("unknown provider: want 400/unknown_provider, got %d %v", code, body)
	}

	q := map[string]any{
		"ticker": "NVDA", "kind": "transcript_excerpt", "provider": "user", "dataState": "live",
		"attribution": "Q1 call", "excerpt": "we are supply constrained",
		"sourceRef": map[string]any{"store": "gateway-fetch", "id": "https://ir.example.com/q1", "locator": ""},
	}
	code, body = e.do(t, "u1", "POST", "/evidence", q)
	if code != http.StatusBadRequest {
		t.Errorf("transcript excerpt from a non-transcript store: want 400, got %d %v", code, body)
	}

	// Unknown kinds are refused with the valid set listed, not coerced into something adjacent.
	code, body = e.do(t, "u1", "POST", "/evidence", map[string]any{
		"ticker": "NVDA", "kind": "tweet", "provider": "user", "dataState": "live",
	})
	if code != http.StatusBadRequest || body["code"] != "unknown_kind" {
		t.Errorf("unknown kind: want 400/unknown_kind, got %d %v", code, body)
	}
}

// Model output cannot enter the store as observed evidence. The one permitted inference record must
// name its model and cite the saved review it came from (§2.7).
func TestEvidenceInferenceMustCiteAReview(t *testing.T) {
	e := newTestEnv(t)

	bare := map[string]any{
		"ticker": "NVDA", "kind": "user_note", "provider": "llm", "dataState": "live",
		"note": "the model thinks margins compress", "inference": true,
	}
	if code, body := e.do(t, "u1", "POST", "/evidence", bare); code != http.StatusBadRequest {
		t.Errorf("inference without modelUsed: want 400, got %d %v", code, body)
	}

	bare["modelUsed"] = "qwen2.5:7b"
	if code, body := e.do(t, "u1", "POST", "/evidence", bare); code != http.StatusBadRequest {
		t.Errorf("inference without a review reference: want 400, got %d %v", code, body)
	}

	bare["sourceRef"] = map[string]any{"store": "journal-review", "id": "rev_1", "locator": "fnd_1"}
	if code, body := e.do(t, "u1", "POST", "/evidence", bare); code != http.StatusCreated {
		t.Errorf("a properly-sourced inference record: want 201, got %d %v", code, body)
	}

	// An inference record claiming a non-model provider is a mislabel, not a valid save.
	mislabelled := map[string]any{
		"ticker": "NVDA", "kind": "user_note", "provider": "user", "dataState": "live",
		"note": "x", "inference": true, "modelUsed": "qwen2.5:7b",
		"sourceRef": map[string]any{"store": "journal-review", "id": "rev_1", "locator": "fnd_2"},
	}
	if code, _ := e.do(t, "u1", "POST", "/evidence", mislabelled); code != http.StatusBadRequest {
		t.Errorf("inference with provider != llm: want 400, got %d", code)
	}
}

// ---- excerpt limits ----

// An over-long excerpt is TRUNCATED and flagged, never rejected: the user keeps the capture and can
// see it was shortened. Filings get the larger cap because SEC filings are US government works.
func TestEvidenceExcerptCapsTruncateNotReject(t *testing.T) {
	e := newTestEnv(t)

	long := strings.Repeat("é", 2000) // multi-byte on purpose: truncation counts runes, not bytes
	news := map[string]any{
		"ticker": "NVDA", "kind": "news", "provider": "marketaux", "dataState": "live",
		"sourceUrl": "https://example.com/one", "attribution": "Marketaux",
		"sourceTime": 1748400000, "title": "headline", "excerpt": long,
	}
	rec := mustCreate(t, e, "u1", news)
	if rec["excerptTruncated"] != true {
		t.Error("an over-long excerpt must be flagged truncated")
	}
	if n := len([]rune(rec["excerpt"].(string))); n != excerptCapNews {
		t.Errorf("news excerpt cap: want %d runes, got %d", excerptCapNews, n)
	}
	if !strings.HasPrefix(rec["excerpt"].(string), "é") {
		t.Error("truncation split a multi-byte character")
	}

	filing := map[string]any{
		"ticker": "NVDA", "kind": "filing", "provider": "sec-edgar", "dataState": "live",
		"sourceUrl": "https://sec.gov/x", "title": "8-K", "excerpt": strings.Repeat("a", 5000),
		"sourceRef": map[string]any{"store": "sec-edgar", "id": "0001045810-26-000012", "locator": ""},
	}
	frec := mustCreate(t, e, "u1", filing)
	if n := len(frec["excerpt"].(string)); n != excerptCapFiling {
		t.Errorf("filing excerpt cap: want %d, got %d", excerptCapFiling, n)
	}

	// A kind with a zero cap stores no prose at all — a computed number needs none.
	fact := deterministicFact("2026-07-28")
	fact["excerpt"] = "some narrative about the RSI"
	drec := mustCreate(t, e, "u1", fact)
	if drec["excerpt"] != "" {
		t.Errorf("deterministic_fact must carry no excerpt, got %q", drec["excerpt"])
	}
	if drec["excerptTruncated"] != true {
		t.Error("dropping an excerpt to a zero cap must still be flagged")
	}
}

// ---- immutability ----

// Only `note` is mutable. An attempt to edit capture state is refused by name, not silently ignored,
// so a caller can never believe an excerpt or a provider was rewritten.
func TestEvidenceOnlyNoteIsMutable(t *testing.T) {
	e := newTestEnv(t)
	rec := mustCreate(t, e, "u1", deterministicFact("2026-07-29"))
	id := rec["id"].(string)

	code, body := e.do(t, "u1", "PATCH", "/evidence/"+id, map[string]any{"note": "watch this into earnings"})
	if code != http.StatusOK {
		t.Fatalf("patch note: %d %v", code, body)
	}
	updated, _ := body["evidence"].(map[string]any)
	if updated["note"] != "watch this into earnings" {
		t.Errorf("note not applied: %v", updated["note"])
	}

	for _, field := range []string{"excerpt", "provider", "dataState", "fact", "capturedAt", "dedupeKey", "ticker"} {
		code, body := e.do(t, "u1", "PATCH", "/evidence/"+id, map[string]any{field: "x"})
		if code != http.StatusBadRequest || body["code"] != "immutable_field" {
			t.Errorf("patching %s: want 400/immutable_field, got %d %v", field, code, body)
		}
	}
}

// ---- duplicate-save policy ----

// Saving the same upstream item twice returns the SAME record with deduplicated:true — not an error
// and not a second copy. captureNew is the explicit escape hatch for a genuine later re-capture.
func TestEvidenceDuplicateSavePolicy(t *testing.T) {
	e := newTestEnv(t)
	news := func() map[string]any {
		return map[string]any{
			"ticker": "NVDA", "kind": "news", "provider": "marketaux", "dataState": "live",
			"sourceUrl": "https://example.com/story?utm_source=twitter&id=7", "attribution": "Marketaux",
			"sourceTime": 1748400000, "title": "NVDA beats",
		}
	}
	first := mustCreate(t, e, "u1", news())

	// Same article, different tracking parameters and a re-worded headline: still the same article.
	again := news()
	again["sourceUrl"] = "https://example.com/story?id=7&utm_campaign=x&fbclid=abc"
	again["title"] = "NVIDIA beats expectations (updated)"
	code, body := e.do(t, "u1", "POST", "/evidence", again)
	if code != http.StatusOK {
		t.Fatalf("duplicate save: want 200, got %d %v", code, body)
	}
	if body["deduplicated"] != true {
		t.Error("duplicate save must report deduplicated:true")
	}
	dup, _ := body["evidence"].(map[string]any)
	if dup["id"] != first["id"] {
		t.Errorf("duplicate save must return the existing record, got %v want %v", dup["id"], first["id"])
	}
	if dup["title"] != first["title"] {
		t.Error("a duplicate save must not rewrite the stored record from the new payload")
	}

	// An explicit re-capture creates a second record.
	forced := news()
	forced["captureNew"] = true
	code, body = e.do(t, "u1", "POST", "/evidence", forced)
	if code != http.StatusCreated || body["deduplicated"] != false {
		t.Fatalf("captureNew: want 201/deduplicated=false, got %d %v", code, body)
	}

	_, list := e.do(t, "u1", "GET", "/evidence", nil)
	if recs, _ := list["evidence"].([]any); len(recs) != 2 {
		t.Errorf("want 2 records after a forced re-capture, got %d", len(recs))
	}

	// Two user notes with identical words are two observations — notes never dedupe.
	note := map[string]any{"ticker": "NVDA", "kind": "user_note", "provider": "user", "dataState": "live", "note": "same words"}
	mustCreate(t, e, "u1", note)
	mustCreate(t, e, "u1", note)
}

// A computed fact from a different day is DIFFERENT evidence — yesterday's RSI must not dedupe into
// today's, or a decision would silently cite the wrong day's number.
func TestEvidenceComputedFactsAreDatedNotTimeless(t *testing.T) {
	e := newTestEnv(t)
	a := mustCreate(t, e, "u1", deterministicFact("2026-07-28"))
	b := mustCreate(t, e, "u1", deterministicFact("2026-07-29"))
	if a["dedupeKey"] == b["dedupeKey"] {
		t.Error("the same indicator on two different dates must not share a dedupe identity")
	}

	// The SAME date and timeframe does dedupe.
	code, body := e.do(t, "u1", "POST", "/evidence", deterministicFact("2026-07-29"))
	if code != http.StatusOK || body["deduplicated"] != true {
		t.Errorf("same fact, same date: want a dedupe hit, got %d %v", code, body)
	}

	// A different timeframe is a different observation.
	other := deterministicFact("2026-07-29")
	other["fact"].(map[string]any)["timeframe"] = "1H"
	if code, body := e.do(t, "u1", "POST", "/evidence", other); code != http.StatusCreated {
		t.Errorf("same fact on another timeframe: want 201, got %d %v", code, body)
	}
}

// ---- links and bearing ----

// A thesis holds supporting AND contradicting evidence at the same time. That is the desired state,
// and it is what "contradictory evidence per active thesis" is measured from.
func TestEvidenceLinkBearingSupportsAndContradicts(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "Datacenter demand stays supply-constrained through FY2027")

	pro := mustCreate(t, e, "u1", deterministicFact("2026-07-29"))
	con := mustCreate(t, e, "u1", map[string]any{
		"ticker": "NVDA", "kind": "news", "provider": "marketaux", "dataState": "live",
		"sourceUrl": "https://example.com/hyperscaler-capex-cut", "attribution": "Marketaux",
		"sourceTime": 1748400000, "title": "Hyperscaler trims capex guidance",
	})

	for _, tc := range []struct{ evID, bearing string }{
		{pro["id"].(string), "supports"},
		{con["id"].(string), "contradicts"},
	} {
		code, body := e.do(t, "u1", "POST", "/evidence/links", map[string]any{
			"thesisId": thesisID, "evidenceId": tc.evID, "bearing": tc.bearing,
		})
		if code != http.StatusCreated {
			t.Fatalf("link %s: %d %v", tc.bearing, code, body)
		}
		link, _ := body["link"].(map[string]any)
		if !strings.HasPrefix(link["id"].(string), "lnk_") {
			t.Errorf("link id must be prefixed lnk_, got %v", link["id"])
		}
		if link["authoredBy"] != "user" {
			t.Errorf("a link made by the user must be authoredBy user, got %v", link["authoredBy"])
		}
		if link["linkedAt"].(float64) == 0 {
			t.Error("linkedAt must be stamped")
		}
	}

	_, body := e.do(t, "u1", "GET", "/evidence/links?thesisId="+thesisID, nil)
	links, _ := body["links"].([]any)
	if len(links) != 2 {
		t.Fatalf("want 2 links on the thesis, got %d", len(links))
	}
	bearings := map[string]bool{}
	for _, l := range links {
		bearings[l.(map[string]any)["bearing"].(string)] = true
	}
	if !bearings["supports"] || !bearings["contradicts"] {
		t.Errorf("a thesis must be able to hold both bearings at once, got %v", bearings)
	}

	// Filtering evidence by thesis resolves through the links.
	_, listed := e.do(t, "u1", "GET", "/evidence?thesisId="+thesisID, nil)
	if recs, _ := listed["evidence"].([]any); len(recs) != 2 {
		t.Errorf("thesisId filter: want 2 records, got %d", len(recs))
	}
}

// Re-linking the same pair CHANGES the bearing rather than creating a second link — a user who moves
// an item from supporting to contradicting has changed their reading, not added a contradiction.
func TestEvidenceRelinkUpdatesBearing(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "claim")
	rec := mustCreate(t, e, "u1", deterministicFact("2026-07-29"))

	code, body := e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": thesisID, "evidenceId": rec["id"], "bearing": "supports",
	})
	if code != http.StatusCreated {
		t.Fatalf("first link: %d %v", code, body)
	}
	firstID := body["link"].(map[string]any)["id"]

	code, body = e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": thesisID, "evidenceId": rec["id"], "bearing": "contradicts", "note": "read it the other way",
	})
	if code != http.StatusOK || body["updated"] != true {
		t.Fatalf("re-link: want 200/updated, got %d %v", code, body)
	}
	link, _ := body["link"].(map[string]any)
	if link["id"] != firstID {
		t.Error("re-linking must reuse the existing link, not mint a new one")
	}
	if link["bearing"] != "contradicts" || link["note"] != "read it the other way" {
		t.Errorf("bearing/note not updated: %v", link)
	}

	_, listed := e.do(t, "u1", "GET", "/evidence/links?thesisId="+thesisID, nil)
	if links, _ := listed["links"].([]any); len(links) != 1 {
		t.Errorf("want exactly 1 link after a re-link, got %d", len(links))
	}
}

func TestEvidenceLinkRejectsBadBearingAndItemID(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "claim")
	rec := mustCreate(t, e, "u1", deterministicFact("2026-07-29"))

	code, body := e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": thesisID, "evidenceId": rec["id"], "bearing": "bullish",
	})
	if code != http.StatusBadRequest {
		t.Errorf("bearing outside the three-value enum: want 400, got %d %v", code, body)
	}

	code, body = e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": thesisID, "evidenceId": rec["id"], "bearing": "supports", "thesisItemId": "qst_1234",
	})
	if code != http.StatusBadRequest {
		t.Errorf("a next-question item id is not a valid link target: want 400, got %d %v", code, body)
	}

	code, _ = e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": thesisID, "evidenceId": rec["id"], "bearing": "supports", "thesisItemId": "asm_deadbeefdeadbeef",
	})
	if code != http.StatusCreated {
		t.Errorf("an assumption item id must be accepted: got %d", code)
	}
}

// Both ends of a link are resolved against the caller's own stores, and a foreign id is a 404.
func TestEvidenceLinkRejectsForeignEnds(t *testing.T) {
	e := newTestEnv(t)
	otherThesis := e.makeThesis(t, "u2", "NVDA", "someone else's claim")
	otherEvidence := mustCreate(t, e, "u2", deterministicFact("2026-07-29"))

	mine := mustCreate(t, e, "u1", deterministicFact("2026-07-28"))
	myThesis := e.makeThesis(t, "u1", "NVDA", "my claim")

	if code, _ := e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": otherThesis, "evidenceId": mine["id"], "bearing": "supports",
	}); code != http.StatusNotFound {
		t.Errorf("linking to another user's thesis: want 404, got %d", code)
	}
	if code, _ := e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": myThesis, "evidenceId": otherEvidence["id"], "bearing": "supports",
	}); code != http.StatusNotFound {
		t.Errorf("linking another user's evidence: want 404, got %d", code)
	}
}

// ---- unlink vs delete ----

// Unlinking destroys the LINK and nothing else. The evidence record survives, keeps its provenance,
// and stays available to link elsewhere.
func TestEvidenceUnlinkPreservesTheRecord(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "claim")
	rec := mustCreate(t, e, "u1", deterministicFact("2026-07-29"))

	_, body := e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": thesisID, "evidenceId": rec["id"], "bearing": "supports",
	})
	linkID := body["link"].(map[string]any)["id"].(string)

	if code, _ := e.do(t, "u1", "DELETE", "/evidence/links/"+linkID, nil); code != http.StatusOK {
		t.Fatalf("unlink: want 200, got %d", code)
	}
	_, links := e.do(t, "u1", "GET", "/evidence/links?thesisId="+thesisID, nil)
	if l, _ := links["links"].([]any); len(l) != 0 {
		t.Errorf("link should be gone, got %d", len(l))
	}
	_, listed := e.do(t, "u1", "GET", "/evidence", nil)
	recs, _ := listed["evidence"].([]any)
	if len(recs) != 1 {
		t.Fatalf("the evidence record must survive an unlink, got %d records", len(recs))
	}
	survived := recs[0].(map[string]any)
	if survived["id"] != rec["id"] || survived["provider"] != "analysis" || survived["dataState"] != "synthetic" {
		t.Errorf("the surviving record lost state: %v", survived)
	}

	// And it can be linked again afterwards.
	if code, _ := e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": thesisID, "evidenceId": rec["id"], "bearing": "context",
	}); code != http.StatusCreated {
		t.Errorf("re-linking after an unlink: want 201, got %d", code)
	}
}

// Deleting linked evidence is refused with the affected theses named. Only an explicit force=true
// cascades — so breaking a citation is always a decision.
func TestEvidenceLinkedDeletionPolicy(t *testing.T) {
	e := newTestEnv(t)
	t1 := e.makeThesis(t, "u1", "NVDA", "claim one")
	t2 := e.makeThesis(t, "u1", "NVDA", "claim two")
	rec := mustCreate(t, e, "u1", deterministicFact("2026-07-29"))
	id := rec["id"].(string)

	for _, th := range []string{t1, t2} {
		e.do(t, "u1", "POST", "/evidence/links", map[string]any{
			"thesisId": th, "evidenceId": id, "bearing": "supports",
		})
	}

	code, body := e.do(t, "u1", "DELETE", "/evidence/"+id, nil)
	if code != http.StatusConflict || body["code"] != "evidence_linked" {
		t.Fatalf("deleting linked evidence: want 409/evidence_linked, got %d %v", code, body)
	}
	if body["linkCount"].(float64) != 2 {
		t.Errorf("want linkCount 2, got %v", body["linkCount"])
	}
	ids, _ := body["thesisIds"].([]any)
	if len(ids) != 2 {
		t.Errorf("want both thesis ids reported, got %v", ids)
	}

	// Nothing was written by the refused delete.
	_, listed := e.do(t, "u1", "GET", "/evidence", nil)
	if recs, _ := listed["evidence"].([]any); len(recs) != 1 {
		t.Fatalf("a refused delete must not remove anything, got %d records", len(recs))
	}

	code, body = e.do(t, "u1", "DELETE", "/evidence/"+id+"?force=true", nil)
	if code != http.StatusOK {
		t.Fatalf("forced delete: want 200, got %d %v", code, body)
	}
	_, listed = e.do(t, "u1", "GET", "/evidence", nil)
	if recs, _ := listed["evidence"].([]any); len(recs) != 0 {
		t.Errorf("forced delete should remove the record, got %d", len(recs))
	}
	for _, th := range []string{t1, t2} {
		_, links := e.do(t, "u1", "GET", "/evidence/links?thesisId="+th, nil)
		if l, _ := links["links"].([]any); len(l) != 0 {
			t.Errorf("forced delete should cascade its links, thesis %s still has %d", th, len(l))
		}
	}
}

// A revoked link is history, not an attachment: it must not block a delete, and revoking keeps the
// record stored so the audit trail survives.
func TestEvidenceRevokedLinkDoesNotBlockDeletion(t *testing.T) {
	e := newTestEnv(t)
	thesisID := e.makeThesis(t, "u1", "NVDA", "claim")
	rec := mustCreate(t, e, "u1", deterministicFact("2026-07-29"))
	_, body := e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": thesisID, "evidenceId": rec["id"], "bearing": "supports",
	})
	linkID := body["link"].(map[string]any)["id"].(string)

	code, patched := e.do(t, "u1", "PATCH", "/evidence/links/"+linkID, map[string]any{"revoked": true})
	if code != http.StatusOK {
		t.Fatalf("revoke: %d %v", code, patched)
	}
	link, _ := patched["link"].(map[string]any)
	if link["revoked"] != true || link["revokedAt"] == nil {
		t.Errorf("a revoked link must be stamped, got %v", link)
	}

	// Revoked links are hidden from the default read but still stored.
	_, hidden := e.do(t, "u1", "GET", "/evidence/links?thesisId="+thesisID, nil)
	if l, _ := hidden["links"].([]any); len(l) != 0 {
		t.Errorf("a revoked link must not appear in the default list, got %d", len(l))
	}
	_, shown := e.do(t, "u1", "GET", "/evidence/links?thesisId="+thesisID+"&includeRevoked=true", nil)
	if l, _ := shown["links"].([]any); len(l) != 1 {
		t.Errorf("a revoked link must still be stored, got %d", len(l))
	}

	if code, _ := e.do(t, "u1", "DELETE", "/evidence/"+rec["id"].(string), nil); code != http.StatusOK {
		t.Errorf("a revoked link must not block deletion: got %d", code)
	}
}

// ---- legacy thesis compatibility ----

// A pre-existing free-text thesis — `text` only, no structured fields — is a valid link target.
// Evidence must attach to the theses users already have, not only to ones created after this change.
func TestEvidenceLinksToLegacyThesis(t *testing.T) {
	e := newTestEnv(t)

	// A v1 record written by an older binary: bare `text`, no claim, no lists, no timestamps.
	legacy := `[{"id":"9f2c1b7a4e0d5a83","ticker":"NVDA","text":"Blackwell ramp beats consensus","status":"active"}]`
	if err := os.MkdirAll(filepath.Join(e.dir, "u1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "u1", "theses.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-open so the store reads the file we just planted.
	theses, err := openThesisStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.theses = theses
	e.handler = e.srv.routes()

	rec := mustCreate(t, e, "u1", deterministicFact("2026-07-29"))
	code, body := e.do(t, "u1", "POST", "/evidence/links", map[string]any{
		"thesisId": "9f2c1b7a4e0d5a83", "evidenceId": rec["id"], "bearing": "supports",
	})
	if code != http.StatusCreated {
		t.Fatalf("linking to a legacy thesis: want 201, got %d %v", code, body)
	}

	// The legacy thesis record itself is untouched by this lane — theses.json is not ours to write.
	after, err := os.ReadFile(filepath.Join(e.dir, "u1", "theses.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != legacy {
		t.Errorf("theses.json must not be rewritten by an evidence link.\n got: %s\nwant: %s", after, legacy)
	}
}

// The unattributed pre-auth bucket stays unreachable. No route can read or write `_legacy` (D-23),
// which is enforced by construction rather than by remembering not to.
func TestEvidenceLegacyBucketUnreachable(t *testing.T) {
	e := newTestEnv(t)
	store, err := openEvidenceStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	if recs := store.listRecords(legacyBucket); len(recs) != 0 {
		t.Errorf("the legacy bucket must never be readable, got %d records", len(recs))
	}
	if _, _, err := store.addRecord(legacyBucket, EvidenceRecord{ID: "ev_x"}); err == nil {
		t.Error("writing into the legacy bucket must be refused")
	}
	if _, err := os.Stat(filepath.Join(e.dir, legacyBucket, evidenceFile)); err == nil {
		t.Error("no evidence file may be created in the legacy bucket")
	}
}

// ---- refuse to clobber ----

// A present-but-unparseable evidence.json is preserved, not replaced with a fresh empty array. The
// write is refused with a distinguishable code so the UI can say the store needs attention.
func TestEvidenceRefusesToOverwriteUnreadableStore(t *testing.T) {
	e := newTestEnv(t)
	if err := os.MkdirAll(filepath.Join(e.dir, "u1"), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := []byte(`[{"id":"ev_truncated","ticker":"NV`)
	if err := os.WriteFile(filepath.Join(e.dir, "u1", evidenceFile), broken, 0o644); err != nil {
		t.Fatal(err)
	}

	code, body := e.do(t, "u1", "POST", "/evidence", deterministicFact("2026-07-29"))
	if code != http.StatusServiceUnavailable || body["code"] != "store_unreadable" {
		t.Fatalf("want 503/store_unreadable, got %d %v", code, body)
	}
	after, err := os.ReadFile(filepath.Join(e.dir, "u1", evidenceFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, broken) {
		t.Error("an unreadable store must be left exactly as it was found")
	}
}

// ---- listing ----

func TestEvidenceListFiltersAndPagination(t *testing.T) {
	e := newTestEnv(t)
	mustCreate(t, e, "u1", deterministicFact("2026-07-27"))
	mustCreate(t, e, "u1", deterministicFact("2026-07-28"))
	mustCreate(t, e, "u1", map[string]any{
		"ticker": "GOOGL", "kind": "user_note", "provider": "user", "dataState": "live", "note": "different company",
	})

	_, byTicker := e.do(t, "u1", "GET", "/evidence?ticker=nvda", nil)
	if recs, _ := byTicker["evidence"].([]any); len(recs) != 2 {
		t.Errorf("ticker filter (case-insensitive): want 2, got %d", len(recs))
	}
	_, byKind := e.do(t, "u1", "GET", "/evidence?kind=user_note", nil)
	if recs, _ := byKind["evidence"].([]any); len(recs) != 1 {
		t.Errorf("kind filter: want 1, got %d", len(recs))
	}

	_, page := e.do(t, "u1", "GET", "/evidence?limit=2", nil)
	recs, _ := page["evidence"].([]any)
	if len(recs) != 2 {
		t.Fatalf("limit=2: want 2, got %d", len(recs))
	}
	cursor, _ := page["nextCursor"].(string)
	if cursor == "" {
		t.Fatal("a truncated page must return a cursor")
	}
	_, next := e.do(t, "u1", "GET", "/evidence?limit=2&cursor="+cursor, nil)
	if recs2, _ := next["evidence"].([]any); len(recs2) != 1 {
		t.Errorf("second page: want 1, got %d", len(recs2))
	}
}

// ---- dedupe identity unit checks ----

func TestCanonicalSourceURLStripsTracking(t *testing.T) {
	cases := [][2]string{
		{"HTTPS://Example.COM/a?utm_source=x&id=1", "https://example.com/a?id=1"},
		{"https://example.com/a?id=1&fbclid=zz", "https://example.com/a?id=1"},
		{"https://example.com/a#section", "https://example.com/a"},
		{"not a url", "not a url"},
	}
	for _, c := range cases {
		if got := canonicalSourceURL(c[0]); got != c[1] {
			t.Errorf("canonicalSourceURL(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestEvidenceDedupeIdentityPerKind(t *testing.T) {
	filing := &EvidenceRecord{
		Kind: "filing", Ticker: "NVDA", SourceURL: "https://sec.gov/x",
		SourceRef: &SourceRef{Store: "sec-edgar", ID: "0001045810-26-000012"},
	}
	if got := evidenceIdentity(filing); got != "0001045810-26-000012" {
		t.Errorf("a filing is identified by its accession number, got %q", got)
	}

	quote := &EvidenceRecord{
		Kind: "transcript_excerpt", Ticker: "NVDA",
		SourceRef: &SourceRef{Store: "llm-transcript", ID: "NVDA/Q1-FY2027", Locator: "evasiveMoments:0:answer"},
	}
	if got := evidenceIdentity(quote); got != "NVDA/Q1-FY2027#evasiveMoments:0:answer" {
		t.Errorf("a quotation is identified by snapshot + locator, got %q", got)
	}

	cat := &EvidenceRecord{Kind: "catalyst", Ticker: "NVDA", Provider: "seed", Title: "  Vera   RUBIN production  "}
	if got := evidenceIdentity(cat); got != "seed#vera rubin production" {
		t.Errorf("catalyst identity should normalize whitespace and case, got %q", got)
	}

	// A record's own id never leaks into another record's identity: two notes differ.
	n1 := &EvidenceRecord{ID: "ev_1", Kind: "user_note", Ticker: "NVDA"}
	n2 := &EvidenceRecord{ID: "ev_2", Kind: "user_note", Ticker: "NVDA"}
	if evidenceDedupeKey(n1) == evidenceDedupeKey(n2) {
		t.Error("user notes must never share a dedupe key")
	}
}

// The title is display state, so editing it upstream must NOT change what a decision cited. Identity
// is the random id; the dedupe key answers a different question.
func TestEvidenceIdentityIsNotDerivedFromTitle(t *testing.T) {
	a := &EvidenceRecord{
		Kind: "news", Ticker: "NVDA", Title: "Original headline",
		SourceURL: "https://example.com/a",
	}
	b := &EvidenceRecord{
		Kind: "news", Ticker: "NVDA", Title: "Completely rewritten headline",
		SourceURL: "https://example.com/a",
	}
	if evidenceDedupeKey(a) != evidenceDedupeKey(b) {
		t.Error("a re-worded headline must not change which upstream article a capture refers to")
	}
}
