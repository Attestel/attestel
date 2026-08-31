package main

// explore_test.go — Wave 2 Lane 2C.
//
// The ranking is tested as the PURE FUNCTION AD-7 requires it to be: `rankExplore` takes the events,
// the follow set and `now` as arguments and does no I/O, so a fixture in and an ordering out is the
// whole contract. The handler tests on top of that cover the degraded path, the per-user cache key
// and the negative that matters most — that nothing here reaches a model.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// exploreNow is the fixed clock every pure-function test scores against. A ranking that depended on
// the wall clock could not be reproduced for the benchmark, which is one of AD-7's three reasons for
// existing.
var exploreNow = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

// asCanonical round-trips a fixture map through the same JSON decoding a real :8004 response goes
// through, so a field the struct silently drops fails here rather than in production.
func asCanonical(t *testing.T, fixtures []map[string]any) []canonicalEvent {
	t.Helper()
	raw, err := json.Marshal(fixtures)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	var out []canonicalEvent
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return out
}

func testFollowSet(tickers ...string) followSet {
	f := followSet{set: map[string]bool{}, source: "subscriptions"}
	for _, t := range tickers {
		f.set[t] = true
		f.tickers = append(f.tickers, t)
	}
	return f
}

func allItems(s exploreSections) []exploreItem {
	var out []exploreItem
	out = append(out, s.ForYou...)
	out = append(out, s.MarketMoving...)
	out = append(out, s.RelatedToYourCompanies...)
	out = append(out, s.Earnings...)
	out = append(out, s.Macro...)
	out = append(out, s.Trending...)
	return out
}

// ─────────────────────────────────────────────────────────────────── the versioned constants

// TestExploreWeightsSumToOne: the score is published on every item and compared across requests, so
// it has to live in a fixed range. Weights summing to 1 with every term clamped to [0,1] is what
// puts it in [0,1] without a batch-dependent normalisation pass.
func TestExploreWeightsSumToOne(t *testing.T) {
	sum := exploreW1MarketImportance + exploreW2Freshness + exploreW3Novelty +
		exploreW4SourceTier + exploreW5RelationToFollowed + exploreW6SectorSimilarity
	if math.Abs(sum-1.0) > 1e-9 {
		t.Fatalf("w1..w6 sum to %v, want 1.0 — the score must be in [0,1]", sum)
	}
	if exploreWeightsVersion == "" {
		t.Fatal("exploreWeightsVersion must be set; a benchmark run is recorded against it")
	}
}

// TestFreshnessDecaysOnTheNamedHalfLife: the decay is a stated constant, not a feeling.
func TestFreshnessDecaysOnTheNamedHalfLife(t *testing.T) {
	now := exploreNow
	if got := freshnessOf(now.Unix(), now); got != 1 {
		t.Fatalf("freshness at age 0 = %v, want 1", got)
	}
	half := now.Add(-time.Duration(freshnessHalfLifeHours) * time.Hour).Unix()
	if got := freshnessOf(half, now); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("freshness at one half-life = %v, want 0.5", got)
	}
	if got := freshnessOf(0, now); got != 0 {
		t.Fatalf("freshness of an unparseable timestamp = %v, want 0", got)
	}
}

// ─────────────────────────────────────────────────────────────────────── the score is pure

// TestExploreScoreIsAPureFunction — AD-7's first reason for existing. Same fixture, same clock,
// same follow set ⇒ byte-identical output, twice. Without this the ranking cannot be reproduced and
// therefore cannot be evaluated.
func TestExploreScoreIsAPureFunction(t *testing.T) {
	// Several events share every term but their id, so any reliance on map iteration order or on an
	// unstable sort shows up here rather than intermittently in production.
	var fixtures []map[string]any
	for i := 0; i < 12; i++ {
		fixtures = append(fixtures, fixtureEvent(fmt.Sprintf("evt_%02d", i), "product_launch",
			"2026-08-19T10:00:00Z", 0.6, 0.6, "professional", 2,
			[]map[string]any{rawTicker("AMD", 1.0, true)}))
	}
	events := asCanonical(t, fixtures)
	follow := testFollowSet("NVDA")
	srv := &Server{cfg: loadConfig(), cache: newCache(), http: &http.Client{}}

	first, _ := json.Marshal(srv.rankExplore(events, follow, nil, exploreNow, "", 10))
	second, _ := json.Marshal(srv.rankExplore(events, follow, nil, exploreNow, "", 10))
	if string(first) != string(second) {
		t.Fatal("two runs over one fixture produced different output — the ranking is not a pure function")
	}
}

