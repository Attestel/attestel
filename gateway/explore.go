package main

// explore.go — Wave 2 Lane 2C. `GET /api/explore`: discovery outside the watchlist (doc §15.3,
// §16.7), scored by AD-7's deterministic weighted sum.
//
// THE SCORE IS CODE. Not a model call, not a heuristic that drifts between releases — one versioned
// constant block, `exploreWeightsVersion`, and a pure function over it. Three separate reasons make
// that binding, and all three are load-bearing:
//
//   - It must be REPRODUCIBLE for the benchmark. A ranking nobody can recompute cannot be evaluated,
//     and an unevaluated ranking is an opinion with a number attached.
//   - It must be CHEAP enough to run over the whole visible corpus on every request.
//   - Invariant #4 forbids a model call caused by a page load. Explore is a page load.
//
// Every item explains itself or it does not ship. `whyYouAreSeeingThis` and `possibleReadThrough`
// are both mandatory (contract §8, doc §16.7); an item that cannot produce both is DROPPED, never
// shipped with an empty string. An unexplained recommendation is the exact failure mode §16.7 names.
//
// §9.32 and what it actually costs, measured: `possibleReadThrough` is supposed to fall back to a
// string composed from `eventType` and the leading `affectedDimensions` entry when no `transmission`
// exists. But `affectedDimensions` is ITSELF enrichment-gated — `services/events/app/events.py`
// emits it only inside `if enriched:`, and `_link_tickers` writes the column as `'[]'` at ingest —
// so with the shipped default `EVENT_ENRICH_ENABLED=false` BOTH inputs are absent and the literal
// fallback still yields nothing. The fallback here is therefore two-stage: a real dimension when one
// exists, and otherwise a dimension derived deterministically from the §3.4 event type through
// `eventTypeDimension`, which is versioned with the weights. Nothing is invented — the mapping is a
// statement about a closed enum, not about a company — and `readThroughBasis` says on every item
// which stage produced the sentence.

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ═══════════════════════════════════════════════════════════════════════════════════════════════
// THE VERSIONED CONSTANT BLOCK (AD-7)
//
// Everything the score depends on lives here. Changing any value below is a version bump, because
// a benchmark run recorded under "explore@1" must be recomputable from "explore@1" forever.
// ═══════════════════════════════════════════════════════════════════════════════════════════════

const exploreWeightsVersion = "explore@1"

const (
	// w1..w6 sum to 1.0, so the score is itself in [0,1] and two events are comparable across
	// requests without a normalisation pass that would depend on the batch.
	exploreW1MarketImportance   = 0.30 // what the store says the event is worth
	exploreW2Freshness          = 0.20 // discovery is about now
	exploreW3Novelty            = 0.15 // the fifth article about one thing is not a discovery
	exploreW4SourceTier         = 0.10 // who said it
	exploreW5RelationToFollowed = 0.15 // the bridge from the watchlist to outside it
	exploreW6SectorSimilarity   = 0.10 // the weakest term, and the one with the weakest data

	// freshnessHalfLifeHours: an event is worth half as much 18 hours later. Chosen so a morning
	// release still ranks that evening and is clearly stale the next day.
	freshnessHalfLifeHours = 18.0

	// sourceTier weights, matching `entities.py`'s TIER_RANK ordering (discussion < professional <
	// official). A tier the store does not emit scores 0 rather than guessing a middle value.
	exploreTierOfficial     = 1.00
	exploreTierProfessional = 0.60
	exploreTierDiscussion   = 0.25

	// relationToFollowed's two positive levels. 1.0 = the event touches a company you follow;
	// 0.5 = it touches a company that appears ALONGSIDE one you follow on some other event in the
	// window. The 0.5 rung is measured from the corpus, not asserted.
	exploreRelationDirect     = 1.0
	exploreRelationCooccurred = 0.5

	// sectorSimilarity is binary: same sector as something you follow, or nothing. There is no
	// "adjacent sector" rung because there is no data to support one (see sectorOf).
	exploreSectorSame = 1.0

	// Section membership rules. These are thresholds, not scores — a rule table, not a second
	// ranking (see assignSections).
	exploreMarketMovingMin = 0.70 // `importance` at which an event is "market moving"
	exploreTrendingDocMin  = 3    // `documentCount` at which an event is "trending"

	// exploreWindowDays bounds the corpus one request ranks. Explore is discovery, and a
	// three-month-old event is not a discovery.
	exploreWindowDays = 14

	// exploreFetchLimit is MAX_LIMIT on :8004 (services/events/app/events.py:143).
	exploreFetchLimit = 200

	// Per-section page size.
	exploreDefaultLimit = 10
	exploreMaxLimit     = 50

	// exploreMaxSectionsPerItem: one event may appear in at most two sections, so a single big
	// story cannot occupy the whole page.
	exploreMaxSectionsPerItem = 2
)

