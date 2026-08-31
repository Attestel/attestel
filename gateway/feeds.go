package main

// feeds.go — Wave 2 Lane 2C. The Following feed, the `/api/events/{id}` read-through, and the
// degraded cascade both of them and `explore.go` share.
//
// Contract: EVENT_CONTRACTS.md §8 (the routes), §3.12 (the canonical event JSON), §2.1 (journal's
// subscription surface), §9.2 (the read-through), §9.33 (the offline floor).
//
// FOUR THINGS THIS FILE DELIBERATELY DOES NOT DO.
//
//  1. It declares no events client. `gateway` is one flat `package main` and `s.eventsGet`
//     (gateway/events.go:48, a METHOD on *Server) is the single declaration site. A second one is a
//     compile error, not a merge conflict, which is the wrong way to discover that two waves both
//     needed one.
//  2. It registers `GET /api/changed` nowhere. That route is Lane 5B's (§9.1) and returns
//     `changes.go`'s existing `ChangeItem` vocabulary; two `mux.HandleFunc` calls on one pattern
//     panic at startup.
//  3. It makes no model call. Not for ranking, not for reasons, not for summaries — invariant #4
//     forbids a model call caused by a page load, and a feed is the definition of a page load.
//     Neither this file nor `explore.go` names the llm service's base URL at all.
//  4. It persists nothing. `since` and `cursor` are query parameters with documented defaults; a
//     server-side "last seen" timestamp is journal's `user_event_state`, not the gateway's.
//
// THE TIME BOUNDARY IS HERE AND ONLY HERE. `services/events` speaks ISO 8601 UTC strings; the
// gateway speaks unix-second int64 on every `…At` field (`changes.go`'s `ChangeItem.At` is the
// house precedent). `feedUnix` is the one conversion, and every item keeps the original string
// alongside it (`publishedAtISO`) so the UI can render a source timestamp without re-formatting a
// number back into a date. Doing this in five places is how a feed ends up an hour off in one
// section.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// init attaches this lane's routes through the Wave 0 seam (`routeseams.go`), exactly as
// `subscriptions.go` does for Wave 1. Neither `main.go` nor `routeseams.go` is edited here.
func init() {
	registerEventRoute(func(s *Server, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/following", s.handleFollowing)
		mux.HandleFunc("GET /api/explore", s.handleExplore)
		mux.HandleFunc("GET /api/events/{id}", s.handleEventRead)
	})
}

// ─────────────────────────────────────────────────────────────────────────────────── constants

const (
	// feedsTimeout bounds one feed request across all its upstream hops. Well under the Server's
	// 130s client timeout: a feed is a page load, and a page load that hangs is worse than a page
	// load that degrades.
	feedsTimeout = 20 * time.Second

	// followingDefaultLimit / followingMaxLimit bound the page the caller gets back.
	followingDefaultLimit = 50
	followingMaxLimit     = 200

	// followingFetchLimit is what we ask :8004 for per query. It is MAX_LIMIT on that service
	// (services/events/app/events.py:143), so asking for more is silently clamped there anyway.
	// The gateway paginates the MERGED, subscription-filtered set, which is not the same set
	// :8004 paginates, so its cursor cannot be passed through and ours is computed here.
	followingFetchLimit = 200

	// feedNewsCap mirrors `newsFor`'s own cap (enrich.go:78). The degraded path does not widen it.
	feedNewsCap = 20

	// followingWindowDays bounds how far back a feed with no explicit `since` reaches. Without it
	// the first page of a year-old corpus is a year old.
	followingWindowDays = 30
)

// degraded markers. Every one of these is a POSITIVE, machine-checkable signal in the response
// body — §9.44: log absence is never evidence, so the cascade reports where it landed rather than
// leaving a caller to infer it from a missing log line.
const (
	degradedEventsUnreachable        = "events:unreachable"
	degradedEventsUnconfigured       = "events:unconfigured"
	degradedSubscriptionsGuest       = "subscriptions:guest"
	degradedSubscriptionsUnreachable = "subscriptions:unreachable"
	degradedSubscriptionsEmpty       = "subscriptions:empty"
	degradedNewsUnavailable          = "news:unavailable"
)

// feedResponseCacheable refuses to preserve a local dependency outage. In the all-in-one image,
// nginx and the gateway can accept the first browser request a fraction before the events process
// finishes its PostgreSQL migration. Caching that response for NewsTTL turns a sub-second startup
// race into a 30-minute apparent outage. Provider/news failures remain cacheable because their
// separate rate limits are exactly why NewsTTL exists.
func feedResponseCacheable(degraded []string) bool {
	for _, marker := range degraded {
		if marker == degradedEventsUnreachable {
			return false
		}
	}
	return true
}

// feedOrigin* names where an item actually came from, so provenance survives into the payload and
// the frontend can badge it. This is `source`'s role elsewhere in the gateway; the name differs
// because a feed item already has a `sources[]` array of documents.
const (
	feedOriginEvents = "events"
	feedOriginNews   = "news"
	feedOriginSeed   = "seed"
)

// ──────────────────────────────────────────────────────── the §3.12 canonical event, decoded