// TestUnknownSectorScoresZeroNotAHalf: the tempting default — "probably somewhat related" — would
// invent affinity to make a section look full, which is how a discovery feed becomes a random feed
// with a confident explanation attached.
func TestUnknownSectorScoresZeroNotAHalf(t *testing.T) {
	events := asCanonical(t, []map[string]any{
		fixtureEvent("evt_unknown", "product_launch", "2026-08-19T10:00:00Z", 0.5, 0.5,
			"professional", 1, []map[string]any{rawTicker("ZZZZ", 1.0, true)}),
		fixtureEvent("evt_known", "product_launch", "2026-08-19T10:00:00Z", 0.5, 0.5,
			"professional", 1, []map[string]any{rawTicker("AMD", 1.0, true)}),
	})
	follow := testFollowSet("NVDA") // semiconductors

	if _, ok := sectorOf["ZZZZ"]; ok {
		t.Fatal("the fixture assumes ZZZZ is not in sectorOf")
	}
	followedSectors := map[string]bool{"semiconductors": true}

	unknown := exploreScoreTerms(events[0], follow, map[string]bool{}, followedSectors, exploreNow)
	if unknown.SectorSimilarity != 0 {
		t.Fatalf("sectorSimilarity for an unknown ticker = %v, want 0", unknown.SectorSimilarity)
	}
	known := exploreScoreTerms(events[1], follow, map[string]bool{}, followedSectors, exploreNow)
	if known.SectorSimilarity != exploreSectorSame {
		t.Fatalf("sectorSimilarity for a same-sector ticker = %v, want %v",
			known.SectorSimilarity, exploreSectorSame)
	}
}

// TestRelationRungsAreMeasuredNotAsserted: 1.0 for a direct touch, 0.5 only when the co-occurrence
// was actually observed in the window, 0 otherwise.
func TestRelationRungsAreMeasuredNotAsserted(t *testing.T) {
	// TSM appears alongside NVDA on one event, which is what earns it the 0.5 rung on another.
	events := asCanonical(t, []map[string]any{
		fixtureEvent("evt_pair", "supply_chain", "2026-08-19T10:00:00Z", 0.5, 0.5, "professional", 1,
			[]map[string]any{rawTicker("NVDA", 1.0, true), rawTicker("TSM", 0.5, false)}),
		fixtureEvent("evt_tsm", "earnings_result", "2026-08-19T11:00:00Z", 0.5, 0.5, "professional", 1,
			[]map[string]any{rawTicker("TSM", 1.0, true)}),
		fixtureEvent("evt_far", "legal_action", "2026-08-19T11:00:00Z", 0.5, 0.5, "professional", 1,
			[]map[string]any{rawTicker("XOM", 1.0, true)}),
	})
	follow := testFollowSet("NVDA")
	co := cooccurrenceSet(events, follow)
	if !co["TSM"] {
		t.Fatal("TSM appears alongside NVDA and must be in the co-occurrence set")
	}
	if co["XOM"] {
		t.Fatal("XOM never appears with a followed company and must not be in the set")
	}
	if co["NVDA"] {
		t.Fatal("a followed company is not its own co-occurrence")
	}

	sectors := map[string]bool{"semiconductors": true}
	if got := exploreScoreTerms(events[0], follow, co, sectors, exploreNow).RelationToFollowed; got != exploreRelationDirect {
		t.Fatalf("direct touch = %v, want %v", got, exploreRelationDirect)
	}
	if got := exploreScoreTerms(events[1], follow, co, sectors, exploreNow).RelationToFollowed; got != exploreRelationCooccurred {
		t.Fatalf("co-occurring touch = %v, want %v", got, exploreRelationCooccurred)
	}
	if got := exploreScoreTerms(events[2], follow, co, sectors, exploreNow).RelationToFollowed; got != 0 {
		t.Fatalf("unrelated event = %v, want 0", got)
	}
}

