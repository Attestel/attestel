package main

// changed_test.go — `GET /api/changed`, Wave 5 Lane 5B.
//
// NO TEST HERE TOUCHES THE NETWORK: the fixture's `deniedTransport` fails every host that is not
// one of this file's own fakes, and the per-ticker collectors (analysis, alerts, journal) are
// therefore genuinely unreachable. That is not a gap — it is the degraded path, and asserting that
// the route still answers with truthful `degraded` markers is one of the things this file is for.
//
// THE ASSERTION THAT MATTERS MOST is the shape one. Three Wave 4 lanes rendered this route's
// absence, and `WhatChangedPanel` read its item shape defensively across two vocabularies
// (`researchLink.ticker || ticker || subject`, `bearing || direction`) precisely because the route
// did not exist to measure. `TestPayloadMatchesWhatTheFrontendReadsFirst` is that measurement.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func changedBody(t *testing.T, f *feedsFixture, path, uid string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if uid != "" {
		req.AddCookie(&http.Cookie{Name: f.srv.cfg.CookieName,
			Value: testSessionToken(f.srv.cfg.Secret, uid)})
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: body is not JSON (%v): %s", path, err, rec.Body.String())
	}
	return body
}

func num(t *testing.T, body map[string]any, key string) float64 {
	t.Helper()
	v, ok := body[key].(float64)
	if !ok {
		t.Fatalf("%s missing or not a number: %v", key, body[key])
	}
	return v
}

// ───────────────────────────────────────────────────────────── the route exists and answers

func TestChangedIsServedAndReportsChangeNotVolume(t *testing.T) {
	// Three events over two followed companies. Two are material (importance >= 0.70); the third
	// is not, and it still contributes its documents to the "what we read" number.
	ev := newEventsFake(t, []map[string]any{
		fixtureEvent("evt_0000000000000001", "filing_8k", "2026-08-20T10:00:00Z", 0.90, 0.5,
			"official", 12, []map[string]any{rawTicker("NVDA", 0.9, true)}),
		fixtureEvent("evt_0000000000000002", "product_launch", "2026-08-20T09:00:00Z", 0.75, 0.5,
			"professional", 30, []map[string]any{rawTicker("AMD", 0.8, true)}),
		fixtureEvent("evt_0000000000000003", "analyst_action", "2026-08-20T08:00:00Z", 0.20, 0.1,
			"professional", 45, []map[string]any{rawTicker("NVDA", 0.3, false)}),
	})
	journal := journalFake(t, map[string][]string{"u1": {"NVDA", "AMD"}})
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	body := changedBody(t, f, "/api/changed", "u1")

	// 12 + 30 + 45 — what it READ. Small, mono, dim on screen; the largest number in the payload.
	if got := num(t, body, "documentsProcessed"); got != 87 {
		t.Fatalf("documentsProcessed = %v, want 87", got)
	}
	// What it CONCLUDED. The two heading-scale numbers.
	if got := num(t, body, "materialEvents"); got != 2 {
		t.Fatalf("materialEvents = %v, want 2 (only importanceBand high counts)", got)
	}
	if got := num(t, body, "companiesChanged"); got != 2 {
		t.Fatalf("companiesChanged = %v, want 2", got)
	}
	if body["empty"] != false {
		t.Fatalf("empty = %v, want false — two material events is a change", body["empty"])
	}
}

func TestChangedServesTheMaterialityDefinitionRatherThanLettingTheClientKeepOne(t *testing.T) {
	// §AD-8's lesson from Wave 4 integration: two lanes hand-copied `importanceHighMin` into
	// JavaScript and one copy had already drifted. The word "material" in a payload is a claim, and
	// a claim needs its definition served beside it.
	f := newFeedsFixture(t, "", "")
	body := changedBody(t, f, "/api/changed", "")
	m, ok := body["materiality"].(map[string]any)
	if !ok {
		t.Fatalf("no materiality block: %v", body)
	}
	if m["importanceBand"] != "high" {
		t.Fatalf("materiality.importanceBand = %v, want high", m["importanceBand"])
	}
	if m["importanceMin"] != importanceHighMin {
		t.Fatalf("materiality.importanceMin = %v, want %v (the SERVED constant, not a copy)",
			m["importanceMin"], importanceHighMin)
	}
	if m["movePct"] != materialMovePct {
		t.Fatalf("materiality.movePct = %v, want %v", m["movePct"], materialMovePct)
	}
}

// ───────────────────────────────────────────────────────────── the boundary and its copy rule