// canonicalEvent is contract §3.12 verbatim, with the two enrichment-gated fields as pointers so
// "absent" survives decoding as absent rather than collapsing to a zero value. With
// `EVENT_ENRICH_ENABLED=false` — the shipped default — `whyItMatters`, `keyFacts` and `audit` are
// omitted by `services/events`, and a feed that rendered `""` for them would be lying.
type canonicalEvent struct {
	ID                string        `json:"id"`
	EventType         string        `json:"eventType"`
	Title             string        `json:"title"`
	Summary           string        `json:"summary"`
	WhyItMatters      *string       `json:"whyItMatters"`
	KeyFacts          []string      `json:"keyFacts"`
	OccurredAt        string        `json:"occurredAt"`
	PublishedAt       string        `json:"publishedAt"`
	FirstSeenAt       string        `json:"firstSeenAt"`
	SourceTier        string        `json:"sourceTier"`
	OfficialConfirmed bool          `json:"officialConfirmed"`
	Importance        float64       `json:"importance"`
	Novelty           float64       `json:"novelty"`
	DocumentCount     int           `json:"documentCount"`
	EnrichmentState   string        `json:"enrichmentState"`
	Tickers           []eventTicker `json:"tickers"`
	Sources           []eventSource `json:"sources"`
}

// eventTicker is §3.8 as it appears in §3.12's JSON. Everything below `isPrimary` is enrichment
// output and therefore optional; `relevance` and `isPrimary` are code's and always present (§9.15).
type eventTicker struct {
	Ticker             string   `json:"ticker"`
	Relevance          float64  `json:"relevance"`
	IsPrimary          bool     `json:"isPrimary"`
	PotentialDirection string   `json:"potentialDirection,omitempty"`
	PotentialStrength  *float64 `json:"potentialStrength,omitempty"`
	ExpectedHorizon    string   `json:"expectedHorizon,omitempty"`
	AffectedDimensions []string `json:"affectedDimensions,omitempty"`
	Transmission       string   `json:"transmission,omitempty"`
}

// eventSource is one supporting document's attribution, read from `event_documents` (§3.7).
type eventSource struct {
	DocumentID  string `json:"documentId"`
	Provider    string `json:"provider"`
	SourceTier  string `json:"sourceTier"`
	URL         string `json:"url"`
	PublishedAt string `json:"publishedAt"`
}

// macroDetail is §3.9's row, attached to the macro event it hangs off so a Fed/CPI card can show
// actual/expected/surprise without a second round trip from the browser.
type macroDetail struct {
	ID                    string   `json:"id"`
	EventID               string   `json:"eventId"`
	Series                string   `json:"series"`
	ReleaseAt             int64    `json:"releaseAt"`
	ReleaseAtISO          string   `json:"releaseAtISO"`
	Actual                *float64 `json:"actual"`
	Expected              *float64 `json:"expected"`
	Previous              *float64 `json:"previous"`
	Surprise              *float64 `json:"surprise"`
	Unit                  string   `json:"unit"`
	ExpectationSource     string   `json:"expectationSource,omitempty"`
	ExpectationCapturedAt string   `json:"expectationCapturedAt,omitempty"`
}

// primary returns the subject company of an event, "" when it has none (a market-wide macro
// release links no company until per-ticker macro enrichment runs — doc §15.8).
func (e canonicalEvent) primary() string {
	for _, t := range e.Tickers {
		if t.IsPrimary {
			return t.Ticker
		}
	}
	return ""
}

// tickerEntry returns the per-ticker row for one symbol.
func (e canonicalEvent) tickerEntry(sym string) (eventTicker, bool) {
	for _, t := range e.Tickers {
		if strings.EqualFold(t.Ticker, sym) {
			return t, true
		}
	}
	return eventTicker{}, false
}

// feedUnix is THE ISO→unix boundary. Everything `…At` in a gateway payload is an int64 and passes
// through here exactly once. Unparseable ⇒ 0, which sorts last and is visibly wrong rather than
// silently plausible.
func feedUnix(iso string) int64 {
	if iso == "" {
		return 0
	}
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02T15:04:05.999999", "2006-01-02T15:04:05", "2006-01-02",
	} {
		if t, err := time.Parse(layout, iso); err == nil {
			return t.UTC().Unix()
		}
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────── the events-service reads

// eventsQuery is a decoded `GET /events` envelope (§3.11).
type eventsQuery struct {
	Events     []canonicalEvent `json:"events"`
	AsOf       string           `json:"asOf"`
	NextCursor *string          `json:"nextCursor"`
	Coverage   string           `json:"coverage"`
	Degraded   []string         `json:"degraded"`
}

// listEvents calls `GET {events}/events` through the ONE client and decodes §3.12.
//
// It is a thin wrapper on `s.eventsGet` and not a client: no transport, no timeout, no base URL of
// its own. Everything it adds is the envelope this caller asked for, which is exactly why
// `eventsGet` hands back bytes.
func (s *Server) listEvents(ctx context.Context, q url.Values) (eventsQuery, error) {
	var out eventsQuery
	body, err := s.eventsGet(ctx, "events?"+q.Encode())
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, err
	}
	return out, nil
}

