package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// event_impact_test.go — Phase 3's deterministic rules, tested where they live.
//
// The two build* functions are pure, so these tests need no server, no store and no network for
// the parts that matter: the matching rule, the bearing rule and the no-double-counting rule.
// Ownership isolation is tested through the real HTTP surface, because that is where it lives.

func relFor(eventID, ticker, relationship, band, title, description, kind string) storedRelationship {
	var rel storedRelationship
	rel.EventID = eventID
	rel.Ticker = ticker
	rel.Relationship = relationship
	rel.RelevanceBand = band
	rel.Reason = "configured reference relationship"
	rel.Source = "reference"
	rel.SourceRef = "gateway/seed/nvda.json:suppliers"
	rel.CalcVersion = "relationships@1"
	rel.Event.ID = eventID
	rel.Event.Kind = kind
	rel.Event.Ticker = ticker
	rel.Event.ScheduledAt = "2026-11-18T22:00:00Z"
	rel.Event.Title = title
	rel.Event.Description = description
	rel.Event.Status = "confirmed"
	rel.Event.Confirmed = true
	rel.Event.Importance = "high"
	rel.Event.Source = "company-ir"
	rel.Event.SourceTier = "official"
	rel.Event.SourceURL = "https://investor.nvidia.com/events/"
	return rel
}

func thesisWith(id, ticker string, items map[string][]string) Thesis {
	th := Thesis{ID: id, Ticker: ticker, Status: "active", Claim: "a claim"}
	build := func(prefix string, texts []string) []ThesisItem {
		out := make([]ThesisItem, 0, len(texts))
		for i, text := range texts {
			out = append(out, ThesisItem{ID: prefix + "_" + string(rune('a'+i)), Text: text})
		}
		return out
	}
	th.Catalysts = build("cat", items["catalysts"])
	th.Risks = build("rsk", items["risks"])
	th.InvalidationConditions = build("inv", items["invalidationConditions"])
	return th
}

// ── the matching rule ────────────────────────────────────────────────────────────────────────────

func TestATermOverlapProducesAContextReviewNeverADirection(t *testing.T) {
	th := thesisWith("th_1", "NVDA", map[string][]string{
		"risks": {"CoWoS packaging capacity constrains data-centre shipments"},
	})
	rel := relFor("sch_1", "NVDA", "direct", "primary",
		"TSMC CoWoS packaging capacity update", "Advanced packaging capacity commentary", "company_event")

	reviews := buildThesisReviews(
		map[string][]Thesis{"NVDA": {th}}, []storedRelationship{rel}, "2026-08-23T12:00:00Z")

	if len(reviews) != 1 {
		t.Fatalf("want one review, got %d", len(reviews))
	}
	got := reviews[0]
	if got.Bearing != bearingContext {
		t.Fatalf("bearing = %q, want %q", got.Bearing, bearingContext)
	}
	if got.Matched == nil || got.Matched.Field != "risks" {
		t.Fatalf("matched = %+v, want the risk the user wrote", got.Matched)
	}
	if len(got.Matched.Terms) == 0 {
		t.Fatalf("the record must name the terms it matched on")
	}
	if !strings.Contains(got.BearingWhy, "does not establish") {
		t.Fatalf("the record must say what deterministic matching does NOT establish: %q", got.BearingWhy)
	}
}

func TestDeterministicMatchingNeverEmitsSupportsOrContradicts(t *testing.T) {
	// Every shape of input the deterministic path can see, including ones deliberately worded to
	// look like confirmation or refutation. None may produce a direction.
	theses := map[string][]Thesis{"NVDA": {thesisWith("th_1", "NVDA", map[string][]string{
		"catalysts":              {"data-centre revenue growth accelerates"},
		"risks":                  {"packaging capacity constrains shipments"},
		"invalidationConditions": {"data-centre revenue declines for two consecutive quarters"},
	})}}
	inputs := []storedRelationship{
		relFor("sch_1", "NVDA", "direct", "primary",
			"NVIDIA beats on data-centre revenue", "Revenue growth accelerates sharply", "earnings"),
		relFor("sch_2", "NVDA", "direct", "primary",
			"NVIDIA misses; data-centre revenue declines", "Revenue declines", "earnings"),
		relFor("sch_3", "NVDA", "sector", "contextual", "Sector event", "", "company_event"),
		relFor("sch_4", "NVDA", "macro", "contextual", "CPI", "Inflation release", "macro_release"),
	}
	for _, rel := range inputs {
		for _, review := range buildThesisReviews(theses, []storedRelationship{rel}, "t") {
			if review.Bearing == bearingSupports || review.Bearing == bearingContradicts {
				t.Fatalf("deterministic matching produced a DIRECTION (%q) for %q — it has no "+
					"evidence that could support one", review.Bearing, rel.Event.Title)
			}
		}
	}
}

