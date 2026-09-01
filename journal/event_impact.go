package main

// event_impact.go — Phase 3: turning a stored Calendar event into a REVIEW ITEM, deterministically.
//
// WHAT THIS FILE PRODUCES, AND WHAT IT REFUSES TO
// -----------------------------------------------
// Two read-only surfaces over data that already exists:
//
//   GET /theses/event-reviews          "which of my theses does this week's calendar touch, and
//                                       which condition I wrote down does it touch?"
//   GET /portfolios/{id}/event-impact  "which of my holdings does it touch, how much weight is
//                                       exposed, and what did I already say about those companies?"
//
// Both are REVIEW TOOLS. Neither produces an instruction, an allocation, a position size, an order,
// or a bearing the evidence does not support. Specifically:
//
//   * **No thesis is ever modified.** A review record is a pointer to work the user might do; it is
//     not a state change on their research. Nothing here writes to the thesis store.
//   * **`bearing` is never `supports` or `contradicts` from deterministic matching.** A term
//     overlap between "CoWoS packaging capacity" and a TSMC earnings date establishes that the two
//     are ABOUT the same thing. It establishes nothing whatever about direction. Claiming
//     "contradicts" from a keyword hit would be the exact fabrication this system is built to
//     avoid, so the deterministic path emits `context` (this touches something you named) or
//     `unclear` (this touches your company but nothing you named). `supports`/`contradicts` are
//     reserved for a bounded, background, evidence-citing model hypothesis — see §"Bearing".
//   * **Every weight is computed here, in code, from the user's own holdings.** No model sees a
//     number, and none is asked to produce one.
//
// OWNERSHIP AND ISOLATION
// -----------------------
// Theses and portfolios are per-user records this service already owns and scopes. Events are
// global and carry no user identity at all. So the join happens HERE, after `userID(r)` has
// resolved, and every store read is already uid-scoped — one user cannot reach another's thesis or
// portfolio through these routes, for the same reason they cannot through the routes they are
// built on.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// eventImpactVersion stamps every record. Bumped when the MATCHING RULES change — a stored or
// cached record must always say which rules produced it.
const eventImpactVersion = "event-impact@1"

// The default forward window. A review queue is about what is coming; a two-week horizon is long
// enough to prepare and short enough that the queue stays short.
const (
	eventImpactDefaultAheadDays  = 14
	eventImpactDefaultBehindDays = 3
	eventImpactMaxAheadDays      = 180
	eventImpactRequestTimeout    = 15 * time.Second
	eventImpactMaxEvents         = 400
)

// Bearing values. Two are reachable deterministically; two are reserved for an evidence-citing
// model hypothesis and are never produced by the code in this file.
const (
	bearingContext     = "context"
	bearingUnclear     = "unclear"
	bearingSupports    = "supports"    // reserved: model hypothesis only
	bearingContradicts = "contradicts" // reserved: model hypothesis only
	matchSourceTerms   = "term-overlap"
	matchSourceKind    = "event-kind"
	matchSourceDirect  = "direct-subject"
)