// listMacro calls `GET {events}/macro` and returns the rows keyed by the event they hang off, so a
// macro event in either feed can carry its actual/expected/surprise.
//
// Fail-soft on its own: a macro read that fails costs the annotation, never the feed. The caller
// gets an empty map and every macro card still renders from the canonical event.
func (s *Server) listMacro(ctx context.Context, since string, limit int) map[string]macroDetail {
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	q.Set("limit", strconv.Itoa(limit))

	body, err := s.eventsGet(ctx, "macro?"+q.Encode())
	if err != nil {
		return map[string]macroDetail{}
	}
	var env struct {
		Macro []struct {
			ID                    string   `json:"id"`
			EventID               string   `json:"eventId"`
			Series                string   `json:"series"`
			ReleaseAt             string   `json:"releaseAt"`
			Actual                *float64 `json:"actual"`
			Expected              *float64 `json:"expected"`
			Previous              *float64 `json:"previous"`
			Surprise              *float64 `json:"surprise"`
			Unit                  string   `json:"unit"`
			ExpectationSource     string   `json:"expectationSource"`
			ExpectationCapturedAt string   `json:"expectationCapturedAt"`
		} `json:"macro"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return map[string]macroDetail{}
	}
	out := make(map[string]macroDetail, len(env.Macro))
	for _, m := range env.Macro {
		if m.EventID == "" {
			// §9.41 made `macro_events.event_id` nullable: 1C writes the row at ingest and 2A
			// backfills the link during assemble(). An unlinked row has no event to annotate, so
			// it is skipped rather than attached to an arbitrary one.
			continue
		}
		out[m.EventID] = macroDetail{
			ID: m.ID, EventID: m.EventID, Series: m.Series,
			ReleaseAt: feedUnix(m.ReleaseAt), ReleaseAtISO: m.ReleaseAt,
			Actual: m.Actual, Expected: m.Expected, Previous: m.Previous, Surprise: m.Surprise,
			Unit: m.Unit, ExpectationSource: m.ExpectationSource,
			ExpectationCapturedAt: m.ExpectationCapturedAt,
		}
	}
	return out
}

// ────────────────────────────────────────────────────────── step 1+2 of the cascade: who to show

// followSet is the resolved ticker universe for one request, plus how it was resolved.
type followSet struct {
	tickers  []string        // ordered, upper-cased, deduped
	set      map[string]bool // membership, for the once-per-event rule and Explore's relation term
	degraded []string
	source   string // "subscriptions" | "universe"
}

func (f followSet) has(sym string) bool { return f.set[strings.ToUpper(sym)] }

// resolveFollowSet runs steps 1 and 2 of the cascade.
//
//  1. signed in + journal answers + non-empty ⇒ the user's subscriptions (§2.1, priority order).
//  2. guest, journal unreachable, or an empty follow graph ⇒ the configured TICKERS universe,
//     and the response SAYS SO.
//
// The empty case gets its own marker rather than reusing `subscriptions:guest`. A signed-in user
// who follows nothing and a guest are different states, and collapsing them would make the one
// case where the product should offer onboarding indistinguishable from the case where it should
// offer sign-in.
func (s *Server) resolveFollowSet(ctx context.Context, id identity) followSet {
	universe := func(marker string) followSet {
		out := followSet{set: map[string]bool{}, degraded: []string{marker}, source: "universe"}
		for _, t := range s.cfg.Tickers {
			sym := strings.ToUpper(t.Symbol)
			if !out.set[sym] {
				out.set[sym] = true
				out.tickers = append(out.tickers, sym)
			}
		}
		return out
	}

	if id.userID == "" {
		return universe(degradedSubscriptionsGuest)
	}

	body, err := s.journalGet(ctx, strings.TrimRight(s.cfg.JournalURL, "/")+"/subscriptions", id.cookie)
	if err != nil {
		return universe(degradedSubscriptionsUnreachable)
	}
	raw, _ := body["subscriptions"].([]any)

	out := followSet{set: map[string]bool{}, source: "subscriptions"}
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		sym := strings.ToUpper(strings.TrimSpace(asString(m["ticker"])))
		if sym == "" || out.set[sym] {
			continue
		}
		out.set[sym] = true
		out.tickers = append(out.tickers, sym)
	}
	if len(out.tickers) == 0 {
		return universe(degradedSubscriptionsEmpty)
	}
	return out
}

// ──────────────────────────────────────────────────────────────────────── the feed item shape

// feedItem is one row of the Following feed.
//
// The numeric event fields are POINTERS ON PURPOSE. On the degraded path a headline from `newsFor`
// has no importance, no novelty and no document count — the events service that computes them is
// the thing that is down. `omitempty` on a nil pointer omits the key entirely, which is the honest
// answer; `0.0` would read as "scored, and scored zero" and would poison any client that sorts on
// it.
type feedItem struct {
	ID                string   `json:"id"`
	EventType         string   `json:"eventType,omitempty"`
	Title             string   `json:"title"`
	Summary           string   `json:"summary,omitempty"`
	WhyItMatters      *string  `json:"whyItMatters,omitempty"`
	KeyFacts          []string `json:"keyFacts,omitempty"`
	OccurredAt        int64    `json:"occurredAt,omitempty"`
	OccurredAtISO     string   `json:"occurredAtISO,omitempty"`
	PublishedAt       int64    `json:"publishedAt"`
	PublishedAtISO    string   `json:"publishedAtISO"`
	SourceTier        string   `json:"sourceTier,omitempty"`
	OfficialConfirmed *bool    `json:"officialConfirmed,omitempty"`
	Importance        *float64 `json:"importance,omitempty"`
	// ImportanceBand is the SERVED band — "high" | "medium" | "low" — computed here by
	// `importanceBand` from the same `importanceHighMin` / `importanceMediumMin` constants that
	// produce `counts.byImportance`. It exists so the client stops keeping a hand-synced second
	// copy of those two thresholds across a language boundary, where nothing fails if one moves and
	// the other does not. Omitted exactly when `Importance` is omitted: an unscored item has no
	// band, and "low" would read as "scored, and scored lowest".
	ImportanceBand string        `json:"importanceBand,omitempty"`
	Novelty        *float64      `json:"novelty,omitempty"`
	DocumentCount  *int          `json:"documentCount,omitempty"`
	Tickers        []feedTicker  `json:"tickers"`
	Macro          *macroDetail  `json:"macro,omitempty"`
	Sources        []eventSource `json:"sources,omitempty"`
	URL            string        `json:"url,omitempty"`
	DataState      string        `json:"dataState"`
	Origin         string        `json:"origin"`
}

// feedTicker is one matching company on a feed item: which followed ticker matched, and what the
// per-ticker enrichment (if any) says about it. `followed` is per-item so the UI can distinguish
// "this is why the item is here" from "this company is also on the event".
type feedTicker struct {
	Ticker string `json:"ticker"`
	// Followed is why the item is in this feed; IsPrimary is whether the event is ABOUT this
	// company. Both are always knowable. Relevance is a POINTER because it is not: it is computed
	// by `services/events` (§9.15), so on the degraded path there is no value and the key is
	// omitted rather than sent as a confident 0.
	Followed           bool     `json:"followed"`
	Relevance          *float64 `json:"relevance,omitempty"`
	IsPrimary          bool     `json:"isPrimary"`
	PotentialDirection string   `json:"potentialDirection,omitempty"`
	PotentialStrength  *float64 `json:"potentialStrength,omitempty"`
	ExpectedHorizon    string   `json:"expectedHorizon,omitempty"`
	AffectedDimensions []string `json:"affectedDimensions,omitempty"`
	Transmission       string   `json:"transmission,omitempty"`
}

// feedCounts reports what the user cares about, not what the query returned: how many, how they
// split by importance, and how many of their companies are actually involved.
type feedCounts struct {
	Total             int            `json:"total"`
	ByImportance      map[string]int `json:"byImportance"`
	CompaniesAffected int            `json:"companiesAffected"`
	Companies         []string       `json:"companies"`
}

type followingResponse struct {
	Items      []feedItem `json:"items"`
	Counts     feedCounts `json:"counts"`
	Tickers    []string   `json:"tickers"`
	Cursor     string     `json:"cursor,omitempty"`
	NextCursor string     `json:"nextCursor,omitempty"`
	Truncated  bool       `json:"truncated"`
	AsOf       string     `json:"asOf"`
	Degraded   []string   `json:"degraded"`
	// Disclaimer is §9.18's ANALYST_DISCLAIMER, carried by the feed that renders
	// `potentialDirection` and `transmission` chips. `docs/DISCLOSURE_CLASSIFICATION.md` rule 3
	// requires the string ADJACENT to the output it qualifies, and it is a server-supplied constant
	// — a browser that composed the sentence itself would satisfy the letter and destroy the point,
	// which is that one place owns the words. `/api/explore` already mirrors it onto every item
	// (`gateway/explore.go`); this is the same string on the other feed.
	Disclaimer string `json:"disclaimer"`
}

// importanceBand buckets a deterministic importance into the three names the counts report. The
// thresholds are named constants because "high" appearing in a payload is a claim, and a claim
// needs a stated definition.
const (
	importanceHighMin   = 0.70
	importanceMediumMin = 0.40
)

func importanceBand(v float64) string {
	switch {
	case v >= importanceHighMin:
		return "high"
	case v >= importanceMediumMin:
		return "medium"
	default:
		return "low"
	}
}

// ────────────────────────────────────────────────────────────────────── GET /api/following

// handleFollowing answers "what materially happened to the companies I follow?" (doc §15.2).
func (s *Server) handleFollowing(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), feedsTimeout)
	defer cancel()

	id := s.identityFrom(r)
	uid := id.userID
	if uid == "" {
		uid = "guest"
	}
	q := parseFollowingQuery(r.URL.Query())

	// PER-USER CACHE KEY, invariant #5. `ttlCache` is ONE process-wide map: a key that omits the
	// caller's identity hands the next caller the previous caller's follow graph. The uid is in the
	// key, and the normalised query is hashed rather than concatenated so a long filter set cannot
	// produce an unbounded key.
	key := "following:" + uid + ":" + feedQueryHash(q.normalised())
	if cached, ok := s.cache.get(key); ok {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cached)
		return
	}

	resp := s.buildFollowing(ctx, id, q)
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

// followingQuery is the validated request. Every field has a documented default; nothing here is
// remembered between requests (locked decision #5).
type followingQuery struct {
	tickers       []string // requested subset, intersected with the follow set
	types         []string // §3.4 members only; unknown members are ignored, not an error
	minImportance float64
	hasMin        bool
	since         int64 // unix seconds; 0 ⇒ followingWindowDays back
	limit         int
	cursor        string
}

func parseFollowingQuery(v url.Values) followingQuery {
	q := followingQuery{limit: followingDefaultLimit, cursor: strings.TrimSpace(v.Get("cursor"))}

	for _, t := range splitCSV(v.Get("tickers")) {
		q.tickers = append(q.tickers, strings.ToUpper(t))
	}
	// An unknown event type is IGNORED, not rejected. The enum is closed (§3.4) and a client that
	// sends a stale member should get the rest of its filter honoured, not a 400 on a feed.
	for _, t := range splitCSV(v.Get("types")) {
		if eventTypes[t] {
			q.types = append(q.types, t)
		}
	}
	if raw := strings.TrimSpace(v.Get("minImportance")); raw != "" {
		if f, err := strconv.ParseFloat(raw, 64); err == nil && f >= 0 && f <= 1 {
			q.minImportance, q.hasMin = f, true
		}
	}
	if raw := strings.TrimSpace(v.Get("since")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			q.since = n
		}
	}
	if raw := strings.TrimSpace(v.Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			q.limit = min(n, followingMaxLimit)
		}
	}
	return q
}

// normalised renders the query in a canonical form so two spellings of one request share a cache
// entry, and two different requests never do.
func (q followingQuery) normalised() string {
	t := append([]string(nil), q.tickers...)
	sort.Strings(t)
	ty := append([]string(nil), q.types...)
	sort.Strings(ty)
	mi := "-"
	if q.hasMin {
		mi = strconv.FormatFloat(q.minImportance, 'f', 4, 64)
	}
	return strings.Join(t, ",") + "|" + strings.Join(ty, ",") + "|" + mi + "|" +
		strconv.FormatInt(q.since, 10) + "|" + strconv.Itoa(q.limit) + "|" + q.cursor
}

func feedQueryHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// buildFollowing runs the whole cascade and returns the response, degraded markers included.
func (s *Server) buildFollowing(ctx context.Context, id identity, q followingQuery) followingResponse {
	now := time.Now().UTC()
	follow := s.resolveFollowSet(ctx, id)
	degraded := append([]string(nil), follow.degraded...)

	// The requested subset is INTERSECTED with the follow set. A ticker the user does not follow is
	// ignored, not an error — the filter is a view onto their own feed, and a 400 here would make a
	// stale bookmark look like a broken product.
	scope := follow.tickers
	if len(q.tickers) > 0 {
		scope = nil
		for _, t := range q.tickers {
			if follow.has(t) {
				scope = append(scope, strings.ToUpper(t))
			}
		}
	}

	since := q.since
	if since == 0 {
		since = now.AddDate(0, 0, -followingWindowDays).Unix()
	}

	items, origin, evDegraded := s.followingItems(ctx, scope, q, since)
	degraded = append(degraded, evDegraded...)

	// Sort (publishedAt DESC, id DESC) — a TOTAL order. Anything less and the cursor skips items
	// whenever two events share a published second, which is common: one ingest run stamps a whole
	// batch.
	sort.Slice(items, func(i, j int) bool {
		if items[i].PublishedAt != items[j].PublishedAt {
			return items[i].PublishedAt > items[j].PublishedAt
		}
		return items[i].ID > items[j].ID
	})

	// Counts describe the WHOLE matched set, before the page is cut. A "3 of 40" header that
	// counted only the visible 3 would be useless.
	counts := feedCountsOf(items, follow)

	page, next, truncated := paginateFeed(items, q.cursor, q.limit)
	if page == nil {
		page = []feedItem{}
	}

	_ = origin
	return followingResponse{
		Items:      page,
		Counts:     counts,
		Tickers:    scope,
		Cursor:     q.cursor,
		NextCursor: next,
		Truncated:  truncated,
		AsOf:       now.Format(time.RFC3339),
		Degraded:   dedupeStrings(degraded),
		Disclaimer: analystDisclaimer,
	}
}

// followingItems is steps 3–5 of the cascade, in order, stopping at the first one that answers.
//
//  3. :8004 answers                    ⇒ canonical events (the real feed).
//  4. :8004 unreachable or erroring    ⇒ `newsFor` per ticker, marked events:unreachable.
//  5. `newsFor` empty as well          ⇒ the EMBEDDED SEED for seed-covered tickers, dataState
//     "seed". §9.33: `newsFor` is all network, so it is not the
//     offline floor; `seed.go` is. This is the step that makes
//     invariant #1's offline half answer at all.
func (s *Server) followingItems(ctx context.Context, scope []string, q followingQuery, since int64) ([]feedItem, string, []string) {
	if len(scope) == 0 {
		return []feedItem{}, feedOriginEvents, nil
	}

	// ── step 3
	if items, err := s.followingFromEvents(ctx, scope, q, since); err == nil {
		return items, feedOriginEvents, nil
	} else {
		marker := degradedEventsUnreachable
		if err == errEventsUnconfigured {
			marker = degradedEventsUnconfigured
		}

		// ── step 4
		if items := s.followingFromNews(ctx, scope, since); len(items) > 0 {
			return items, feedOriginNews, []string{marker}
		}

		// ── step 5
		return s.followingFromSeed(scope), feedOriginSeed,
			[]string{marker, degradedNewsUnavailable}
	}
}

// followingFromEvents is the real feed: canonical events from :8004, one row per event.
//
// The filters are pushed DOWN rather than applied here. `services/events` already implements
// `tickers`/`types`/`minImportance`/`since` in SQL (`events.py:712`), with an EXISTS clause
// precisely so a multi-ticker event comes back ONCE per query rather than once per match — which
// is contract §3.8 and the difference between this feed and the five duplicate headlines `newsFor`
// produces today. Re-filtering in Go would be slower and would risk disagreeing with the store.
func (s *Server) followingFromEvents(ctx context.Context, scope []string, q followingQuery, since int64) ([]feedItem, error) {
	v := url.Values{}
	v.Set("tickers", strings.Join(scope, ","))
	if len(q.types) > 0 {
		v.Set("types", strings.Join(q.types, ","))
	}
	if q.hasMin {
		v.Set("minImportance", strconv.FormatFloat(q.minImportance, 'f', -1, 64))
	}
	v.Set("since", time.Unix(since, 0).UTC().Format(time.RFC3339))
	v.Set("limit", strconv.Itoa(followingFetchLimit))

	env, err := s.listEvents(ctx, v)
	if err != nil {
		return nil, err
	}

	// Macro annotation is best-effort and never gates the feed (see listMacro).
	macro := s.listMacro(ctx, v.Get("since"), followingFetchLimit)

	followed := map[string]bool{}
	for _, t := range scope {
		followed[t] = true
	}

	seen := map[string]bool{}
	items := make([]feedItem, 0, len(env.Events))
	for _, ev := range env.Events {
		if ev.ID == "" || seen[ev.ID] {
			continue // ONE EVENT APPEARS ONCE, even if it matches three followed tickers.
		}
		seen[ev.ID] = true
		items = append(items, feedItemFromEvent(ev, followed, macro))
	}
	return items, nil
}

// feedItemFromEvent maps §3.12 onto the feed row, converting time exactly once.
func feedItemFromEvent(ev canonicalEvent, followed map[string]bool, macro map[string]macroDetail) feedItem {
	tickers := make([]feedTicker, 0, len(ev.Tickers))
	for _, t := range ev.Tickers {
		relevance := t.Relevance
		tickers = append(tickers, feedTicker{
			Ticker: t.Ticker, Followed: followed[strings.ToUpper(t.Ticker)],
			Relevance: &relevance, IsPrimary: t.IsPrimary,
			PotentialDirection: t.PotentialDirection, PotentialStrength: t.PotentialStrength,
			ExpectedHorizon: t.ExpectedHorizon, AffectedDimensions: t.AffectedDimensions,
			Transmission: t.Transmission,
		})
	}
	importance, novelty, docs, confirmed := ev.Importance, ev.Novelty, ev.DocumentCount, ev.OfficialConfirmed
	item := feedItem{
		ID: ev.ID, EventType: ev.EventType, Title: ev.Title, Summary: ev.Summary,
		WhyItMatters: ev.WhyItMatters, KeyFacts: ev.KeyFacts,
		OccurredAt: feedUnix(ev.OccurredAt), OccurredAtISO: ev.OccurredAt,
		PublishedAt: feedUnix(ev.PublishedAt), PublishedAtISO: ev.PublishedAt,
		SourceTier: ev.SourceTier, OfficialConfirmed: &confirmed,
		Importance: &importance, Novelty: &novelty, DocumentCount: &docs,
		ImportanceBand: importanceBand(importance),
		Tickers:        tickers, Sources: ev.Sources,
		DataState: dataStateLive, Origin: feedOriginEvents,
	}
	if m, ok := macro[ev.ID]; ok {
		item.Macro = &m
	}
	if len(ev.Sources) > 0 {
		item.URL = ev.Sources[0].URL
	}
	return item
}

// followingFromNews is step 4: the existing `newsFor` cascade, one call per ticker.
//
// `newsFor` is called, never modified (invariant #4 of the lane brief): same 20-item cap, same
// cache key, same providers, same source label. The mapping is deliberately LOSSY IN THE HONEST
// DIRECTION — a headline is not a canonical event, so importance, novelty, documentCount, sourceTier
// and eventType are simply absent rather than defaulted.
func (s *Server) followingFromNews(ctx context.Context, scope []string, since int64) []feedItem {
	var items []feedItem
	seen := map[string]bool{}

	for _, ticker := range scope {
		news, source := s.newsFor(ctx, ticker)
		state := dataStateForNewsSource(source)
		for _, n := range news {
			headline := asString(n["headline"])
			if headline == "" {
				continue
			}
			// `newsFor` dedupes by exact lowercased headline WITHIN one ticker; across tickers the
			// same wire story arrives twice. The feed's whole premise is one item per happening,
			// so the dedupe is repeated here across the merged set.
			dk := strings.ToLower(headline)
			if seen[dk] {
				continue
			}
			seen[dk] = true

			publishedISO := asString(n["publishedAt"])
			at := feedUnix(publishedISO)
			if at != 0 && at < since {
				continue
			}
			items = append(items, feedItem{
				ID:             newsItemID(ticker, headline, publishedISO),
				Title:          headline,
				Summary:        asString(n["note"]),
				PublishedAt:    at,
				PublishedAtISO: publishedISO,
				Tickers:        []feedTicker{{Ticker: ticker, Followed: true, IsPrimary: true}},
				URL:            asString(n["url"]),
				DataState:      state,
				Origin:         feedOriginNews,
			})
		}
	}
	// `newsFor` caps at 20 per ticker and this path does not widen it; the merged set is capped at
	// the same number per ticker in scope so a five-ticker feed is not a hundred-item wall.
	if cap := feedNewsCap * len(scope); len(items) > cap {
		items = items[:cap]
	}
	return items
}

// followingFromSeed is step 5 — the offline floor (§9.33).
//
// `newsFor` is Marketaux + Google News RSS + SEC EDGAR: ALL NETWORK. With no network it returns
// nothing for a non-seeded ticker, and a feed that stopped there would leave invariant #1's offline
// half with an empty page. The embedded seed (`//go:embed seed/nvda.json`) is compiled into the
// binary, so this step cannot fail for a seed-covered ticker, and every item it emits is labelled
// `dataState: "seed"` so nobody mistakes it for today's news.
func (s *Server) followingFromSeed(scope []string) []feedItem {
	items := []feedItem{}
	for _, ticker := range scope {
		if !isSeedTicker(ticker) {
			continue
		}
		for _, n := range seedNews() {
			headline := asString(n["headline"])
			if headline == "" {
				continue
			}
			publishedISO := asString(n["publishedAt"])
			items = append(items, feedItem{
				ID:             newsItemID(ticker, headline, publishedISO),
				Title:          headline,
				Summary:        asString(n["note"]),
				PublishedAt:    feedUnix(publishedISO),
				PublishedAtISO: publishedISO,
				Tickers: []feedTicker{{
					Ticker: strings.ToUpper(ticker), Followed: true, IsPrimary: true,
				}},
				URL:       asString(n["url"]),
				DataState: dataStateSeed,
				Origin:    feedOriginSeed,
			})
		}
	}
	return items
}

// dataStateForNewsSource maps `newsFor`'s composite source label onto the gateway's three-value
// `dataState` (changes.go:982). `newsFor` returns things like "marketaux+rss+sec8k", "seed+rss",
// "seed" or "none"; anything with a live provider in it is live data, "seed" alone is seed.
func dataStateForNewsSource(source string) string {
	for _, live := range []string{"marketaux", "rss", "sec8k"} {
		if strings.Contains(source, live) {
			return dataStateLive
		}
	}
	if strings.Contains(source, "seed") {
		return dataStateSeed
	}
	return dataStateSeed
}

// newsItemID is deterministic from (ticker, headline, publishedAt) so the same degraded feed
// produces the same ids across calls — the property that makes the cursor stable and the response
// diffable. Prefixed `nws_` so nothing can mistake it for an `evt_` id from :8004.
func newsItemID(ticker, headline, publishedAt string) string {
	sum := sha256.Sum256([]byte(strings.ToUpper(ticker) + "\x00" +
		strings.ToLower(headline) + "\x00" + publishedAt))
	return "nws_" + hex.EncodeToString(sum[:])[:16]
}

// feedCountsOf reports totals over the WHOLE matched set. Items with no importance (the degraded
// path) are counted in `total` and in the companies figure but not in a band — inventing a band for
// a headline that was never scored would be exactly the kind of quiet fabrication the degraded
// markers exist to prevent.
func feedCountsOf(items []feedItem, follow followSet) feedCounts {
	counts := feedCounts{
		ByImportance: map[string]int{"high": 0, "medium": 0, "low": 0},
		Companies:    []string{},
	}
	companies := map[string]bool{}
	for _, it := range items {
		counts.Total++
		if it.Importance != nil {
			counts.ByImportance[importanceBand(*it.Importance)]++
		}
		for _, t := range it.Tickers {
			sym := strings.ToUpper(t.Ticker)
			if follow.has(sym) {
				companies[sym] = true
			}
		}
	}
	for sym := range companies {
		counts.Companies = append(counts.Companies, sym)
	}
	sort.Strings(counts.Companies)
	counts.CompaniesAffected = len(counts.Companies)
	return counts
}

// paginateFeed cuts a page out of the sorted set using an opaque cursor.
//
// The cursor encodes the LAST ITEM OF THE PREVIOUS PAGE, `publishedAt|id`, and the next page is
// everything strictly after it in the same total order. Encoding a position (an offset) instead
// would shift under any insertion; encoding the boundary item cannot.
func paginateFeed(items []feedItem, cursor string, limit int) ([]feedItem, string, bool) {
	start := 0
	if cursor != "" {
		if at, id, ok := decodeFeedCursor(cursor); ok {
			for i, it := range items {
				if it.PublishedAt < at || (it.PublishedAt == at && it.ID < id) {
					start = i
					break
				}
				start = i + 1
			}
		}
	}
	if start >= len(items) {
		return []feedItem{}, "", false
	}
	end := min(start+limit, len(items))
	page := items[start:end]
	if end < len(items) {
		last := page[len(page)-1]
		return page, encodeFeedCursor(last.PublishedAt, last.ID), true
	}
	return page, "", false
}

func encodeFeedCursor(at int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at, 10) + "|" + id))
}