func TestAnInvalidationConditionOutranksARiskAndACatalyst(t *testing.T) {
	th := thesisWith("th_1", "NVDA", map[string][]string{
		"catalysts":              {"packaging capacity expands"},
		"risks":                  {"packaging capacity constrains shipments"},
		"invalidationConditions": {"packaging capacity halves"},
	})
	rel := relFor("sch_1", "NVDA", "direct", "primary", "Packaging capacity update", "", "company_event")

	reviews := buildThesisReviews(map[string][]Thesis{"NVDA": {th}}, []storedRelationship{rel}, "t")
	if len(reviews) != 1 {
		t.Fatalf("one event against one thesis must produce ONE review, got %d", len(reviews))
	}
	if reviews[0].Matched.Field != "invalidationConditions" {
		t.Fatalf("matched %q, want the invalidation condition first", reviews[0].Matched.Field)
	}
}

func TestAHighImportanceDirectEventWithNoMatchIsUnclearNotSilent(t *testing.T) {
	th := thesisWith("th_1", "NVDA", map[string][]string{
		"risks": {"regulatory export restrictions on advanced chips"},
	})
	rel := relFor("sch_1", "NVDA", "direct", "primary",
		"NVIDIA investor day", "A scheduled company event.", "company_event")

	reviews := buildThesisReviews(map[string][]Thesis{"NVDA": {th}}, []storedRelationship{rel}, "t")
	if len(reviews) != 1 || reviews[0].Bearing != bearingUnclear {
		t.Fatalf("want one `unclear` review, got %+v", reviews)
	}
	if reviews[0].Matched != nil {
		t.Fatalf("an unmatched review must not claim a matched condition: %+v", reviews[0].Matched)
	}
}

func TestALowImportanceUnmatchedEventProducesNoReview(t *testing.T) {
	th := thesisWith("th_1", "NVDA", map[string][]string{"risks": {"export restrictions"}})
	rel := relFor("sch_1", "NVDA", "sector", "contextual", "Some peer event", "", "company_event")
	rel.Event.Importance = "low"
	if reviews := buildThesisReviews(
		map[string][]Thesis{"NVDA": {th}}, []storedRelationship{rel}, "t"); len(reviews) != 0 {
		t.Fatalf("a review queue must stay short: %+v", reviews)
	}
}

func TestStopwordsAndShortTokensCannotMatch(t *testing.T) {
	th := thesisWith("th_1", "NVDA", map[string][]string{
		"risks": {"The company will report results in the quarter"},
	})
	// Every significant word here is a stopword or under four characters, so a match would mean
	// the matcher is firing on noise.
	rel := relFor("sch_1", "NVDA", "sector", "contextual",
		"The company will report results for the quarter", "", "company_event")
	for _, review := range buildThesisReviews(
		map[string][]Thesis{"NVDA": {th}}, []storedRelationship{rel}, "t") {
		if review.Matched != nil && review.Matched.MatchVia == matchSourceTerms {
			t.Fatalf("matched on noise: %+v", review.Matched.Terms)
		}
	}
}

func TestReviewsCarryEvidenceAsOfAndVersion(t *testing.T) {
	th := thesisWith("th_1", "NVDA", map[string][]string{"risks": {"packaging capacity"}})
	rel := relFor("sch_1", "NVDA", "direct", "primary", "Packaging capacity update", "", "company_event")

	reviews := buildThesisReviews(map[string][]Thesis{"NVDA": {th}}, []storedRelationship{rel},
		"2026-08-23T12:00:00Z")
	got := reviews[0]
	if got.AsOf != "2026-08-23T12:00:00Z" || got.Version != eventImpactVersion {
		t.Fatalf("asOf/version = %q/%q", got.AsOf, got.Version)
	}
	kinds := map[string]bool{}
	for _, ev := range got.Evidence {
		kinds[ev.Kind] = true
	}
	for _, want := range []string{"scheduled_event", "relationship", "thesis_item"} {
		if !kinds[want] {
			t.Fatalf("evidence is missing %q: %+v", want, got.Evidence)
		}
	}
	if got.ThesisLink == "" || got.ResearchLink == "" || got.Event.DeepLink == "" {
		t.Fatalf("a review must deep-link to the event, the company and the thesis: %+v", got)
	}
}