func TestSinceBasisIsServedSoTheHeadingCanNeverLie(t *testing.T) {
	// Wave 4 Lane 4A's locked decision 6: a default 24-hour window may NEVER be labelled "since
	// your last check". The client needs to tell the two apart without re-deriving the rule, so the
	// BASIS is served rather than inferred from whether `since` happened to be echoed.
	f := newFeedsFixture(t, "", "")

	body := changedBody(t, f, "/api/changed", "")
	since, _ := body["since"].(map[string]any)
	if since["basis"] != "default24h" {
		t.Fatalf("since.basis = %v, want default24h", since["basis"])
	}

	body = changedBody(t, f, "/api/changed?since=1780000000", "")
	since, _ = body["since"].(map[string]any)
	if since["basis"] != "requested" || since["at"] != float64(1780000000) {
		t.Fatalf("since = %v, want {at:1780000000, basis:requested}", since)
	}
}

func TestAnUnparseableSinceFallsBackToTheWindowAndSaysSo(t *testing.T) {
	// A stale bookmark must not 400 the landing surface. It must also not be silently honoured as
	// "your last check", which is why the basis flips back to `default24h`.
	f := newFeedsFixture(t, "", "")
	for _, raw := range []string{"tomorrow", "-5", "0", ""} {
		body := changedBody(t, f, "/api/changed?since="+raw, "")
		since, _ := body["since"].(map[string]any)
		if since["basis"] != "default24h" {
			t.Fatalf("since=%q gave basis %v, want default24h", raw, since["basis"])
		}
	}
}

// ───────────────────────────────────────────────────────────── guests, and honest degradation

func TestAGuestIsServedRatherThanChallenged(t *testing.T) {
	// The panel renders on the opening screen. A 401 here would turn it into a login wall for a
	// surface that has something honest to say without one.
	f := newFeedsFixture(t, "", "")
	req := httptest.NewRequest(http.MethodGet, "/api/changed", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("guest GET /api/changed = %d, want 200", rec.Code)
	}
}

func TestUnreachableSourcesDegradeAndAreNamed(t *testing.T) {
	// Every collector here is behind `deniedTransport`, so all of them fail. The route must still
	// answer, and it must NAME what it could not read — an answer that hid the gap would be
	// indistinguishable from "nothing changed", which is the exact lie 4A's copy refused to tell.
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	f := newFeedsFixture(t, "", journal.URL)
	body := changedBody(t, f, "/api/changed", "u1")

	degraded := degradedOf(t, body)
	for _, want := range []string{"market_context", "transcripts"} {
		if !hasString(degraded, want) {
			t.Fatalf("degraded = %v, missing %q", degraded, want)
		}
	}
	if _, ok := body["items"].([]any); !ok {
		t.Fatalf("items must be an array even when every source degraded: %v", body["items"])
	}
}

func TestNothingChangedIsAFirstClassAnswer(t *testing.T) {
	ev := newEventsFake(t, nil)
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	f := newFeedsFixture(t, ev.URL(), journal.URL)
	body := changedBody(t, f, "/api/changed", "u1")

	if num(t, body, "materialEvents") != 0 || num(t, body, "companiesChanged") != 0 {
		t.Fatalf("expected a zero-change answer, got %v", body)
	}
	if body["empty"] != true {
		t.Fatalf("empty = %v, want true — an honest empty answer is a feature, and the panel "+
			"renders it calmly rather than as an error", body["empty"])
	}
}

// ───────────────────────────────────────────────────────────── the shape 4A guessed at

func TestPayloadMatchesWhatTheFrontendReadsFirst(t *testing.T) {
	// `WhatChangedPanel.subjectOf` reads `researchLink.ticker || ticker || subject`, and
	// `bearingOf` reads `bearing || direction`. Those fallbacks exist because the route did not
	// exist to measure. This asserts the FIRST spelling in each pair is the one that arrives, so
	// the fallbacks are dead spellings rather than load-bearing guesses.
	item := ChangeItem{
		ID: "chg_x", Kind: changeMarketContext, At: 1780000000,
		Summary: "NVDA closed 6.2% higher since your last review",
		Bearing: optString("strengthens"), DataState: dataStateLive,
	}
	out := retargetToCompany([]ChangeItem{item}, "nvda")

	buf, err := json.Marshal(out[0])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatal(err)
	}
	link, ok := got["researchLink"].(map[string]any)
	if !ok || link["ticker"] != "NVDA" {
		t.Fatalf("researchLink.ticker = %v, want NVDA — the field the row label reads first", got["researchLink"])
	}
	if got["bearing"] != "strengthens" {
		t.Fatalf("bearing = %v, want strengthens", got["bearing"])
	}
	if got["summary"] == "" || got["kind"] != changeMarketContext {
		t.Fatalf("summary/kind wrong: %v", got)
	}
	// And the panel's fallback spellings are genuinely absent, so a future reader does not conclude
	// the payload speaks both vocabularies.
	for _, dead := range []string{"ticker", "subject", "direction"} {
		if _, present := got[dead]; present {
			t.Fatalf("payload carries %q — 4A's fallback vocabulary must NOT be served as well, "+
				"or the two spellings will drift", dead)
		}
	}
}