// ──────────────────────────────────────────────────── every item explains itself, or it is dropped

// TestEveryExploreItemExplainsItself — contract §8 and doc §16.7. Both strings are mandatory; an
// unexplained recommendation is the exact failure mode §16.7 names.
func TestEveryExploreItemExplainsItself(t *testing.T) {
	events := asCanonical(t, []map[string]any{
		fixtureEvent("evt_a", "earnings_result", "2026-08-19T10:00:00Z", 0.9, 0.8, "official", 5,
			[]map[string]any{rawTicker("TSM", 1.0, true), rawTicker("NVDA", 0.5, false)}),
		fixtureEvent("evt_b", "macro_release", "2026-08-19T11:00:00Z", 0.8, 0.4, "official", 3,
			[]map[string]any{}),
		fixtureEvent("evt_c", "product_launch", "2026-08-18T11:00:00Z", 0.4, 0.9, "professional", 1,
			[]map[string]any{rawTicker("AMD", 1.0, true)}),
	})
	srv := &Server{cfg: loadConfig(), cache: newCache(), http: &http.Client{}}
	sections := srv.rankExplore(events, testFollowSet("NVDA"), nil, exploreNow, "", 10)

	got := allItems(sections)
	if len(got) == 0 {
		t.Fatal("no items ranked — the fixture is meant to populate several sections")
	}
	for _, it := range got {
		if it.WhyYouAreSeeingThis == "" {
			t.Fatalf("%s ships with an empty whyYouAreSeeingThis", it.ID)
		}
		if it.PossibleReadThrough == "" {
			t.Fatalf("%s ships with an empty possibleReadThrough", it.ID)
		}
		// §9.18: the disclosure travels with the read-through it qualifies.
		if it.Disclaimer != analystDisclaimer {
			t.Fatalf("%s carries a possibleReadThrough without the disclosure string", it.ID)
		}
		if it.WeightsVersion != exploreWeightsVersion {
			t.Fatalf("%s is not stamped with the weights version it was scored under", it.ID)
		}
		if it.SectionRule == "" {
			t.Fatalf("%s does not say which rule put it in %s", it.ID, it.Section)
		}
	}
}