// analystDisclaimer mirrors §9.18's `ANALYST_DISCLAIMER` into every Explore item that carries a
// `possibleReadThrough`, adjacent to the output it qualifies (docs/DISCLOSURE_CLASSIFICATION.md
// rule 3).
//
// It is a Go LITERAL rather than a value fetched from `services/llm`, for two reasons: the gateway
// must render Explore with no network and no model (invariant #1, invariant #4), and a disclosure
// that can fail to load is not a disclosure. `services/llm/app/analyst.py` does not exist yet — it
// is Lane 3B's file — so the handoff records this constant as a seam the integrator must reconcile
// byte-for-byte once 3B lands.
const analystDisclaimer = "Analytical context, not investment advice and not a recommendation. " +
	"Read-throughs are hypotheses about how an event might reach a company, never a verdict on it."

// sectorOf is the sector term's ENTIRE data source, and its limits are the point of this comment.
//
// There is no sector data anywhere in this repo: doc layer L4 (market context) is MISSING, no
// gateway payload carries a sector field, and `services/events` stores none. So the term is served
// from a small static map, versioned with the weights above.
//
// AN UNKNOWN TICKER SCORES 0, NEVER 0.5. The tempting default — "probably somewhat related" —
// would invent affinity to make a section look full, which is precisely how a discovery feed
// becomes a random feed with a confident explanation attached. A ticker that is not in this map
// simply does not earn this term.
var sectorOf = map[string]string{
	"NVDA": "semiconductors", "AMD": "semiconductors", "INTC": "semiconductors",
	"AVGO": "semiconductors", "TSM": "semiconductors", "MU": "semiconductors",
	"ASML": "semiconductors", "QCOM": "semiconductors", "ARM": "semiconductors",
	"MRVL": "semiconductors", "TXN": "semiconductors", "AMAT": "semiconductors",
	"LRCX": "semiconductors", "KLAC": "semiconductors", "SMCI": "hardware",
	"DELL": "hardware", "HPQ": "hardware", "AAPL": "hardware",
	"MSFT": "software", "ORCL": "software", "CRM": "software", "ADBE": "software",
	"NOW": "software", "PLTR": "software",
	"GOOGL": "internet", "GOOG": "internet", "META": "internet", "AMZN": "internet",
	"NFLX": "internet", "UBER": "internet",
	"TSLA": "autos", "F": "autos", "GM": "autos", "RIVN": "autos",
	"JPM": "financials", "BAC": "financials", "GS": "financials", "MS": "financials",
	"XOM": "energy", "CVX": "energy", "COP": "energy",
	"LLY": "healthcare", "UNH": "healthcare", "JNJ": "healthcare", "PFE": "healthcare",
}

// eventTypeDimension is stage two of §9.32's fallback: the dimension a given §3.4 event type is
// about, when no per-ticker `affectedDimensions` exists to say so.
//
// This is a statement about a CLOSED ENUM, not about a company — "an earnings result is about
// revenue exposure" is true of every earnings result — which is what keeps it out of invented-
// affinity territory. Values are drawn from §3.8's `affected_dimensions` vocabulary so the string
// the user reads uses the same words as an enriched event's would.
var eventTypeDimension = map[string]string{
	"earnings_result":   "revenue_exposure",
	"earnings_guidance": "growth",
	"analyst_revision":  "valuation",
	"product_launch":    "growth",
	"management_change": "management",
	"ma_transaction":    "valuation",
	"regulatory_action": "regulation",
	"legal_action":      "litigation",
	"supply_chain":      "supply",
	"capital_return":    "valuation",
	"financing":         "valuation",
	"partnership":       "demand",
	"macro_release":     "rates",
	"central_bank":      "rates",
	"sector_event":      "demand",
	"other":             "",
}

// eventTypeLabel renders a §3.4 member as prose. A type with no label here yields "", which drops
// the item rather than printing a snake_case identifier at a user.
var eventTypeLabel = map[string]string{
	"earnings_result": "Earnings result", "earnings_guidance": "Guidance update",
	"analyst_revision": "Analyst revision", "product_launch": "Product launch",
	"management_change": "Management change", "ma_transaction": "M&A transaction",
	"regulatory_action": "Regulatory action", "legal_action": "Legal action",
	"supply_chain": "Supply-chain development", "capital_return": "Capital return",
	"financing": "Financing", "partnership": "Partnership",
	"macro_release": "Macro release", "central_bank": "Central-bank decision",
	"sector_event": "Sector development", "other": "",
}