func TestAPortfolioItemLinksToTheCompanyNotToAThesisThatDoesNotExist(t *testing.T) {
	// `changeLinkFor(ticker, "")` would produce subview "thesis" with an empty thesisId — a link to
	// nothing. On this feed the subject is the company.
	link := companyOrThesisLink("NVDA", "")
	if link == nil || link.Subview != "overview" || link.ThesisID != "" {
		t.Fatalf("company link = %+v, want an overview link", link)
	}
	// When the source DOES name a thesis, the thesis link is kept — the item is about that thesis.
	link = companyOrThesisLink("NVDA", "th_1")
	if link == nil || link.Subview != "thesis" || link.ThesisID != "th_1" {
		t.Fatalf("thesis link = %+v, want the thesis link preserved", link)
	}
	// A macro item with no company has no company page to open, and `nil` is the honest answer.
	if companyOrThesisLink("", "") != nil {
		t.Fatal("a macro item with no ticker must not be given a company link")
	}
}

// ───────────────────────────────────────────────────────────── the counting rules

func TestDocumentsCountSourceDocumentsNotItems(t *testing.T) {
	three := 3
	docs, _, _ := summariseChange([]feedItem{
		{DocumentCount: &three, ImportanceBand: "low"},
		{DocumentCount: &three, ImportanceBand: "low"},
	})
	if docs != 6 {
		t.Fatalf("documents = %d, want 6 — one event can cluster many articles", docs)
	}
}

func TestAnItemWithNoDocumentCountCountsAsOneNotZero(t *testing.T) {
	// The degraded news/seed paths have no cluster and therefore no count. Zero would understate
	// the reading behind the conclusions; a guess would overstate it. It is the one document it is.
	docs, _, _ := summariseChange([]feedItem{{ImportanceBand: "low"}, {ImportanceBand: "low"}})
	if docs != 2 {
		t.Fatalf("documents = %d, want 2", docs)
	}
}

func TestOnlyFollowedCompaniesOnMaterialEventsCount(t *testing.T) {
	_, material, companies := summariseChange([]feedItem{
		{ImportanceBand: "high", Tickers: []feedTicker{
			{Ticker: "NVDA", Followed: true}, {Ticker: "TSM", Followed: false}}},
		{ImportanceBand: "medium", Tickers: []feedTicker{{Ticker: "AMD", Followed: true}}},
	})
	if material != 1 {
		t.Fatalf("material = %d, want 1 — `medium` is not material", material)
	}
	if fmt.Sprint(companies) != "[NVDA]" {
		t.Fatalf("companies = %v, want [NVDA]: a company the user does not follow did not change "+
			"THEIR portfolio, and an unfollowed name on a material event is not their news",
			companies)
	}
}

func TestTheSameCompanyOnTwoMaterialEventsIsCountedOnce(t *testing.T) {
	_, material, companies := summariseChange([]feedItem{
		{ImportanceBand: "high", Tickers: []feedTicker{{Ticker: "NVDA", Followed: true}}},
		{ImportanceBand: "high", Tickers: []feedTicker{{Ticker: "NVDA", Followed: true}}},
	})
	if material != 2 || len(companies) != 1 {
		t.Fatalf("material=%d companies=%v — two events, one company", material, companies)
	}
}

// ───────────────────────────────────────────────────────────── the fan-out is bounded

func TestThePerTickerFanOutIsBoundedNotJustCapped(t *testing.T) {
	// Two DIFFERENT bounds doing two different jobs, and it is worth an assertion because losing
	// the second one is invisible until a user with a real portfolio opens the page:
	// `changedMaxTickers` limits the TOTAL work, `changedFanOut` limits the BURST. Unbounded, 25
	// followed tickers means 50 simultaneous requests to the analysis service per visitor — the
	// same unbudgeted fan-out §9.24 forbids the thesis monitor.
	if changedFanOut <= 0 || changedFanOut >= changedMaxTickers {
		t.Fatalf("changedFanOut = %d — it must be a real bound below changedMaxTickers (%d), or "+
			"the burst is only limited by the total", changedFanOut, changedMaxTickers)
	}

	source, err := os.ReadFile("changed.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "gate <- struct{}{}") {
		t.Fatal("the semaphore is gone from portfolioChangeItems — every collector would run at once")
	}
}

// ───────────────────────────────────────────────────────────── invariant #4

func TestChangedNeverNamesTheLLM(t *testing.T) {
	// Invariant #4 forbids a model call caused by a page load, and this route is on the landing
	// surface. `changes.go`'s optional summary is reachable only from a POST; nothing here may go
	// near it. Source-level, in the style `alerts/eval_test.go` already uses.
	source, err := os.ReadFile("changed.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"LLMURL", "LLM_URL", "callChangeSummary", "/chat", "qwen"} {
		if strings.Contains(string(source), banned) {
			t.Fatalf("changed.go names %q — a model call on a page-load route violates invariant #4",
				banned)
		}
	}
}