// TestReadThroughFallsBackWithoutTransmission — §9.32, and the clause this lane had to make
// actually work.
//
// MEASURED: `affectedDimensions` is itself enrichment-gated in `services/events/app/events.py` (it
// is emitted only inside `if enriched:`, and `_link_tickers` writes '[]' at ingest), so with the
// shipped default `EVENT_ENRICH_ENABLED=false` the literal fallback — "eventType plus the leading
// affectedDimensions entry" — has NO input either, and every Explore item would be dropped. That is
// the outcome §9.32 exists to prevent. The third stage derives the dimension from the closed §3.4
// enum, and `readThroughBasis` reports which stage produced the sentence.
func TestReadThroughFallsBackWithoutTransmission(t *testing.T) {
	follow := testFollowSet("NVDA")

	// Stage 3 — the shipped default: raw links, no transmission, no dimensions.
	raw := asCanonical(t, []map[string]any{
		fixtureEvent("evt_raw", "product_launch", "2026-08-19T10:00:00Z", 0.5, 0.5, "professional", 1,
			[]map[string]any{rawTicker("AMD", 1.0, true)}),
	})[0]
	text, basis := possibleReadThrough(raw, follow)
	if text == "" {
		t.Fatal("an unenriched event produced no possibleReadThrough — §9.32 forbids exactly this")
	}
	if basis != "eventType" {
		t.Fatalf("basis = %q, want %q for an event with neither transmission nor dimensions", basis, "eventType")
	}
	if text != "Product launch — relevant to growth." {
		t.Fatalf("possibleReadThrough = %q, want the deterministic eventType composition", text)
	}

	// Stage 2 — §9.32's literal form, when dimensions do exist.
	dims := asCanonical(t, []map[string]any{
		fixtureEvent("evt_dims", "product_launch", "2026-08-19T10:00:00Z", 0.5, 0.5, "professional", 1,
			[]map[string]any{enrichedTicker("AMD", 1.0, true, []string{"margin", "growth"}, "")}),
	})[0]
	text, basis = possibleReadThrough(dims, follow)
	if basis != "dimensions" || text != "Product launch — relevant to margins." {
		t.Fatalf("got (%q, %q), want the leading affectedDimensions entry", text, basis)
	}

	// Stage 1 — the model's own sentence, when enrichment ran. Preferred for a FOLLOWED company.
	trans := asCanonical(t, []map[string]any{
		fixtureEvent("evt_trans", "supply_chain", "2026-08-19T10:00:00Z", 0.5, 0.5, "professional", 1,
			[]map[string]any{
				enrichedTicker("AMD", 1.0, true, []string{"supply"}, "AMD's foundry allocation moves."),
				enrichedTicker("NVDA", 0.7, false, []string{"supply"}, "NVDA shares the same foundry."),
			}),
	})[0]
	text, basis = possibleReadThrough(trans, follow)
	if basis != "transmission" || text != "NVDA shares the same foundry." {
		t.Fatalf("got (%q, %q), want the followed company's transmission", text, basis)
	}
}

// TestUnexplainableItemIsDroppedNotBlanked — locked decision #4. An `other`-typed event with no
// relation, no notable importance and no breadth has genuinely nothing to say, and shipping it with
// an empty string would be worse than not shipping it.
func TestUnexplainableItemIsDroppedNotBlanked(t *testing.T) {
	events := asCanonical(t, []map[string]any{
		fixtureEvent("evt_mute", "other", "2026-08-19T10:00:00Z", 0.1, 0.1, "professional", 1,
			[]map[string]any{rawTicker("ZZZZ", 1.0, true)}),
	})
	srv := &Server{cfg: loadConfig(), cache: newCache(), http: &http.Client{}}
	sections := srv.rankExplore(events, testFollowSet("NVDA"), nil, exploreNow, "", 10)
	if got := allItems(sections); len(got) != 0 {
		t.Fatalf("items = %d, want 0 — an item that cannot explain itself is dropped", len(got))
	}
}

// ────────────────────────────────────────────────────────────────────────── section assignment

// TestFollowedPrimariesStayOutOfForYou — doc §15.3. Explore is discovery OUTSIDE the watchlist; an
// event about a company already followed belongs in Following.
func TestFollowedPrimariesStayOutOfForYou(t *testing.T) {
	events := asCanonical(t, []map[string]any{
		fixtureEvent("evt_own", "earnings_result", "2026-08-20T10:00:00Z", 0.95, 0.95, "official", 9,
			[]map[string]any{rawTicker("NVDA", 1.0, true)}),
		fixtureEvent("evt_other", "earnings_result", "2026-08-20T10:00:00Z", 0.5, 0.5, "professional", 1,
			[]map[string]any{rawTicker("AMD", 1.0, true)}),
	})
	srv := &Server{cfg: loadConfig(), cache: newCache(), http: &http.Client{}}
	sections := srv.rankExplore(events, testFollowSet("NVDA"), nil, exploreNow, "", 10)

	for _, it := range sections.ForYou {
		if it.ID == "evt_own" {
			t.Fatal("an event whose primary company the user already follows reached forYou")
		}
	}
	// It is still the top-scoring event, so it must not vanish — it lands in the sections whose
	// rules it satisfies.
	found := false
	for _, it := range allItems(sections) {
		if it.ID == "evt_own" {
			found = true
		}
	}
	if !found {
		t.Fatal("evt_own disappeared entirely; only forYou excludes followed primaries")
	}
	// relatedToYourCompanies is "reaches a company you follow, but is not about one".
	for _, it := range sections.RelatedToYourCompanies {
		if it.ID == "evt_own" {
			t.Fatal("an event ABOUT a followed company reached relatedToYourCompanies")
		}
	}
}