// dimensionLabel renders §3.8's `affected_dimensions` vocabulary as prose.
var dimensionLabel = map[string]string{
	"revenue_exposure": "revenue exposure", "margin": "margins", "growth": "growth",
	"regulation": "regulation", "management": "management", "litigation": "litigation",
	"valuation": "valuation", "demand": "demand", "supply": "supply",
	"rates": "rates", "currency": "currency exposure",
}

// exploreSectionOrder is the priority used when an event qualifies for more than
// `exploreMaxSectionsPerItem` sections. It is a fixed order, declared once, because "pick by
// highest score" cannot break the tie on its own: the score is a property of the EVENT, identical
// in every section it qualifies for. Within a section, items are ordered by score.
//
// `forYou` leads because it is capped at `limit` — only the highest-scoring handful reach it — so
// putting it first costs the specific sections almost nothing while keeping the flagship section
// genuinely the top of the ranking.
var exploreSectionOrder = []string{
	exploreSectionForYou,
	exploreSectionRelated,
	exploreSectionEarnings,
	exploreSectionMacro,
	exploreSectionMarketMoving,
	exploreSectionTrending,
}

const (
	exploreSectionForYou       = "forYou"
	exploreSectionMarketMoving = "marketMoving"
	exploreSectionRelated      = "relatedToYourCompanies"
	exploreSectionEarnings     = "earnings"
	exploreSectionMacro        = "macro"
	exploreSectionTrending     = "trending"
)

// ═══════════════════════════════════════════════════════════════════════ end of the constant block

// exploreTerms is the score's six inputs, published on every item. A ranked feed that will not show
// its work cannot be argued with, and AD-7's whole premise is that this one can be.
type exploreTerms struct {
	MarketImportance   float64 `json:"marketImportance"`
	Freshness          float64 `json:"freshness"`
	Novelty            float64 `json:"novelty"`
	SourceTier         float64 `json:"sourceTier"`
	RelationToFollowed float64 `json:"relationToFollowed"`
	SectorSimilarity   float64 `json:"sectorSimilarity"`
}

type exploreFollow struct {
	Ticker   string `json:"ticker"`
	Followed bool   `json:"followed"`
}

// exploreItem is one card. Note what is NOT here: no direction word, no target, no verdict. A feed
// that ranks events is not a feed that tells anyone what to do (invariant #2).
type exploreItem struct {
	ID                string       `json:"id"`
	EventType         string       `json:"eventType"`
	Title             string       `json:"title"`
	Summary           string       `json:"summary,omitempty"`
	PublishedAt       int64        `json:"publishedAt"`
	PublishedAtISO    string       `json:"publishedAtISO"`
	PrimaryTicker     string       `json:"primaryTicker,omitempty"`
	Tickers           []feedTicker `json:"tickers"`
	SourceTier        string       `json:"sourceTier,omitempty"`
	OfficialConfirmed bool         `json:"officialConfirmed"`
	Importance        float64      `json:"importance"`
	// ImportanceBand is the SERVED band — "high" | "medium" | "low" — from `importanceBand` in
	// feeds.go, i.e. the same two named thresholds the Following feed's counts use. Serving it is
	// what lets the client stop re-deriving the bands from a hand-copied pair of constants.
	ImportanceBand      string        `json:"importanceBand"`
	Novelty             float64       `json:"novelty"`
	DocumentCount       int           `json:"documentCount"`
	Score               float64       `json:"score"`
	ScoreTerms          exploreTerms  `json:"scoreTerms"`
	WeightsVersion      string        `json:"weightsVersion"`
	Section             string        `json:"section"`
	SectionRule         string        `json:"sectionRule"`
	WhyYouAreSeeingThis string        `json:"whyYouAreSeeingThis"`
	PossibleReadThrough string        `json:"possibleReadThrough"`
	ReadThroughBasis    string        `json:"readThroughBasis"`
	Disclaimer          string        `json:"disclaimer"`
	Follow              exploreFollow `json:"follow"`
	Macro               *macroDetail  `json:"macro,omitempty"`
	Sources             []eventSource `json:"sources,omitempty"`
	DataState           string        `json:"dataState"`
}

