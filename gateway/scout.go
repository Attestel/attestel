package main

// scout.go — company-level research leads from the events service's immutable Scout snapshots.
//
// The events service owns the global, versioned base rank.  The gateway adds only user-scoped
// context: followed companies, portfolio holdings, and deterministic relationship/sector overlap.
// It never calls prediction or llm, and it never turns a rank into a buy/sell/hold verdict.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

func init() {
	registerEventRoute(func(s *Server, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/scout", s.handleScout)
	})
}

const (
	scoutDefaultLimit       = 12
	scoutMaxLimit           = 50
	scoutFetchLimit         = 100
	scoutBaseWeight         = 0.85
	scoutRelationshipWeight = 0.15
)

const scoutDisclaimer = "Research lead, not an investment recommendation. " +
	"Open the evidence and run any Qwen research explicitly before drawing a conclusion."

type scoutCandidate struct {
	Ticker                 string           `json:"ticker"`
	Rank                   int              `json:"rank"`
	BaseRank               int              `json:"baseRank"`
	AttentionScore         float64          `json:"attentionScore"`
	BaseAttentionScore     float64          `json:"baseAttentionScore"`
	AttentionBand          string           `json:"attentionBand"`
	Components             map[string]any   `json:"components"`
	WhyNow                 string           `json:"whyNow"`
	WhyYouAreSeeingThis    string           `json:"whyYouAreSeeingThis"`
	Evidence               []map[string]any `json:"evidence"`
	RelatedTickers         []string         `json:"relatedTickers"`
	RelatedToYourCompanies []string         `json:"relatedToYourCompanies"`
	LatestEvidenceAt       string           `json:"latestEvidenceAt"`
	SourceTiers            []string         `json:"sourceTiers"`
	DataState              string           `json:"dataState"`
	Follow                 exploreFollow    `json:"follow"`
	Disclaimer             string           `json:"disclaimer"`
}

type scoutStoreEnvelope struct {
	RunID           *string          `json:"runId"`
	ScoreVersion    string           `json:"scoreVersion"`
	UniverseVersion string           `json:"universeVersion"`
	AsOf            *string          `json:"asOf"`
	Coverage        map[string]any   `json:"coverage"`
	Candidates      []scoutCandidate `json:"candidates"`
	Degraded        []string         `json:"degraded"`
}

type scoutResponse struct {
	RunID                  *string          `json:"runId"`
	ScoreVersion           string           `json:"scoreVersion"`
	PersonalizationVersion string           `json:"personalizationVersion"`
	UniverseVersion        string           `json:"universeVersion"`
	AsOf                   *string          `json:"asOf"`
	Coverage               map[string]any   `json:"coverage"`
	Candidates             []scoutCandidate `json:"candidates"`
	Following              []string         `json:"following"`
	PortfolioTickers       []string         `json:"portfolioTickers"`
	Degraded               []string         `json:"degraded"`
}

const scoutPersonalizationVersion = "scout-personalization@1"

const degradedScoutPortfolioUnreachable = "portfolio:unreachable"

func emptyScoutResponse() scoutResponse {
	return scoutResponse{
		PersonalizationVersion: scoutPersonalizationVersion,
		Coverage:               map[string]any{"state": "insufficient"},
		Candidates:             []scoutCandidate{}, Following: []string{}, PortfolioTickers: []string{},
		Degraded: []string{},
	}
}

func (s *Server) handleScout(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), feedsTimeout)
	defer cancel()

	limit := scoutDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = min(n, scoutMaxLimit)
		}
	}
	id := s.identityFrom(r)
	uid := id.userID
	if uid == "" {
		uid = "guest"
	}
	key := "scout:" + uid + ":" + strconv.Itoa(limit)
	if cached, ok := s.cache.get(key); ok {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cached)
		return
	}

	resp := s.buildScout(ctx, id, limit)
	body, err := json.Marshal(resp)
	if err != nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	cacheState := "BYPASS"
	if scoutResponseCacheable(resp.Degraded) {
		s.cache.set(key, body, s.cfg.CalendarTTL)
		cacheState = "MISS"
	}
	w.Header().Set("X-Cache", cacheState)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func scoutResponseCacheable(degraded []string) bool {
	for _, marker := range degraded {
		switch marker {
		case degradedEventsUnreachable, degradedEventsUnconfigured,
			degradedSubscriptionsUnreachable, degradedScoutPortfolioUnreachable:
			return false
		}
	}
	return true
}