// TestSectionRuleTableIsDeterministic: an event qualifying for many sections lands in at most two,
// chosen by the declared priority order rather than by the order the predicates happen to be
// written in.
func TestSectionRuleTableIsDeterministic(t *testing.T) {
	// Qualifies for forYou, related, earnings, marketMoving AND trending.
	events := asCanonical(t, []map[string]any{
		fixtureEvent("evt_all", "earnings_result", "2026-08-20T11:00:00Z", 0.95, 0.9, "official", 8,
			[]map[string]any{rawTicker("AMD", 1.0, true), rawTicker("NVDA", 0.6, false)}),
	})
	srv := &Server{cfg: loadConfig(), cache: newCache(), http: &http.Client{}}
	sections := srv.rankExplore(events, testFollowSet("NVDA"), nil, exploreNow, "", 10)

	count := 0
	for _, it := range allItems(sections) {
		if it.ID == "evt_all" {
			count++
		}
	}
	if count != exploreMaxSectionsPerItem {
		t.Fatalf("evt_all appears in %d sections, want %d", count, exploreMaxSectionsPerItem)
	}
	if len(sections.ForYou) != 1 || sections.ForYou[0].ID != "evt_all" {
		t.Fatalf("forYou = %v, want evt_all first in the declared priority order", sections.ForYou)
	}
	if len(sections.RelatedToYourCompanies) != 1 {
		t.Fatalf("relatedToYourCompanies = %d, want the second-priority placement",
			len(sections.RelatedToYourCompanies))
	}
}

// TestExploreSectionFilterAndLimit: `section?` narrows to one section; `limit?` caps each.
func TestExploreSectionFilterAndLimit(t *testing.T) {
	var fixtures []map[string]any
	for i := 0; i < 8; i++ {
		fixtures = append(fixtures, fixtureEvent(fmt.Sprintf("evt_%02d", i), "earnings_result",
			"2026-08-20T10:00:00Z", 0.5, 0.5, "professional", 1,
			[]map[string]any{rawTicker("AMD", 1.0, true)}))
	}
	events := asCanonical(t, fixtures)
	srv := &Server{cfg: loadConfig(), cache: newCache(), http: &http.Client{}}

	only := srv.rankExplore(events, testFollowSet("NVDA"), nil, exploreNow, exploreSectionEarnings, 3)
	if len(only.Earnings) != 3 {
		t.Fatalf("earnings = %d, want 3 (the limit)", len(only.Earnings))
	}
	if len(only.ForYou) != 0 || len(only.Trending) != 0 {
		t.Fatal("section=earnings must leave the other sections empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────── the handler

// TestExploreSectionsAreAlwaysAllSix: contract §8 names six keys. A section that vanishes when it
// is empty is a section the frontend has to special-case.
func TestExploreSectionsAreAlwaysAllSix(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, []map[string]any{})
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/explore", "u1")
	sections, ok := body["sections"].(map[string]any)
	if !ok {
		t.Fatalf("no sections object: %v", body)
	}
	for _, key := range []string{"forYou", "marketMoving", "relatedToYourCompanies",
		"earnings", "macro", "trending"} {
		v, present := sections[key]
		if !present {
			t.Fatalf("section %q is missing from the response", key)
		}
		if v == nil {
			t.Fatalf("section %q is null; an empty section is [], not null", key)
		}
	}
	if body["weightsVersion"] != exploreWeightsVersion {
		t.Fatalf("weightsVersion = %v, want %q", body["weightsVersion"], exploreWeightsVersion)
	}
}

// TestExploreDegradesToEmptySectionsNotToHeadlines — locked decision #3.
//
// Four of the six terms (importance, novelty, sourceTier, documentCount) do not exist for a
// `newsFor` headline, so a fallback ranking would be three-quarters invented while looking
// identical to the real one. Following degrades to headlines because a headline IS news; Explore
// does not, because a headline is not a scored event.
func TestExploreDegradesToEmptySectionsNotToHeadlines(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, nil)
	ev.status = http.StatusInternalServerError
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	rec, body := f.get(t, "/api/explore", "u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a degraded marker", rec.Code)
	}
	if d := degradedOf(t, body); !hasString(d, degradedEventsUnreachable) {
		t.Fatalf("degraded = %v, want %q", d, degradedEventsUnreachable)
	}
	sections, _ := body["sections"].(map[string]any)
	for key, v := range sections {
		if arr, _ := v.([]any); len(arr) != 0 {
			t.Fatalf("section %q has %d items with :8004 down — Explore must not rank headlines",
				key, len(arr))
		}
	}
}