// storedRelationship is the events service's `GET /relationships` row.
type storedRelationship struct {
	EventID       string `json:"eventId"`
	Ticker        string `json:"ticker"`
	Relationship  string `json:"relationship"`
	Reason        string `json:"reason"`
	Source        string `json:"source"`
	SourceRef     string `json:"sourceRef"`
	RelevanceBand string `json:"relevanceBand"`
	CalcVersion   string `json:"calcVersion"`
	Event         struct {
		ID          string `json:"id"`
		Kind        string `json:"kind"`
		Ticker      string `json:"ticker"`
		Series      string `json:"series"`
		ScheduledAt string `json:"scheduledAt"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Status      string `json:"status"`
		Confirmed   bool   `json:"confirmed"`
		Importance  string `json:"importance"`
		Source      string `json:"source"`
		SourceTier  string `json:"sourceTier"`
		SourceURL   string `json:"sourceUrl"`
	} `json:"event"`
}

// EventRef is the event as it appears on a review record — enough to render and deep-link, and no
// more. Every field is copied verbatim from the store.
type EventRef struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Ticker      string `json:"ticker,omitempty"`
	Series      string `json:"series,omitempty"`
	ScheduledAt string `json:"scheduledAt"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
	Confirmed   bool   `json:"confirmed"`
	Importance  string `json:"importance"`
	Source      string `json:"source"`
	SourceTier  string `json:"sourceTier"`
	SourceURL   string `json:"sourceUrl,omitempty"`
	// DeepLink is the app route that opens this event in Calendar.
	DeepLink string `json:"deepLink"`
}

// MatchedCondition names the thing the user themselves wrote that this event touches.
type MatchedCondition struct {
	ItemID   string   `json:"itemId"`
	Field    string   `json:"field"` // catalysts | risks | invalidationConditions
	Text     string   `json:"text"`
	Terms    []string `json:"terms"`
	MatchVia string   `json:"matchVia"` // term-overlap | event-kind
}

// ThesisEventReview is one "review required" record.
type ThesisEventReview struct {
	ThesisID     string            `json:"thesisId"`
	Ticker       string            `json:"ticker"`
	EventID      string            `json:"eventId"`
	Event        EventRef          `json:"event"`
	Relationship string            `json:"relationship"`
	Reason       string            `json:"reason"`
	Matched      *MatchedCondition `json:"matchedCondition"`
	Bearing      string            `json:"bearing"`
	BearingWhy   string            `json:"bearingReason"`
	Evidence     []EvidenceRef     `json:"evidence"`
	AsOf         string            `json:"asOf"`
	Version      string            `json:"version"`
	ThesisLink   string            `json:"thesisDeepLink"`
	ResearchLink string            `json:"researchDeepLink"`
}

// EvidenceRef points at the stored thing a record was derived from. Ids and URLs only — this is a
// citation, not a copy.
type EvidenceRef struct {
	Kind string `json:"kind"` // scheduled_event | relationship | thesis_item
	Ref  string `json:"ref"`
	URL  string `json:"url,omitempty"`
	Note string `json:"note,omitempty"`
}

// ── term matching ────────────────────────────────────────────────────────────────────────────────
//
// Deliberately dull. A shared SIGNIFICANT term between what the user wrote and what the event says
// is evidence that the two are about the same subject — nothing stronger, and the record says
// nothing stronger. Stopwords and short tokens are dropped so "the" and "of" cannot match anything,
// and a match needs a term of at least `minTermLen` runes so "AI" and "US" do not fire either.

const minTermLen = 4

var stopTerms = map[string]bool{
	"about": true, "after": true, "again": true, "against": true, "because": true, "been": true,
	"before": true, "being": true, "below": true, "between": true, "both": true, "could": true,
	"does": true, "doing": true, "down": true, "during": true, "each": true, "from": true,
	"further": true, "have": true, "having": true, "here": true, "into": true, "more": true,
	"most": true, "only": true, "other": true, "over": true, "same": true, "should": true,
	"some": true, "such": true, "than": true, "that": true, "their": true, "them": true,
	"then": true, "there": true, "these": true, "they": true, "this": true, "those": true,
	"through": true, "under": true, "until": true, "very": true, "were": true, "what": true,
	"when": true, "where": true, "which": true, "while": true, "with": true, "will": true,
	"would": true, "your": true, "company": true, "quarter": true, "quarterly": true,
	"results": true, "report": true, "reported": true, "date": true, "event": true,
	"scheduled": true, "conference": true, "call": true, "release": true, "announce": true,
	"announced": true, "update": true, "updates": true, "expected": true, "remains": true,
}

func normalizeTerms(text string) map[string]bool {
	out := map[string]bool{}
	var token []rune
	flush := func() {
		if len(token) >= minTermLen {
			word := strings.ToLower(string(token))
			if !stopTerms[word] {
				out[word] = true
			}
		}
		token = token[:0]
	}
	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			token = append(token, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

func sharedTerms(a, b map[string]bool) []string {
	var out []string
	for term := range a {
		if b[term] {
			out = append(out, term)
		}
	}
	sort.Strings(out)
	return out
}

// eventKindMatchesItem catches the case term overlap structurally cannot: a catalyst that says
// "next earnings report" against an event whose KIND is `earnings`. The overlap is stopworded away
// (both "earnings" survives, actually) — but a catalyst phrased "Q3 print" would not overlap at all
// while still obviously being about the earnings event. So kind is checked too, against a closed
// table rather than a similarity score.
var kindPhrases = map[string][]string{
	"earnings":      {"earnings", "eps", "results", "guidance", "print", "quarter"},
	"macro_release": {"inflation", "cpi", "ppi", "payroll", "payrolls", "gdp", "pce", "macro", "rates"},
	"central_bank":  {"fomc", "federal reserve", "fed ", "rate decision", "interest rate", "policy"},
	"company_event": {"investor day", "annual meeting", "shareholder", "product launch"},
}

func kindMatches(kind, itemText string) bool {
	lowered := strings.ToLower(itemText)
	for _, phrase := range kindPhrases[kind] {
		if strings.Contains(lowered, phrase) {
			return true
		}
	}
	return false
}

// matchThesisItem finds the FIRST condition this event touches, searching the lists in the order
// they matter for a review: an invalidation condition first (the user said this would break the
// thesis), then a risk, then a catalyst. One match per (thesis, event) — a review queue that lists
// the same event four times because four sentences share a word is not a queue.
func matchThesisItem(th Thesis, eventText string, kind string) *MatchedCondition {
	eventTerms := normalizeTerms(eventText)

	lists := []struct {
		field string
		items []ThesisItem
	}{
		{"invalidationConditions", th.InvalidationConditions},
		{"risks", th.Risks},
		{"catalysts", th.Catalysts},
	}

	for _, list := range lists {
		for _, item := range list.items {
			if shared := sharedTerms(normalizeTerms(item.Text), eventTerms); len(shared) > 0 {
				return &MatchedCondition{
					ItemID: item.ID, Field: list.field, Text: item.Text,
					Terms: shared, MatchVia: matchSourceTerms,
				}
			}
		}
	}
	for _, list := range lists {
		for _, item := range list.items {
			if kindMatches(kind, item.Text) {
				return &MatchedCondition{
					ItemID: item.ID, Field: list.field, Text: item.Text,
					Terms: []string{}, MatchVia: matchSourceKind,
				}
			}
		}
	}
	return nil
}

// ── the events client ────────────────────────────────────────────────────────────────────────────

// fetchRelationships reads the events service's store-only relationship view.
//
// It is a READ of a store, so it cannot cause a provider call or a model call; that is the property
// "no model call occurs when merely opening Following, Calendar or Portfolio" rests on, and it is
// asserted from the other side too (`services/events` refuses to fetch on a read).
func (s *Server) fetchRelationships(
	ctx context.Context, tickers []string, from, to string,
) ([]storedRelationship, error) {
	if s.cfg.EventsURL == "" {
		return nil, fmt.Errorf("events service not configured")
	}
	if len(tickers) == 0 {
		return nil, nil
	}
	query := url.Values{}
	query.Set("tickers", strings.Join(tickers, ","))
	query.Set("from", from)
	query.Set("to", to)
	query.Set("limit", fmt.Sprint(eventImpactMaxEvents))

	ctx, cancel := context.WithTimeout(ctx, eventImpactRequestTimeout)
	defer cancel()

	var payload struct {
		Relationships []storedRelationship `json:"relationships"`
	}
	u := s.cfg.EventsURL + "/relationships?" + query.Encode()
	if err := s.getJSON(ctx, u, &payload); err != nil {
		return nil, err
	}
	return payload.Relationships, nil
}

func toEventRef(rel storedRelationship) EventRef {
	e := rel.Event
	return EventRef{
		ID: e.ID, Kind: e.Kind, Ticker: e.Ticker, Series: e.Series,
		ScheduledAt: e.ScheduledAt, Title: e.Title, Description: e.Description,
		Status: e.Status, Confirmed: e.Confirmed, Importance: e.Importance,
		Source: e.Source, SourceTier: e.SourceTier, SourceURL: e.SourceURL,
		DeepLink: "#calendar?event=" + url.QueryEscape(e.ID),
	}
}

// eventWindow resolves the requested window, bounded so one request cannot ask for a decade.
func eventWindow(r *http.Request, now time.Time) (string, string) {
	ahead := eventImpactDefaultAheadDays
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		var parsed int
		if _, err := fmt.Sscanf(raw, "%d", &parsed); err == nil && parsed > 0 {
			ahead = parsed
		}
	}
	if ahead > eventImpactMaxAheadDays {
		ahead = eventImpactMaxAheadDays
	}
	from := now.Add(-time.Duration(eventImpactDefaultBehindDays) * 24 * time.Hour)
	to := now.Add(time.Duration(ahead) * 24 * time.Hour)
	return from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)
}

func uniqueTickers(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		symbol := strings.ToUpper(strings.TrimSpace(v))
		if symbol == "" || seen[symbol] {
			continue
		}
		seen[symbol] = true
		out = append(out, symbol)
	}
	sort.Strings(out)
	return out
}