func TestOneEventCanReviewSeveralThesesAndSeveralTickers(t *testing.T) {
	theses := map[string][]Thesis{
		"NVDA": {
			thesisWith("th_1", "NVDA", map[string][]string{"risks": {"packaging capacity"}}),
			thesisWith("th_2", "NVDA", map[string][]string{"catalysts": {"packaging capacity"}}),
		},
		"AMD": {thesisWith("th_3", "AMD", map[string][]string{"risks": {"packaging capacity"}})},
	}
	relationships := []storedRelationship{
		relFor("sch_1", "NVDA", "direct", "primary", "Packaging capacity update", "", "company_event"),
		relFor("sch_1", "AMD", "sector", "contextual", "Packaging capacity update", "", "company_event"),
	}
	reviews := buildThesisReviews(theses, relationships, "t")
	if len(reviews) != 3 {
		t.Fatalf("one event must be able to reach several theses and tickers, got %d", len(reviews))
	}
}

// ── portfolio exposure arithmetic ────────────────────────────────────────────────────────────────

func intelligenceWith(positions ...PortfolioPositionIntelligence) PortfolioIntelligence {
	return PortfolioIntelligence{
		PortfolioID: "pf_1", CalculationVersion: "portfolio@1",
		Positions: positions, Degraded: []string{},
		Concentration: PortfolioConcentration{LargestTicker: "NVDA", LargestWeight: floatPtr(0.30)},
	}
}

func position(ticker string, weight float64, sector string) PortfolioPositionIntelligence {
	return PortfolioPositionIntelligence{
		Ticker: ticker, Weight: floatPtr(weight), MarketValue: floatPtr(weight * 1000),
		Sector: sector, ValuationSource: "quote",
	}
}

func TestExposedWeightCountsEachHoldingOnce(t *testing.T) {
	intel := intelligenceWith(position("AMD", 0.06, "Technology"))
	// AMD is connected to this event TWICE: as a competitor and as a sector peer.
	relationships := []storedRelationship{
		relFor("sch_1", "AMD", "competitor", "secondary", "NVIDIA earnings", "", "earnings"),
		relFor("sch_1", "AMD", "sector", "contextual", "NVIDIA earnings", "", "earnings"),
	}
	impacts := buildPortfolioEventImpacts(intel, map[string][]Thesis{}, relationships, "t")

	if len(impacts) != 1 {
		t.Fatalf("want one event impact, got %d", len(impacts))
	}
	if len(impacts[0].AffectedHoldings) != 1 {
		t.Fatalf("one holding must appear once: %+v", impacts[0].AffectedHoldings)
	}
	if impacts[0].ExposedWeight == nil || *impacts[0].ExposedWeight != 0.06 {
		t.Fatalf("exposed weight = %v, want 0.06 (not 0.12)", impacts[0].ExposedWeight)
	}
	// The stronger band wins for display, so the holding is described by its most direct link.
	if impacts[0].AffectedHoldings[0].Relationship != "competitor" {
		t.Fatalf("relationship = %q, want the stronger band", impacts[0].AffectedHoldings[0].Relationship)
	}
	if len(impacts[0].Relationships) != 2 {
		t.Fatalf("both relationship types must still be reported: %+v", impacts[0].Relationships)
	}
}

func TestExposedWeightSumsDistinctHoldings(t *testing.T) {
	intel := intelligenceWith(
		position("NVDA", 0.30, "Technology"),
		position("AMD", 0.06, "Technology"),
		position("KO", 0.10, "Consumer Staples"),
	)
	relationships := []storedRelationship{
		relFor("sch_1", "NVDA", "direct", "primary", "NVIDIA earnings", "", "earnings"),
		relFor("sch_1", "AMD", "competitor", "secondary", "NVIDIA earnings", "", "earnings"),
	}
	impacts := buildPortfolioEventImpacts(intel, map[string][]Thesis{}, relationships, "t")
	if *impacts[0].ExposedWeight != 0.36 {
		t.Fatalf("exposed weight = %v, want 0.36", *impacts[0].ExposedWeight)
	}
	// The unaffected holding is not in the impact at all.
	for _, holding := range impacts[0].AffectedHoldings {
		if holding.Ticker == "KO" {
			t.Fatalf("an unrelated holding must not appear: %+v", impacts[0].AffectedHoldings)
		}
	}
	if len(impacts[0].SectorExposure) != 1 || impacts[0].SectorExposure[0].Key != "Technology" {
		t.Fatalf("sector context = %+v", impacts[0].SectorExposure)
	}
}