// exploreSections is a STRUCT rather than a map so all six keys of contract §8 are always present,
// `[]` when empty. A section that vanishes from the payload when it has nothing in it is a section
// the frontend has to special-case.
type exploreSections struct {
	ForYou                 []exploreItem `json:"forYou"`
	MarketMoving           []exploreItem `json:"marketMoving"`
	RelatedToYourCompanies []exploreItem `json:"relatedToYourCompanies"`
	Earnings               []exploreItem `json:"earnings"`
	Macro                  []exploreItem `json:"macro"`
	Trending               []exploreItem `json:"trending"`
}

func emptySections() exploreSections {
	return exploreSections{
		ForYou: []exploreItem{}, MarketMoving: []exploreItem{},
		RelatedToYourCompanies: []exploreItem{}, Earnings: []exploreItem{},
		Macro: []exploreItem{}, Trending: []exploreItem{},
	}
}

func (s *exploreSections) append(section string, item exploreItem) {
	switch section {
	case exploreSectionForYou:
		s.ForYou = append(s.ForYou, item)
	case exploreSectionMarketMoving:
		s.MarketMoving = append(s.MarketMoving, item)
	case exploreSectionRelated:
		s.RelatedToYourCompanies = append(s.RelatedToYourCompanies, item)
	case exploreSectionEarnings:
		s.Earnings = append(s.Earnings, item)
	case exploreSectionMacro:
		s.Macro = append(s.Macro, item)
	case exploreSectionTrending:
		s.Trending = append(s.Trending, item)
	}
}

type exploreResponse struct {
	Sections       exploreSections `json:"sections"`
	Following      []string        `json:"following"`
	WeightsVersion string          `json:"weightsVersion"`
	AsOf           string          `json:"asOf"`
	Degraded       []string        `json:"degraded"`
}

// ─────────────────────────────────────────────────────────────────────────── GET /api/explore

func (s *Server) handleExplore(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), feedsTimeout)
	defer cancel()

	id := s.identityFrom(r)
	uid := id.userID
	if uid == "" {
		uid = "guest"
	}

	section := strings.TrimSpace(r.URL.Query().Get("section"))
	limit := exploreDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = min(n, exploreMaxLimit)
		}
	}

	// Per-user cache key, invariant #5 — the same rule as `/api/following` and for the same reason:
	// `ttlCache` is one process-wide map and Explore's ranking is a function of the caller's follow
	// graph, so a key without the uid leaks it.
	key := "explore:" + uid + ":" + feedQueryHash(section+"|"+strconv.Itoa(limit))
	if cached, ok := s.cache.get(key); ok {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cached)
		return
	}

	resp := s.buildExplore(ctx, id, section, limit)
	if body, err := json.Marshal(resp); err == nil {
		cacheState := "BYPASS"
		if feedResponseCacheable(resp.Degraded) {
			s.cache.set(key, body, s.cfg.NewsTTL)
			cacheState = "MISS"
		}
		w.Header().Set("X-Cache", cacheState)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildExplore ranks the visible corpus and lays it out in six sections.
//
// When :8004 is unreachable this returns SIX EMPTY SECTIONS with the degraded marker — deliberately
// NOT a ranking over `newsFor` headlines. Four of the six terms (importance, novelty, sourceTier,
// documentCount) do not exist for a headline, so a fallback ranking would be three-quarters
// invented while looking identical to the real one. Following degrades to headlines because a
// headline IS a piece of news; Explore does not, because a headline is not a scored event.
func (s *Server) buildExplore(ctx context.Context, id identity, section string, limit int) exploreResponse {
	now := time.Now().UTC()
	follow := s.resolveFollowSet(ctx, id)
	degraded := append([]string(nil), follow.degraded...)

	base := exploreResponse{
		Sections:       emptySections(),
		Following:      follow.tickers,
		WeightsVersion: exploreWeightsVersion,
		AsOf:           now.Format(time.RFC3339),
	}

	since := now.AddDate(0, 0, -exploreWindowDays)
	v := url.Values{}
	v.Set("since", since.Format(time.RFC3339))
	v.Set("limit", strconv.Itoa(exploreFetchLimit))

	env, err := s.listEvents(ctx, v)
	if err != nil {
		marker := degradedEventsUnreachable
		if err == errEventsUnconfigured {
			marker = degradedEventsUnconfigured
		}
		base.Degraded = dedupeStrings(append(degraded, marker))
		return base
	}
	macro := s.listMacro(ctx, v.Get("since"), exploreFetchLimit)

	base.Sections = s.rankExplore(env.Events, follow, macro, now, section, limit)
	base.Degraded = dedupeStrings(degraded)
	return base
}

// rankExplore is the pure half: events in, sections out, no I/O. Extracted so the score can be
// tested as the pure function AD-7 requires it to be — same fixture, same ordering, every run.
func (s *Server) rankExplore(events []canonicalEvent, follow followSet, macro map[string]macroDetail,
	now time.Time, section string, limit int) exploreSections {

	cooccurring := cooccurrenceSet(events, follow)
	followedSectors := map[string]bool{}
	for _, t := range follow.tickers {
		if sec := sectorOf[strings.ToUpper(t)]; sec != "" {
			followedSectors[sec] = true
		}
	}

	type scored struct {
		ev    canonicalEvent
		terms exploreTerms
		score float64
	}
	ranked := make([]scored, 0, len(events))
	for _, ev := range events {
		if ev.ID == "" {
			continue
		}
		terms := exploreScoreTerms(ev, follow, cooccurring, followedSectors, now)
		ranked = append(ranked, scored{ev: ev, terms: terms, score: exploreScore(terms)})
	}

	// Sort by score DESC, then (publishedAt DESC, id DESC) — a TOTAL order, so two runs over one
	// fixture produce byte-identical ordering even when scores tie exactly, which they routinely do
	// (two raw events from one ingest run share every term but freshness).
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		ai, aj := feedUnix(ranked[i].ev.PublishedAt), feedUnix(ranked[j].ev.PublishedAt)
		if ai != aj {
			return ai > aj
		}
		return ranked[i].ev.ID > ranked[j].ev.ID
	})

	// forYou is the top of the OVERALL score, excluding events whose primary company the user
	// already follows — Explore is discovery outside the watchlist (doc §15.3), so an event about a
	// company already on the watchlist belongs in Following, not here.
	forYou := map[string]bool{}
	for _, r := range ranked {
		if len(forYou) >= limit {
			break
		}
		if primary := r.ev.primary(); primary != "" && follow.has(primary) {
			continue
		}
		forYou[r.ev.ID] = true
	}

	out := emptySections()
	for _, r := range ranked {
		placements := assignSections(r.ev, r.terms, follow, forYou[r.ev.ID])
		if len(placements) == 0 {
			continue
		}
		for _, p := range placements {
			if section != "" && p.section != section {
				continue
			}
			if sectionLen(&out, p.section) >= limit {
				continue
			}
			item, ok := buildExploreItem(r.ev, r.terms, r.score, follow, macro, p)
			if !ok {
				// Locked decision #4: an item that cannot produce BOTH explanation strings is
				// dropped, not shipped with an empty one.
				continue
			}
			out.append(p.section, item)
		}
	}
	return out
}