// ── thesis review queue ──────────────────────────────────────────────────────────────────────────

func (s *Server) handleThesisEventReviews(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	if uid == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"reviews": []ThesisEventReview{}, "degraded": []string{"auth:guest"},
			"version": eventImpactVersion,
		})
		return
	}
	theses := s.theses.List(uid)

	now := time.Now().UTC()
	from, to := eventWindow(r, now)
	asOf := now.Format(time.RFC3339)

	active := make([]Thesis, 0, len(theses))
	byTicker := map[string][]Thesis{}
	for _, th := range theses {
		if th.Status != "active" {
			continue
		}
		symbol := strings.ToUpper(strings.TrimSpace(th.Ticker))
		if symbol == "" {
			continue
		}
		active = append(active, th)
		byTicker[symbol] = append(byTicker[symbol], th)
	}

	degraded := []string{}
	reviews := []ThesisEventReview{}

	if len(byTicker) > 0 {
		tickers := make([]string, 0, len(byTicker))
		for symbol := range byTicker {
			tickers = append(tickers, symbol)
		}
		relationships, err := s.fetchRelationships(r.Context(), uniqueTickers(tickers), from, to)
		if err != nil {
			degraded = append(degraded, "events:unavailable")
		} else {
			reviews = buildThesisReviews(byTicker, relationships, asOf)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"reviews":  reviews,
		"from":     from,
		"to":       to,
		"asOf":     asOf,
		"version":  eventImpactVersion,
		"theses":   len(active),
		"degraded": degraded,
	})
}