func decodeFeedCursor(c string) (int64, string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", false
	}
	atStr, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return 0, "", false
	}
	at, err := strconv.ParseInt(atStr, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return at, id, true
}

func dedupeStrings(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// withOwnedKeys adds the gateway's OWN top-level keys to the read-through envelope WITHOUT touching
// a byte the store sent. Two of them today: §9.18's `disclaimer`, and the `importanceBand`.
//
// A VERBATIM PROXY MAY ADD, NEVER ALTER. The byte-verbatim guarantee above exists so the client and
// the store can never disagree about a field the STORE owns — "a detail page that disagreed with
// the store about a field name would be a bug that only ever appeared on one screen". Adding a
// top-level sibling the GATEWAY owns does not touch that guarantee: no upstream key is modified,
// renamed, reordered or removed. Forbidding the add would instead force analyst vocabulary down
// into `services/events`, which holds none and should hold none.
//
// So the disclosure travels with the payload it qualifies. §9.18 makes it a server-supplied
// constant precisely so one place owns the words; without this, `GET /api/events/{id}` is a surface
// that serves `potentialDirection` and no disclosure, and the client must withhold the direction.
//
// `importanceBand` is here for a defect caught on a screenshot rather than in a test, and it is
// worth stating because it is the failure this whole wave is about. The client stopped re-deriving
// the band and started reading the served one — correct — but `/api/following` serves it and this
// read-through did not, because the STORE does not compute bands. So one event rendered `HIGH
// IMPACT` on the feed card and `ROUTINE` in the detail panel beside it, on one screen, with nothing
// failing anywhere. Same shape as the threshold drift, one endpoint later. The band is computed from
// the `importance` the store DOES send, by the same `importanceBand` every other surface uses.
//
// It is a TOP-LEVEL sibling, deliberately, and not a key written into the store's nested `event`
// object: §9.59 binding 1 permits adding what the gateway owns and binding 2 forbids modifying what
// the upstream sent, and reaching inside `event` to add a field would be the second thing wearing
// the first thing's clothes.
//
// The splice is TEXTUAL on purpose. Unmarshalling into a map and re-marshalling would sort every
// key and reformat every number — that is exactly the "reorder" this rule forbids, and it would
// break the guarantee while appearing to keep it.
func withOwnedKeys(body []byte) []byte {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return body // not a JSON object — there is no envelope to add a sibling to
	}
	// The decode is a READ-ONLY test — the body that goes out is still the original bytes.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return body // unparseable upstream is passed through untouched rather than guessed at
	}

	added := make([]byte, 0, 256)
	appendKey := func(key string, value any) {
		// If the store ever starts sending this key ITSELF, the store's value wins and nothing is
		// added: a duplicate key is a modification by another name (§9.59 binding 4).
		if _, taken := probe[key]; taken {
			return
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return
		}
		if len(added) > 0 {
			added = append(added, ',')
		}
		added = append(added, '"')
		added = append(added, key...)
		added = append(added, `":`...)
		added = append(added, encoded...)
	}

	appendKey("disclaimer", analystDisclaimer)

	// The band, from the importance the store sent. Absent importance ⇒ no band, exactly as the
	// feeds do: an unscored event has no band, and "low" would read as "scored, and scored lowest".
	if raw, ok := probe["event"]; ok {
		var ev struct {
			Importance *float64 `json:"importance"`
		}
		if json.Unmarshal(raw, &ev) == nil && ev.Importance != nil {
			appendKey("importanceBand", importanceBand(*ev.Importance))
		}
	}

	if len(added) == 0 {
		return body
	}

	rest := trimmed[1:]
	out := make([]byte, 0, len(trimmed)+len(added)+8)
	out = append(out, '{')
	out = append(out, added...)
	// A comma only when the upstream object has members of its own, so `{}` cannot become
	// `{"disclaimer":"…",}`.
	if len(bytes.TrimLeft(rest, " \t\r\n")) > 0 && bytes.TrimLeft(rest, " \t\r\n")[0] != '}' {
		out = append(out, ',')
	}
	return append(out, rest...)
}