// ───────────────────────────────────────────────────────────────────────────────── the score

// exploreScore is AD-7's weighted sum. Pure, total, and in [0,1] because the weights sum to 1 and
// every term is clamped.
func exploreScore(t exploreTerms) float64 {
	return exploreW1MarketImportance*t.MarketImportance +
		exploreW2Freshness*t.Freshness +
		exploreW3Novelty*t.Novelty +
		exploreW4SourceTier*t.SourceTier +
		exploreW5RelationToFollowed*t.RelationToFollowed +
		exploreW6SectorSimilarity*t.SectorSimilarity
}

// exploreScoreTerms computes the six terms. `importance` and `novelty` are CONSUMED from :8004, not
// recomputed: Lane 2A owns them, they are versioned there under `SCORE_VERSION`, and a second
// implementation in Go would be a second thing to keep in sync and a second thing to be wrong.
func exploreScoreTerms(ev canonicalEvent, follow followSet, cooccurring map[string]bool,
	followedSectors map[string]bool, now time.Time) exploreTerms {

	terms := exploreTerms{
		MarketImportance: clamp01(ev.Importance),
		Freshness:        freshnessOf(feedUnix(ev.PublishedAt), now),
		Novelty:          clamp01(ev.Novelty),
		SourceTier:       sourceTierWeight(ev.SourceTier),
	}

	for _, t := range ev.Tickers {
		sym := strings.ToUpper(t.Ticker)
		if follow.has(sym) {
			terms.RelationToFollowed = exploreRelationDirect
			break
		}
		if cooccurring[sym] && terms.RelationToFollowed < exploreRelationCooccurred {
			terms.RelationToFollowed = exploreRelationCooccurred
		}
	}

	for _, t := range ev.Tickers {
		if sec := sectorOf[strings.ToUpper(t.Ticker)]; sec != "" && followedSectors[sec] {
			terms.SectorSimilarity = exploreSectorSame
			break
		}
	}
	return terms
}