// strongestPerTickerEvent collapses the relationship rows for one (ticker, event) pair down to the
// strongest one, so a downstream consumer sees each pair once. It preserves input order among the
// survivors, which keeps the review queue's ordering deterministic.
func strongestPerTickerEvent(relationships []storedRelationship) []storedRelationship {
	best := map[string]int{}
	out := []storedRelationship{}
	for _, rel := range relationships {
		key := strings.ToUpper(rel.Ticker) + "|" + rel.Event.ID
		if index, seen := best[key]; seen {
			if bandRank(rel.RelevanceBand) > bandRank(out[index].RelevanceBand) {
				out[index] = rel
			}
			continue
		}
		best[key] = len(out)
		out = append(out, rel)
	}
	return out
}

// buildThesisReviews is the whole deterministic rule set, extracted so it can be tested without a
// server, a store or a network.
func buildThesisReviews(
	byTicker map[string][]Thesis, relationships []storedRelationship, asOf string,
) []ThesisEventReview {
	// ONE REVIEW PER (THESIS, EVENT). The same event reaches one ticker through several
	// relationship types — AMD is both a competitor and a sector peer of NVIDIA — and the store is
	// right to record both. A review QUEUE is not: listing the same event twice against the same
	// thesis is how a queue stops being read. The strongest relationship describes the item.
	seen := map[string]int{}
	out := []ThesisEventReview{}
	for _, rel := range strongestPerTickerEvent(relationships) {
		for _, th := range byTicker[strings.ToUpper(rel.Ticker)] {
			eventText := strings.Join([]string{
				rel.Event.Title, rel.Event.Description, rel.Event.Series, rel.Event.Ticker,
			}, " ")
			matched := matchThesisItem(th, eventText, rel.Event.Kind)

			bearing, why := bearingFor(matched, rel)
			if bearing == "" {
				continue
			}

			evidence := []EvidenceRef{
				{Kind: "scheduled_event", Ref: rel.Event.ID, URL: rel.Event.SourceURL,
					Note: rel.Event.Source + " (" + rel.Event.SourceTier + ")"},
				{Kind: "relationship", Ref: rel.Ticker + ":" + rel.Relationship,
					Note: rel.Source + " — " + rel.SourceRef},
			}
			if matched != nil {
				evidence = append(evidence, EvidenceRef{
					Kind: "thesis_item", Ref: matched.ItemID, Note: matched.Field,
				})
			}

			key := th.ID + "|" + rel.Event.ID
			if index, dup := seen[key]; dup {
				// Reached again through a weaker relationship. `strongestPerTickerEvent` already
				// ordered them, so the first one stands.
				_ = index
				continue
			}
			seen[key] = len(out)
			out = append(out, ThesisEventReview{
				ThesisID: th.ID, Ticker: th.Ticker, EventID: rel.Event.ID,
				Event: toEventRef(rel), Relationship: rel.Relationship, Reason: rel.Reason,
				Matched: matched, Bearing: bearing, BearingWhy: why,
				Evidence: evidence, AsOf: asOf, Version: eventImpactVersion,
				ThesisLink:   "#research/thesis?ticker=" + url.QueryEscape(th.Ticker),
				ResearchLink: "#research/overview?ticker=" + url.QueryEscape(th.Ticker),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Event.ScheduledAt != out[j].Event.ScheduledAt {
			return out[i].Event.ScheduledAt < out[j].Event.ScheduledAt
		}
		if out[i].ThesisID != out[j].ThesisID {
			return out[i].ThesisID < out[j].ThesisID
		}
		return out[i].EventID < out[j].EventID
	})
	return out
}

// bearingFor is where the honesty lives.
//
// Returns "" when the event does not warrant a review record at all. It NEVER returns `supports` or
// `contradicts`: term overlap and a relationship type establish that an event is ABOUT something
// the user named, and nothing at all about which way it cuts. Those two values exist in the
// vocabulary for a bounded background model hypothesis that cites its evidence — not for this
// function, which has no evidence that could support them.
func bearingFor(matched *MatchedCondition, rel storedRelationship) (string, string) {
	if matched != nil {
		return bearingContext, fmt.Sprintf(
			"This event is about the same subject as a %s you wrote on this thesis. "+
				"Deterministic matching establishes the SUBJECT overlap only — it does not "+
				"establish whether the event supports or challenges the thesis.",
			strings.TrimSuffix(matched.Field, "s"),
		)
	}
	// Nothing the user named, but the event is on the thesis company itself and the store rates it
	// high importance. Worth surfacing; explicitly not interpreted.
	if rel.Relationship == "direct" && strings.EqualFold(rel.Event.Importance, "high") {
		return bearingUnclear, "A high-importance event on this thesis's company that does not " +
			"match any condition you have written down."
	}
	return "", ""
}

// ── portfolio event impact ───────────────────────────────────────────────────────────────────────

// AffectedHolding is one position an event bears on, with the weight it carries.
type AffectedHolding struct {
	Ticker           string            `json:"ticker"`
	Weight           *float64          `json:"weight,omitempty"`
	MarketValue      *float64          `json:"marketValue,omitempty"`
	Sector           string            `json:"sector,omitempty"`
	Relationship     string            `json:"relationship"`
	RelationshipWhy  string            `json:"relationshipReason"`
	RelevanceBand    string            `json:"relevanceBand"`
	ValuationSource  string            `json:"valuationSource"`
	SourceSynthetic  bool              `json:"sourceIsSynthetic"`
	ThesisID         string            `json:"thesisId,omitempty"`
	MatchedCondition *MatchedCondition `json:"matchedCondition,omitempty"`
	ResearchLink     string            `json:"researchDeepLink"`
}

// PortfolioEventImpact is one event's effect on one portfolio.
type PortfolioEventImpact struct {
	EventID string   `json:"eventId"`
	Event   EventRef `json:"event"`

	AffectedHoldings []AffectedHolding `json:"affectedHoldings"`
	// ExposedWeight counts each HOLDING once, however many relationship types connect it to the
	// event. A position that is both `sector` and `competitor` is still one position.
	ExposedWeight   *float64               `json:"exposedWeight,omitempty"`
	WeightsComplete bool                   `json:"weightsComplete"`
	Relationships   []string               `json:"relationshipTypes"`
	SectorExposure  []PortfolioExposure    `json:"sectorExposure"`
	Concentration   PortfolioConcentration `json:"concentrationContext"`

	AsOf     string   `json:"asOf"`
	Version  string   `json:"version"`
	Degraded []string `json:"degraded"`
}

func (s *Server) handlePortfolioEventImpact(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	portfolio, ok, err := s.portfolios.Get(uid, r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio store is unreadable"})
		return
	}
	if !ok {
		// Ownership isolation: a portfolio belonging to somebody else is NOT FOUND, not FORBIDDEN.
		// The same answer for "does not exist" and "is not yours" is what keeps the store from
		// confirming another user's records exist.
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "portfolio not found"})
		return
	}

	intelligence, err := s.buildPortfolioIntelligence(r.Context(), uid, portfolio)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio intelligence unavailable"})
		return
	}

	theses := s.theses.List(uid)
	byTicker := map[string][]Thesis{}
	for _, th := range theses {
		if th.Status != "active" {
			continue
		}
		symbol := strings.ToUpper(strings.TrimSpace(th.Ticker))
		if symbol != "" {
			byTicker[symbol] = append(byTicker[symbol], th)
		}
	}

	now := time.Now().UTC()
	from, to := eventWindow(r, now)
	asOf := now.Format(time.RFC3339)

	held := make([]string, 0, len(intelligence.Positions))
	for _, position := range intelligence.Positions {
		held = append(held, position.Ticker)
	}

	degraded := append([]string{}, intelligence.Degraded...)
	impacts := []PortfolioEventImpact{}
	relationships, err := s.fetchRelationships(r.Context(), uniqueTickers(held), from, to)
	if err != nil {
		degraded = append(degraded, "events:unavailable")
	} else {
		impacts = buildPortfolioEventImpacts(intelligence, byTicker, relationships, asOf)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"portfolioId":        portfolio.ID,
		"portfolioVersion":   portfolio.Version,
		"calculationVersion": intelligence.CalculationVersion,
		"version":            eventImpactVersion,
		"asOf":               asOf,
		"from":               from,
		"to":                 to,
		"impacts":            impacts,
		"degraded":           degraded,
	})
}