// A process-start race must heal on the next request. Before this regression guard, the first
// events:unreachable response was cached for NewsTTL (30 minutes), long after :8004 had recovered.
func TestExploreDoesNotCacheTransientEventsOutage(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, []map[string]any{})
	ev.status = http.StatusServiceUnavailable
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	first, body := f.get(t, "/api/explore", "u1")
	if first.Header().Get("X-Cache") != "BYPASS" {
		t.Fatalf("first X-Cache = %q, want BYPASS", first.Header().Get("X-Cache"))
	}
	if d := degradedOf(t, body); !hasString(d, degradedEventsUnreachable) {
		t.Fatalf("first degraded = %v, want %q", d, degradedEventsUnreachable)
	}

	ev.status = 0
	second, body := f.get(t, "/api/explore", "u1")
	if second.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("recovery X-Cache = %q, want MISS", second.Header().Get("X-Cache"))
	}
	if d := degradedOf(t, body); hasString(d, degradedEventsUnreachable) {
		t.Fatalf("recovery still reports cached outage: %v", d)
	}
}

// TestExploreCacheIsPerUser — invariant #5. The ranking is a function of the caller's follow graph,
// so a key without the uid hands one user another user's discovery feed.
func TestExploreCacheIsPerUser(t *testing.T) {
	journal := journalFake(t, map[string][]string{"alice": {"NVDA"}, "bob": {"XOM"}})
	ev := newEventsFake(t, []map[string]any{
		fixtureEvent("evt_a", "product_launch", "2026-08-19T10:00:00Z", 0.6, 0.6, "official", 2,
			[]map[string]any{rawTicker("AMD", 1.0, true)}),
	})
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	recA, bodyA := f.get(t, "/api/explore", "alice")
	if recA.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("alice's first call = %q, want MISS", recA.Header().Get("X-Cache"))
	}
	if rec, _ := f.get(t, "/api/explore", "alice"); rec.Header().Get("X-Cache") != "HIT" {
		t.Fatal("alice's second call did not hit the cache — the key is not stable")
	}
	recB, bodyB := f.get(t, "/api/explore", "bob")
	if recB.Header().Get("X-Cache") == "HIT" {
		t.Fatal("bob was served alice's cached explore feed — the cache key omits the user identity")
	}
	if fmt.Sprint(bodyA["following"]) == fmt.Sprint(bodyB["following"]) {
		t.Fatalf("alice and bob resolved to the same follow set (%v) — the follow graph leaked",
			bodyA["following"])
	}
}

// TestExploreMakesNoModelCall — invariant #4, asserted from the recorded hosts rather than from the
// absence of a log line (§9.44).
func TestExploreMakesNoModelCall(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, []map[string]any{
		fixtureEvent("evt_a", "product_launch", "2026-08-19T10:00:00Z", 0.6, 0.6, "official", 2,
			[]map[string]any{rawTicker("AMD", 1.0, true)}),
	})
	f := newFeedsFixture(t, ev.URL(), journal.URL)
	f.get(t, "/api/explore", "u1")

	llmHost := mustHost(t, f.srv.cfg.LLMURL)
	for _, host := range f.transport.attempted() {
		if host == llmHost {
			t.Fatalf("explore reached %s — invariant #4 forbids a model call on a page load", host)
		}
	}
}