// freshnessOf is exponential decay with a named half-life. Deterministic given `now`, which the
// caller supplies rather than reading the clock here — that is what makes the whole ranking a pure
// function of its inputs and therefore testable.
func freshnessOf(publishedAt int64, now time.Time) float64 {
	if publishedAt <= 0 {
		return 0
	}
	ageHours := now.Sub(time.Unix(publishedAt, 0)).Hours()
	if ageHours <= 0 {
		return 1 // published at or after `now` (clock skew between services) — treat as brand new
	}
	return clamp01(math.Exp(-math.Ln2 * ageHours / freshnessHalfLifeHours))
}

func sourceTierWeight(tier string) float64 {
	switch tier {
	case "official":
		return exploreTierOfficial
	case "professional":
		return exploreTierProfessional
	case "discussion":
		return exploreTierDiscussion
	default:
		return 0 // a tier the store does not emit earns nothing
	}
}

// cooccurrenceSet is the evidence behind the 0.5 relation rung: every company that appears on some
// event in the window ALONGSIDE a company the user follows. Measured from the corpus in front of us,
// not asserted from a static table — which is exactly the difference between this term and
// `sectorSimilarity`, and why this one can be trusted at 0.5.
func cooccurrenceSet(events []canonicalEvent, follow followSet) map[string]bool {
	out := map[string]bool{}
	for _, ev := range events {
		touchesFollowed := false
		for _, t := range ev.Tickers {
			if follow.has(t.Ticker) {
				touchesFollowed = true
				break
			}
		}
		if !touchesFollowed {
			continue
		}
		for _, t := range ev.Tickers {
			sym := strings.ToUpper(t.Ticker)
			if !follow.has(sym) {
				out[sym] = true
			}
		}
	}
	return out
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ────────────────────────────────────────────────────────────────────── section assignment

// placement is one section an event landed in, plus the rule that put it there. The rule travels
// into the payload (`sectionRule`) so "why is this card in this section" is answerable without
// reading this file.
type placement struct {
	section string
	rule    string
}

// assignSections is a RULE TABLE, not a second score. Membership is a set of stated predicates over
// the event; the score decides ORDER WITHIN a section, never membership in one.
func assignSections(ev canonicalEvent, terms exploreTerms, follow followSet, inForYou bool) []placement {
	var all []placement

	primary := ev.primary()
	primaryFollowed := primary != "" && follow.has(primary)

	if inForYou {
		all = append(all, placement{exploreSectionForYou,
			"top of the overall explore score, primary company not already followed"})
	}
	// relatedToYourCompanies is discovery OUTSIDE the watchlist (doc §15.3): the event must reach a
	// followed company somehow, while not being about one.
	if terms.RelationToFollowed > 0 && !primaryFollowed {
		all = append(all, placement{exploreSectionRelated,
			"reaches a company you follow, but is not about one"})
	}
	switch ev.EventType {
	case "earnings_result", "earnings_guidance":
		all = append(all, placement{exploreSectionEarnings, "event type is " + ev.EventType})
	case "macro_release", "central_bank":
		all = append(all, placement{exploreSectionMacro, "event type is " + ev.EventType})
	}
	if ev.Importance >= exploreMarketMovingMin {
		all = append(all, placement{exploreSectionMarketMoving,
			"importance " + strconv.FormatFloat(ev.Importance, 'f', 2, 64) + " >= " +
				strconv.FormatFloat(exploreMarketMovingMin, 'f', 2, 64)})
	}
	if ev.DocumentCount >= exploreTrendingDocMin {
		all = append(all, placement{exploreSectionTrending,
			strconv.Itoa(ev.DocumentCount) + " sources reporting this"})
	}

	// At most two, taken in the declared priority order so the choice is deterministic rather than
	// dependent on the order the predicates happen to be written in above.
	picked := make([]placement, 0, exploreMaxSectionsPerItem)
	for _, want := range exploreSectionOrder {
		for _, p := range all {
			if p.section == want {
				picked = append(picked, p)
				break
			}
		}
		if len(picked) == exploreMaxSectionsPerItem {
			break
		}
	}
	return picked
}

func sectionLen(s *exploreSections, section string) int {
	switch section {
	case exploreSectionForYou:
		return len(s.ForYou)
	case exploreSectionMarketMoving:
		return len(s.MarketMoving)
	case exploreSectionRelated:
		return len(s.RelatedToYourCompanies)
	case exploreSectionEarnings:
		return len(s.Earnings)
	case exploreSectionMacro:
		return len(s.Macro)
	case exploreSectionTrending:
		return len(s.Trending)
	}
	return 0
}

// ─────────────────────────────────────────────────────────── the two mandatory explanations

// buildExploreItem assembles one card, or reports that it cannot. `ok=false` means at least one of
// the two mandatory strings came back empty, and the caller drops the item.
func buildExploreItem(ev canonicalEvent, terms exploreTerms, score float64, follow followSet,
	macro map[string]macroDetail, p placement) (exploreItem, bool) {

	why := whyYouAreSeeingThis(ev, terms, follow)
	if why == "" {
		return exploreItem{}, false
	}
	readThrough, basis := possibleReadThrough(ev, follow)
	if readThrough == "" {
		return exploreItem{}, false
	}

	tickers := make([]feedTicker, 0, len(ev.Tickers))
	for _, t := range ev.Tickers {
		relevance := t.Relevance
		tickers = append(tickers, feedTicker{
			Ticker: t.Ticker, Followed: follow.has(t.Ticker),
			Relevance: &relevance, IsPrimary: t.IsPrimary,
			PotentialDirection: t.PotentialDirection, PotentialStrength: t.PotentialStrength,
			ExpectedHorizon: t.ExpectedHorizon, AffectedDimensions: t.AffectedDimensions,
			Transmission: t.Transmission,
		})
	}

	primary := ev.primary()
	item := exploreItem{
		ID: ev.ID, EventType: ev.EventType, Title: ev.Title, Summary: ev.Summary,
		PublishedAt: feedUnix(ev.PublishedAt), PublishedAtISO: ev.PublishedAt,
		PrimaryTicker: primary, Tickers: tickers,
		SourceTier: ev.SourceTier, OfficialConfirmed: ev.OfficialConfirmed,
		Importance: ev.Importance, Novelty: ev.Novelty, DocumentCount: ev.DocumentCount,
		ImportanceBand: importanceBand(ev.Importance),
		Score:          score, ScoreTerms: terms, WeightsVersion: exploreWeightsVersion,
		Section: p.section, SectionRule: p.rule,
		WhyYouAreSeeingThis: why,
		PossibleReadThrough: readThrough,
		ReadThroughBasis:    basis,
		// §9.18: the disclosure travels WITH the read-through it qualifies. Any item carrying one
		// carries the other; there is no path through this function that sets one without the other.
		Disclaimer: analystDisclaimer,
		// The `+ Follow {ticker}` affordance of doc §16.7, as data: a ticker and a flag, not a
		// link. The frontend owns navigation.
		Follow:    exploreFollow{Ticker: primary, Followed: primary != "" && follow.has(primary)},
		Sources:   ev.Sources,
		DataState: dataStateLive,
	}
	if m, ok := macro[ev.ID]; ok {
		item.Macro = &m
	}
	return item, true
}

// whyYouAreSeeingThis composes the reason from THE ACTUAL TERM THAT SCORED, in descending order of
// how directly it answers the question. It is not a template with the event dropped into it — an
// item ranked by novelty and one ranked by a follow relationship are here for genuinely different
// reasons, and saying the same thing about both would make the string decorative.
//
// It returns "" when the event carries no scored term and no renderable type — the drop path.
func whyYouAreSeeingThis(ev canonicalEvent, terms exploreTerms, follow followSet) string {
	// 1. Direct relationship: the strongest and most honest answer, and doc §16.7's own example.
	if terms.RelationToFollowed >= exploreRelationDirect {
		var followed []string
		for _, t := range ev.Tickers {
			if follow.has(t.Ticker) {
				followed = append(followed, strings.ToUpper(t.Ticker))
			}
		}
		sort.Strings(followed)
		if len(followed) > 0 {
			return "You follow " + joinAnd(followed) + "."
		}
	}
	// 2. Measured co-occurrence: named, with the followed company it was measured against.
	if terms.RelationToFollowed >= exploreRelationCooccurred {
		var subjects []string
		for _, t := range ev.Tickers {
			if !follow.has(t.Ticker) {
				subjects = append(subjects, strings.ToUpper(t.Ticker))
			}
		}
		sort.Strings(subjects)
		if len(subjects) > 0 {
			return joinAnd(subjects) + " is regularly reported alongside companies you follow."
		}
	}
	// 3. Sector, from the static map — and the string says "sector", so the claim is exactly as
	//    strong as the data behind it.
	if terms.SectorSimilarity > 0 {
		for _, t := range ev.Tickers {
			if sec := sectorOf[strings.ToUpper(t.Ticker)]; sec != "" {
				return "Same sector (" + sec + ") as companies you follow."
			}
		}
	}
	// 4. Market importance, as the BAND WORD, with the confirmation status that earned it.
	//
	//    This sentence used to quote the float — "High market importance (0.83), officially
	//    confirmed." — and four of them reached one Explore screen. `importance` is a float in the
	//    contract and a WORD on screen (AD-8, doc §16.6, locked decision 3): the score has no
	//    calibration behind it, so two digits claim a precision that does not exist. The fix belongs
	//    HERE and not in the browser: the gateway owns this reason string under
	//    `exploreWeightsVersion` (AD-7), and a client that rewrote the sentence would become the
	//    author of a reason the benchmark expects the server to own — and would hide the defect
	//    rather than close it (§9.58).
	//
	//    `exploreMarketMovingMin` == `importanceHighMin`, so `importanceBand` returns "high" on every
	//    path through this branch and the word below is the band, not a synonym for it.
	if ev.Importance >= exploreMarketMovingMin {
		if ev.OfficialConfirmed {
			return "High market importance, officially confirmed."
		}
		return "High market importance."
	}
	// 5. Breadth of coverage.
	if ev.DocumentCount >= exploreTrendingDocMin {
		return strconv.Itoa(ev.DocumentCount) + " sources are reporting this."
	}
	// 6. Last resort: the event type itself, which is a §3.4 member and therefore renderable. If it
	//    is not — unknown or "other" — there is genuinely nothing to say and the item is dropped.
	if label := eventTypeLabel[ev.EventType]; label != "" {
		return label + " in the last " + strconv.Itoa(exploreWindowDays) + " days."
	}
	return ""
}

// possibleReadThrough composes "how might this reach a company", in three stages, and reports which
// stage produced it (`readThroughBasis`) so a reader can tell an enriched sentence from a derived one.
//
//	transmission → the model's own sentence about THIS event and THIS company, when enrichment ran.
//	dimensions   → §9.32's literal fallback: event type + the leading `affectedDimensions` entry.
//	eventType    → §9.32's fallback made to actually work. Measured: `affectedDimensions` is itself
//	               enrichment-gated in `services/events/app/events.py`, and `_link_tickers` writes
//	               '[]' at ingest, so with the shipped default `EVENT_ENRICH_ENABLED=false` the
//	               literal fallback has no input either and every item would be dropped — the exact
//	               outcome §9.32 exists to prevent. This stage derives the dimension from the closed
//	               §3.4 enum instead, which is a claim about the event TYPE, not about a company.
//
// Returns "" only when the event type is unknown or unrenderable, which is the drop path.
func possibleReadThrough(ev canonicalEvent, follow followSet) (string, string) {
	// Stage 1 — prefer the transmission written for a company the user actually follows, then the
	// subject company's, then any. A read-through about a company the reader cares about is the
	// point of the field.
	var fallbackTransmission string
	for _, t := range ev.Tickers {
		if t.Transmission == "" {
			continue
		}
		if follow.has(t.Ticker) {
			return t.Transmission, "transmission"
		}
		if fallbackTransmission == "" || t.IsPrimary {
			fallbackTransmission = t.Transmission
		}
	}
	if fallbackTransmission != "" {
		return fallbackTransmission, "transmission"
	}

	label := eventTypeLabel[ev.EventType]
	if label == "" {
		return "", ""
	}

	// Stage 2 — the leading affectedDimensions entry, preferring a followed company's.
	if dim := leadingDimension(ev, follow); dim != "" {
		if human := dimensionLabel[dim]; human != "" {
			return label + " — relevant to " + human + ".", "dimensions"
		}
	}

	// Stage 3 — the dimension the event TYPE is about.
	if human := dimensionLabel[eventTypeDimension[ev.EventType]]; human != "" {
		return label + " — relevant to " + human + ".", "eventType"
	}
	return "", ""
}

func leadingDimension(ev canonicalEvent, follow followSet) string {
	for _, t := range ev.Tickers {
		if follow.has(t.Ticker) && len(t.AffectedDimensions) > 0 {
			return t.AffectedDimensions[0]
		}
	}
	for _, t := range ev.Tickers {
		if len(t.AffectedDimensions) > 0 {
			return t.AffectedDimensions[0]
		}
	}
	return ""
}

// joinAnd renders a list the way a person would read it: "NVDA", "NVDA and AMD", "NVDA, AMD and TSM".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}