func (s *Server) buildScout(ctx context.Context, id identity, limit int) scoutResponse {
	out := emptyScoutResponse()
	follow := s.resolveFollowSet(ctx, id)
	out.Following = append([]string(nil), follow.tickers...)
	out.Degraded = append(out.Degraded, follow.degraded...)

	// The configured-universe fallback helps guests browse Following, but it is not a claim that a
	// guest or a signed-in user with an empty graph follows those companies.  Personalization uses
	// only an actual subscription response.
	personalFollow := follow
	if follow.source != "subscriptions" {
		personalFollow = followSet{set: map[string]bool{}, tickers: []string{}}
	}

	holdings, portfolioDegraded := s.scoutPortfolioTickers(ctx, id)
	out.PortfolioTickers = sortedSet(holdings)
	out.Degraded = append(out.Degraded, portfolioDegraded...)

	v := url.Values{}
	v.Set("limit", strconv.Itoa(scoutFetchLimit))
	body, err := s.eventsGet(ctx, "scout?"+v.Encode())
	if err != nil {
		marker := degradedEventsUnreachable
		if err == errEventsUnconfigured {
			marker = degradedEventsUnconfigured
		}
		out.Degraded = dedupeStrings(append(out.Degraded, marker))
		return out
	}
	var stored scoutStoreEnvelope
	if err := json.Unmarshal(body, &stored); err != nil {
		out.Degraded = dedupeStrings(append(out.Degraded, degradedEventsUnreachable))
		return out
	}

	out.RunID = stored.RunID
	out.ScoreVersion = stored.ScoreVersion
	out.UniverseVersion = stored.UniverseVersion
	out.AsOf = stored.AsOf
	out.Coverage = stored.Coverage
	out.Candidates = personalizeScout(
		stored.Candidates, personalFollow, holdings, limit,
	)
	out.Degraded = dedupeStrings(append(out.Degraded, stored.Degraded...))
	return out
}

func (s *Server) scoutPortfolioTickers(ctx context.Context, id identity) (map[string]bool, []string) {
	out := map[string]bool{}
	if id.userID == "" {
		return out, nil
	}
	body, err := s.journalGet(
		ctx, strings.TrimRight(s.cfg.JournalURL, "/")+"/portfolios", id.cookie,
	)
	if err != nil {
		return out, []string{degradedScoutPortfolioUnreachable}
	}
	portfolios, _ := body["portfolios"].([]any)
	for _, raw := range portfolios {
		portfolio, _ := raw.(map[string]any)
		positions, _ := portfolio["positions"].([]any)
		for _, rawPosition := range positions {
			position, _ := rawPosition.(map[string]any)
			ticker := strings.ToUpper(strings.TrimSpace(asString(position["ticker"])))
			if ticker != "" {
				out[ticker] = true
			}
		}
	}
	return out, nil
}

func sortedSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func personalizeScout(
	candidates []scoutCandidate,
	follow followSet,
	holdings map[string]bool,
	limit int,
) []scoutCandidate {
	covered := map[string]bool{}
	for ticker := range follow.set {
		covered[ticker] = true
	}
	for ticker := range holdings {
		covered[ticker] = true
	}

	result := make([]scoutCandidate, 0, len(candidates))
	for _, original := range candidates {
		candidate := original
		candidate.BaseRank = original.Rank
		candidate.BaseAttentionScore = original.AttentionScore
		candidate.Disclaimer = scoutDisclaimer
		ticker := strings.ToUpper(original.Ticker)
		if ticker == "" || covered[ticker] {
			continue // Scout is the queue outside companies the user already covers.
		}

		relation := 0.0
		related := map[string]bool{}
		for _, raw := range candidate.RelatedTickers {
			symbol := strings.ToUpper(raw)
			if covered[symbol] {
				relation = 1.0
				related[symbol] = true
			}
		}
		if relation == 0 {
			sector := sectorOf[ticker]
			if sector != "" {
				for symbol := range covered {
					if sectorOf[symbol] == sector {
						relation = 0.5
						related[symbol] = true
					}
				}
			}
		}
		candidate.RelatedToYourCompanies = sortedSet(related)
		candidate.AttentionScore = clamp01(
			scoutBaseWeight*original.AttentionScore + scoutRelationshipWeight*relation,
		)
		candidate.Follow = exploreFollow{Ticker: ticker, Followed: follow.has(ticker)}
		switch {
		case relation >= 1 && len(candidate.RelatedToYourCompanies) > 0:
			candidate.WhyYouAreSeeingThis = "Fresh evidence for " + ticker +
				" also touches " + joinAnd(candidate.RelatedToYourCompanies) +
				", which is already in your research coverage."
		case relation > 0 && len(candidate.RelatedToYourCompanies) > 0:
			candidate.WhyYouAreSeeingThis = ticker + " shares the " + sectorOf[ticker] +
				" coverage group with " + joinAnd(candidate.RelatedToYourCompanies) + "."
		default:
			candidate.WhyYouAreSeeingThis =
				"It ranked near the top of the latest company-level Scout pass."
		}
		if strings.TrimSpace(candidate.WhyNow) == "" || strings.TrimSpace(candidate.WhyYouAreSeeingThis) == "" {
			continue
		}
		result = append(result, candidate)
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].AttentionScore != result[j].AttentionScore {
			return result[i].AttentionScore > result[j].AttentionScore
		}
		if result[i].LatestEvidenceAt != result[j].LatestEvidenceAt {
			return result[i].LatestEvidenceAt > result[j].LatestEvidenceAt
		}
		return result[i].Ticker < result[j].Ticker
	})
	if limit < len(result) {
		result = result[:limit]
	}
	for index := range result {
		result[index].Rank = index + 1
	}
	return result
}