func TestARelationshipToAnUnheldTickerIsIgnored(t *testing.T) {
	intel := intelligenceWith(position("NVDA", 0.30, "Technology"))
	relationships := []storedRelationship{
		relFor("sch_1", "TSM", "customer", "primary", "NVIDIA earnings", "", "earnings"),
	}
	if impacts := buildPortfolioEventImpacts(
		intel, map[string][]Thesis{}, relationships, "t"); len(impacts) != 0 {
		t.Fatalf("a portfolio that holds none of the affected tickers has no impact: %+v", impacts)
	}
}

func TestAnUnvaluedHoldingStillAppearsAndMarksTheTotalIncomplete(t *testing.T) {
	unvalued := PortfolioPositionIntelligence{Ticker: "PRIV", ValuationSource: "unavailable"}
	intel := intelligenceWith(position("NVDA", 0.30, "Technology"), unvalued)
	relationships := []storedRelationship{
		relFor("sch_1", "NVDA", "direct", "primary", "NVIDIA earnings", "", "earnings"),
		relFor("sch_1", "PRIV", "sector", "contextual", "NVIDIA earnings", "", "earnings"),
	}
	impacts := buildPortfolioEventImpacts(intel, map[string][]Thesis{}, relationships, "t")
	if len(impacts[0].AffectedHoldings) != 2 {
		t.Fatalf("an unvalued holding is still affected: %+v", impacts[0].AffectedHoldings)
	}
	if impacts[0].WeightsComplete {
		t.Fatalf("an unvalued holding must mark the total incomplete, not understate it silently")
	}
	if *impacts[0].ExposedWeight != 0.30 {
		t.Fatalf("exposed weight = %v", *impacts[0].ExposedWeight)
	}
	found := false
	for _, reason := range impacts[0].Degraded {
		if reason == "valuation:incomplete" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the incompleteness must be stated: %+v", impacts[0].Degraded)
	}
}

func TestPortfolioImpactAttachesTheThesisAndTheMatchedCondition(t *testing.T) {
	intel := intelligenceWith(position("NVDA", 0.30, "Technology"))
	theses := map[string][]Thesis{"NVDA": {thesisWith("th_1", "NVDA", map[string][]string{
		"risks": {"packaging capacity constrains shipments"},
	})}}
	relationships := []storedRelationship{
		relFor("sch_1", "NVDA", "direct", "primary", "Packaging capacity update", "", "company_event"),
	}
	impacts := buildPortfolioEventImpacts(intel, theses, relationships, "t")
	holding := impacts[0].AffectedHoldings[0]
	if holding.ThesisID != "th_1" {
		t.Fatalf("thesis not attached: %+v", holding)
	}
	if holding.MatchedCondition == nil || holding.MatchedCondition.Field != "risks" {
		t.Fatalf("matched condition not attached: %+v", holding.MatchedCondition)
	}
	if holding.ResearchLink == "" {
		t.Fatalf("a holding must deep-link to its company research")
	}
}

func TestNoImpactRecordContainsAnInstruction(t *testing.T) {
	intel := intelligenceWith(position("NVDA", 0.30, "Technology"))
	relationships := []storedRelationship{
		relFor("sch_1", "NVDA", "direct", "primary", "NVIDIA earnings", "", "earnings"),
	}
	impacts := buildPortfolioEventImpacts(intel, map[string][]Thesis{}, relationships, "t")
	raw, _ := json.Marshal(impacts)
	rendered := strings.ToLower(string(raw))
	for _, banned := range []string{
		"\"buy\"", "\"sell\"", "\"hold\"", "price target", "position size", "allocate",
		"rebalance to", "order", "shares to",
	} {
		if strings.Contains(rendered, banned) {
			t.Fatalf("an impact record contains an instruction-shaped field (%q): %s", banned, raw)
		}
	}
}

// ── ownership isolation, through the real surface ────────────────────────────────────────────────