// ──────────────────────────────────────────────────────────────── GET /api/events/{id} (§9.2)

// handleEventRead is the read-through. The frontend never calls :8004 directly (§9.3), so this
// route exists purely so it does not have to.
//
// It is a PROXY, not an aggregator: on success the upstream body is written back BYTE-IDENTICAL.
// No reshaping, no renaming, no `feedItem` mapping — a detail page that disagreed with the store
// about a field name would be a bug that only ever appeared on one screen.
func (s *Server) handleEventRead(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), feedsTimeout)
	defer cancel()

	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "event id is required"})
		return
	}
	path := "events/" + url.PathEscape(id)
	if asOf := strings.TrimSpace(r.URL.Query().Get("as_of")); asOf != "" {
		path += "?as_of=" + url.QueryEscape(asOf)
	}

	body, err := s.eventsGet(ctx, path)
	if err == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(withOwnedKeys(body)) // upstream bytes verbatim, gateway-owned siblings added
		return
	}

	// An upstream STATUS is the upstream's answer and is passed through with its detail: a missing
	// id is a 404 here because it is a 404 there, and turning it into a 200-with-null would make
	// "no such event" indistinguishable from "the event exists and is empty".
	if status, detail, ok := eventsErrStatus(err); ok {
		writeJSON(w, status, map[string]any{"detail": detail})
		return
	}

	// No status means no answer: transport failure, or no events service configured. That is a
	// DEGRADED READ, not a server error — the gateway is fine, its upstream is not — so it is
	// reported as a typed marker the client can branch on rather than a 500 it can only log.
	marker := degradedEventsUnreachable
	if err == errEventsUnconfigured {
		marker = degradedEventsUnconfigured
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"event": nil, "documents": []any{}, "tickers": []any{},
		"asOf": time.Now().UTC().Format(time.RFC3339), "degraded": []string{marker},
	})
}