// TestExploreCarriesTheFollowAffordance — doc §16.7's "+ Follow <ticker>", as DATA: a ticker and a
// flag, not a link. The frontend owns navigation.
func TestExploreCarriesTheFollowAffordance(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, []map[string]any{
		fixtureEvent("evt_a", "earnings_result", "2026-08-19T10:00:00Z", 0.9, 0.9, "official", 5,
			[]map[string]any{rawTicker("TSM", 1.0, true), rawTicker("NVDA", 0.6, false)}),
	})
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/explore", "u1")
	raw, _ := json.Marshal(body["sections"])
	var sections exploreSections
	if err := json.Unmarshal(raw, &sections); err != nil {
		t.Fatalf("decode sections: %v", err)
	}
	got := allItems(sections)
	if len(got) == 0 {
		t.Fatal("no items ranked")
	}
	for _, it := range got {
		if it.Follow.Ticker != "TSM" || it.Follow.Followed {
			t.Fatalf("follow affordance = %+v, want TSM not-yet-followed", it.Follow)
		}
		if it.WhyYouAreSeeingThis != "You follow NVDA." {
			t.Fatalf("whyYouAreSeeingThis = %q, want doc §16.7's own phrasing", it.WhyYouAreSeeingThis)
		}
	}
}

// TestExploreGuestGetsTheUniverse: a signed-out visitor still gets a ranked page, labelled.
func TestExploreGuestGetsTheUniverse(t *testing.T) {
	journal := journalFake(t, nil)
	ev := newEventsFake(t, []map[string]any{
		fixtureEvent("evt_a", "product_launch", "2026-08-19T10:00:00Z", 0.6, 0.6, "official", 2,
			[]map[string]any{rawTicker("AMD", 1.0, true)}),
	})
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	rec, body := f.get(t, "/api/explore", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if d := degradedOf(t, body); !hasString(d, degradedSubscriptionsGuest) {
		t.Fatalf("degraded = %v, want %q", d, degradedSubscriptionsGuest)
	}
}

// TestJoinAndReadsLikeProse: the reason strings are read by people.
func TestJoinAndReadsLikeProse(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"NVDA"}, "NVDA"},
		{[]string{"NVDA", "AMD"}, "NVDA and AMD"},
		{[]string{"NVDA", "AMD", "TSM"}, "NVDA, AMD and TSM"},
	}
	for _, c := range cases {
		if got := joinAnd(c.in); got != c.want {
			t.Errorf("joinAnd(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ─────────────────────────────────────────────── Wave 4 integration: AD-8 on the server's own prose

// TestExploreReasonStringsCarryNoFloat: `importance`, `novelty`, `relevance` and `potentialStrength`
// are floats in the contract and WORDS on screen (AD-8, doc §16.6, locked decision 3).
//
// That rule binds the GATEWAY, not just the browser. `whyYouAreSeeingThis` used to read "High market
// importance (0.83), officially confirmed." — a two-digit score with no calibration behind it, in a
// string the client is required to render verbatim. Fixing it in the renderer would have made the
// frontend the author of a reason the benchmark expects the server to own, and would have hidden the
// defect instead of closing it (§9.58). It is fixed here, and asserted here.
func TestExploreReasonStringsCarryNoFloat(t *testing.T) {
	// THE FIXTURE TICKERS MATTER AND THE FIRST VERSION OF THIS TEST GOT THEM WRONG.
	//
	// `whyYouAreSeeingThis` returns at the FIRST branch that answers, and the float lived in branch
	// 4 (market importance). The first draft used TSM/AVGO/MU against a user following NVDA — every
	// one of those is `semiconductors` in `sectorOf`, so branch 3 answered first and branch 4 never
	// ran. The test passed against the unfixed code. That is the same vacuous-gate failure this wave
	// found twice elsewhere, so the tickers below are deliberately absent from `sectorOf` and the
	// assertion at the bottom proves branch 4 was actually reached.
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, []map[string]any{
		fixtureEvent("evt_a", "earnings_result", "2026-08-19T10:00:00Z", 0.83, 0.9, "official", 5,
			[]map[string]any{rawTicker("BA", 1.0, true)}),
		fixtureEvent("evt_b", "regulatory_action", "2026-08-19T11:00:00Z", 0.80, 0.5, "official", 4,
			[]map[string]any{rawTicker("CAT", 1.0, true)}),
		fixtureEvent("evt_c", "product_launch", "2026-08-19T12:00:00Z", 0.72, 0.4, "professional", 2,
			[]map[string]any{rawTicker("DE", 1.0, true)}),
	})
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/explore", "u1")
	raw, _ := json.Marshal(body["sections"])
	var sections exploreSections
	if err := json.Unmarshal(raw, &sections); err != nil {
		t.Fatalf("decode sections: %v", err)
	}
	items := allItems(sections)
	if len(items) == 0 {
		t.Fatal("no items ranked — the assertion below would be vacuous")
	}

	// A decimal point between two digits. `0.83` matches; "S&P 500" and "8-K" do not.
	decimal := regexp.MustCompile(`\d\.\d`)
	sawImportanceBranch := false
	for _, it := range items {
		if strings.HasPrefix(it.WhyYouAreSeeingThis, "High market importance") {
			sawImportanceBranch = true
		}
		for label, s := range map[string]string{
			"whyYouAreSeeingThis": it.WhyYouAreSeeingThis,
			"possibleReadThrough": it.PossibleReadThrough,
		} {
			if decimal.MatchString(s) {
				t.Fatalf("%s on %s renders a contract float: %q — AD-8 requires a word", label, it.ID, s)
			}
		}
	}

	// Without this the test is a tautology: it would pass on any corpus whose reasons all come from
	// a branch that never had a float in it.
	if !sawImportanceBranch {
		t.Fatalf("no item reached the market-importance branch — this test cannot have exercised the "+
			"string that carried the float. Reasons seen: %v", reasonsOf(items))
	}
}

func reasonsOf(items []exploreItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.WhyYouAreSeeingThis)
	}
	return out
}