func TestEventImpactRoutesAreScopedToTheirOwner(t *testing.T) {
	e := newPortfolioTestEnv(t)
	// The events service is deliberately unconfigured, so the read degrades rather than reaching
	// out. Ownership is what is under test, and it is decided before any upstream call.
	e.srv.cfg.EventsURL = ""
	e.srv.cfg.AnalysisURL = "http://analysis.invalid"
	e.srv.http = &http.Client{Timeout: 500 * time.Millisecond}

	created := decodePortfolio(t, e.request(http.MethodPost, "/portfolios", "alice", validPortfolioBody()))

	mine := e.request(http.MethodGet, "/portfolios/"+created.ID+"/event-impact", "alice", nil)
	if mine.Code != http.StatusOK {
		t.Fatalf("owner status=%d body=%s", mine.Code, mine.Body.String())
	}

	theirs := e.request(http.MethodGet, "/portfolios/"+created.ID+"/event-impact", "mallory", nil)
	if theirs.Code != http.StatusNotFound {
		t.Fatalf("another user status=%d, want 404 (not 403 — existence stays undisclosed)", theirs.Code)
	}

	guest := e.request(http.MethodGet, "/portfolios/"+created.ID+"/event-impact", "", nil)
	if guest.Code != http.StatusUnauthorized {
		t.Fatalf("guest status=%d, want 401", guest.Code)
	}
}

func TestThesisEventReviewsAreScopedAndDegradeHonestly(t *testing.T) {
	e := newPortfolioTestEnv(t)
	e.srv.cfg.EventsURL = ""
	e.srv.http = &http.Client{Timeout: 500 * time.Millisecond}

	guest := e.request(http.MethodGet, "/theses/event-reviews", "", nil)
	if guest.Code != http.StatusOK {
		t.Fatalf("guest status=%d", guest.Code)
	}
	var guestBody struct {
		Reviews  []ThesisEventReview `json:"reviews"`
		Degraded []string            `json:"degraded"`
	}
	json.Unmarshal(guest.Body.Bytes(), &guestBody)
	if len(guestBody.Reviews) != 0 {
		t.Fatalf("a guest has no theses and therefore no reviews")
	}
	if len(guestBody.Degraded) == 0 {
		t.Fatalf("the guest state must be stated, not implied")
	}
}

func TestTheEventImpactReadNeverReachesAModel(t *testing.T) {
	// Every outbound call this surface can make is recorded. The LLM service must not appear.
	var reached []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.URL.Path)
		writeJSON(w, http.StatusOK, map[string]any{"relationships": []any{}})
	}))
	defer upstream.Close()

	e := newPortfolioTestEnv(t)
	e.srv.cfg.EventsURL = upstream.URL
	e.srv.cfg.LLMURL = "http://llm.invalid"
	e.srv.cfg.AnalysisURL = "http://analysis.invalid"
	e.srv.http = &http.Client{Timeout: 500 * time.Millisecond}

	created := decodePortfolio(t, e.request(http.MethodPost, "/portfolios", "alice", validPortfolioBody()))
	e.request(http.MethodGet, "/portfolios/"+created.ID+"/event-impact", "alice", nil)
	e.request(http.MethodGet, "/theses/event-reviews", "alice", nil)

	for _, path := range reached {
		if strings.Contains(path, "portfolio") && strings.Contains(path, "review") {
			t.Fatalf("the impact read reached a model surface: %s", path)
		}
	}
}

func TestOneEventProducesOneReviewPerThesisHoweverManyRelationshipsConnectIt(t *testing.T) {
	th := thesisWith("th_1", "AMD", map[string][]string{"risks": {"packaging capacity"}})
	// The store legitimately records BOTH: AMD is a competitor of NVIDIA and a sector peer.
	relationships := []storedRelationship{
		relFor("sch_1", "AMD", "sector", "contextual", "Packaging capacity update", "", "company_event"),
		relFor("sch_1", "AMD", "competitor", "secondary", "Packaging capacity update", "", "company_event"),
	}
	reviews := buildThesisReviews(map[string][]Thesis{"AMD": {th}}, relationships, "t")
	if len(reviews) != 1 {
		t.Fatalf("a review queue must list an event once per thesis, got %d: %+v", len(reviews), reviews)
	}
	if reviews[0].Relationship != "competitor" {
		t.Fatalf("the strongest relationship must describe the item, got %q", reviews[0].Relationship)
	}
}

func TestRepeatedIngestionCannotDuplicateAReviewRecord(t *testing.T) {
	// The same relationship arriving twice — the shape a duplicate ingestion pass would produce —
	// still yields one review, because the record is keyed by (thesis, event) and computed on read.
	th := thesisWith("th_1", "NVDA", map[string][]string{"risks": {"packaging capacity"}})
	rel := relFor("sch_1", "NVDA", "direct", "primary", "Packaging capacity update", "", "company_event")
	reviews := buildThesisReviews(
		map[string][]Thesis{"NVDA": {th}}, []storedRelationship{rel, rel, rel}, "t")
	if len(reviews) != 1 {
		t.Fatalf("duplicate input must not duplicate the record, got %d", len(reviews))
	}
}