// eventsErrStatus recovers the upstream HTTP status from an `eventsGet` error.
//
// This parses a string, which is ugly, and it is done here rather than fixed at the source because
// `gateway/events.go` is Wave 0's file and this lane may not edit it. `eventsGet` formats a non-2xx
// as `events {url} -> {code}: {body}` and truncates the body at 200 bytes; the handoff records the
// one-line change (a typed error carrying status + body) that would remove this function.
func eventsErrStatus(err error) (int, string, bool) {
	if err == nil {
		return 0, "", false
	}
	msg := err.Error()
	i := strings.LastIndex(msg, " -> ")
	if i < 0 {
		return 0, "", false
	}
	rest := msg[i+len(" -> "):]
	codeStr, detail, ok := strings.Cut(rest, ": ")
	if !ok {
		return 0, "", false
	}
	code, convErr := strconv.Atoi(codeStr)
	if convErr != nil || code < 400 || code > 599 {
		return 0, "", false
	}
	// The upstream body is FastAPI's `{"detail": "…"}`; unwrap it so the client sees the same shape
	// it would have seen calling :8004 directly, rather than a JSON document inside a string.
	var envelope struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal([]byte(detail), &envelope) == nil && envelope.Detail != "" {
		detail = envelope.Detail
	}
	return code, detail, true
}

// eventTypes is §3.4's closed enum, used to ignore an unknown `types=` member rather than reject
// the request. It is a copy of the Python tuple by necessity — the gateway is stdlib-only Go with
// zero modules (invariant #5) and cannot import it — and it is exhaustive as of FROZEN v1.8.
var eventTypes = map[string]bool{
	"earnings_result": true, "earnings_guidance": true, "analyst_revision": true,
	"product_launch": true, "management_change": true, "ma_transaction": true,
	"regulatory_action": true, "legal_action": true, "supply_chain": true,
	"capital_return": true, "financing": true, "partnership": true,
	"macro_release": true, "central_bank": true, "sector_event": true, "other": true,
}