// TestExploreServesTheImportanceBand: the client must not keep a hand-synced copy of
// `importanceHighMin` / `importanceMediumMin` across a language boundary, where nothing fails if one
// side moves and the other does not. The band is computed once, here, and served.
func TestExploreServesTheImportanceBand(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, []map[string]any{
		fixtureEvent("evt_hi", "earnings_result", "2026-08-19T10:00:00Z", 0.83, 0.9, "official", 5,
			[]map[string]any{rawTicker("NVDA", 1.0, true)}),
		fixtureEvent("evt_mid", "product_launch", "2026-08-19T11:00:00Z", 0.55, 0.5, "official", 3,
			[]map[string]any{rawTicker("NVDA", 1.0, true)}),
		fixtureEvent("evt_lo", "partnership", "2026-08-19T12:00:00Z", 0.20, 0.3, "professional", 1,
			[]map[string]any{rawTicker("NVDA", 1.0, true)}),
	})
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/explore", "u1")
	raw, _ := json.Marshal(body["sections"])
	var sections exploreSections
	if err := json.Unmarshal(raw, &sections); err != nil {
		t.Fatalf("decode sections: %v", err)
	}

	want := map[string]string{"evt_hi": "high", "evt_mid": "medium", "evt_lo": "low"}
	seen := map[string]bool{}
	for _, it := range allItems(sections) {
		if w, ok := want[it.ID]; ok {
			if it.ImportanceBand != w {
				t.Fatalf("%s importanceBand = %q, want %q for importance %.2f",
					it.ID, it.ImportanceBand, w, it.Importance)
			}
			// The band must agree with the one function that defines it, always.
			if it.ImportanceBand != importanceBand(it.Importance) {
				t.Fatalf("%s: served band %q disagrees with importanceBand()", it.ID, it.ImportanceBand)
			}
			seen[it.ID] = true
		}
	}
	if len(seen) == 0 {
		t.Fatal("none of the three fixtures were ranked — the assertion is vacuous")
	}
}