// buildPortfolioEventImpacts groups relationships by event and computes the code-owned context.
// Extracted so the arithmetic is testable without a server, a store or a network.
func buildPortfolioEventImpacts(
	intelligence PortfolioIntelligence,
	thesesByTicker map[string][]Thesis,
	relationships []storedRelationship,
	asOf string,
) []PortfolioEventImpact {
	positions := map[string]PortfolioPositionIntelligence{}
	for _, position := range intelligence.Positions {
		positions[strings.ToUpper(position.Ticker)] = position
	}

	order := []string{}
	grouped := map[string][]storedRelationship{}
	for _, rel := range relationships {
		symbol := strings.ToUpper(rel.Ticker)
		if _, held := positions[symbol]; !held {
			continue
		}
		if _, seen := grouped[rel.Event.ID]; !seen {
			order = append(order, rel.Event.ID)
		}
		grouped[rel.Event.ID] = append(grouped[rel.Event.ID], rel)
	}

	out := []PortfolioEventImpact{}
	for _, eventID := range order {
		rows := grouped[eventID]

		// THE NO-DOUBLE-COUNTING RULE. One holding contributes its weight ONCE, no matter how many
		// relationship types connect it to this event; the strongest relevance band wins for
		// display. Summing per relationship row would report 12% exposure for a 6% position that
		// happens to be both a competitor and a sector peer.
		strongest := map[string]storedRelationship{}
		types := map[string]bool{}
		for _, rel := range rows {
			symbol := strings.ToUpper(rel.Ticker)
			// Every relationship type is REPORTED (the event reaches this portfolio in several
			// ways and hiding that would misdescribe it); only the strongest one DESCRIBES the
			// holding, and only one contributes weight.
			types[rel.Relationship] = true
			current, seen := strongest[symbol]
			if !seen || bandRank(rel.RelevanceBand) > bandRank(current.RelevanceBand) {
				strongest[symbol] = rel
			}
		}

		symbols := make([]string, 0, len(strongest))
		for symbol := range strongest {
			symbols = append(symbols, symbol)
		}
		sort.Strings(symbols)

		holdings := make([]AffectedHolding, 0, len(symbols))
		exposed := 0.0
		complete := true
		sectorWeights := map[string]float64{}

		for _, symbol := range symbols {
			rel := strongest[symbol]
			position := positions[symbol]

			holding := AffectedHolding{
				Ticker: position.Ticker, Weight: position.Weight,
				MarketValue: position.MarketValue, Sector: position.Sector,
				Relationship: rel.Relationship, RelationshipWhy: rel.Reason,
				RelevanceBand: rel.RelevanceBand, ValuationSource: position.ValuationSource,
				SourceSynthetic: position.SourceSynthetic,
				ResearchLink:    "#research/overview?ticker=" + url.QueryEscape(position.Ticker),
			}
			if theses := thesesByTicker[symbol]; len(theses) > 0 {
				th := theses[0]
				holding.ThesisID = th.ID
				eventText := strings.Join([]string{
					rel.Event.Title, rel.Event.Description, rel.Event.Series, rel.Event.Ticker,
				}, " ")
				holding.MatchedCondition = matchThesisItem(th, eventText, rel.Event.Kind)
			}
			holdings = append(holdings, holding)

			if position.Weight == nil {
				// A holding we could not value still APPEARS — it is affected either way — but the
				// exposure total is marked incomplete rather than silently understated.
				complete = false
				continue
			}
			exposed += *position.Weight
			if position.Sector != "" {
				sectorWeights[position.Sector] += *position.Weight
			}
		}

		relationshipTypes := make([]string, 0, len(types))
		for name := range types {
			relationshipTypes = append(relationshipTypes, name)
		}
		sort.Strings(relationshipTypes)

		sectors := make([]PortfolioExposure, 0, len(sectorWeights))
		for key, weight := range sectorWeights {
			sectors = append(sectors, PortfolioExposure{Kind: "sector", Key: key, Weight: rounded(weight)})
		}
		sort.SliceStable(sectors, func(i, j int) bool {
			if sectors[i].Weight != sectors[j].Weight {
				return sectors[i].Weight > sectors[j].Weight
			}
			return sectors[i].Key < sectors[j].Key
		})

		impact := PortfolioEventImpact{
			EventID: eventID, Event: toEventRef(rows[0]),
			AffectedHoldings: holdings, WeightsComplete: complete,
			Relationships: relationshipTypes, SectorExposure: sectors,
			Concentration: intelligence.Concentration,
			AsOf:          asOf, Version: eventImpactVersion, Degraded: []string{},
		}
		if len(holdings) > 0 {
			impact.ExposedWeight = floatPtr(exposed)
		}
		if !complete {
			impact.Degraded = append(impact.Degraded, "valuation:incomplete")
		}
		out = append(out, impact)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Event.ScheduledAt != out[j].Event.ScheduledAt {
			return out[i].Event.ScheduledAt < out[j].Event.ScheduledAt
		}
		return out[i].EventID < out[j].EventID
	})
	return out
}

func bandRank(band string) int {
	switch band {
	case "primary":
		return 3
	case "secondary":
		return 2
	case "contextual":
		return 1
	}
	return 0
}

// marshalIndentForTests keeps `encoding/json` referenced when the file is compiled without its
// handlers being exercised; it is used by the tests' golden comparisons.
var _ = json.Marshal
