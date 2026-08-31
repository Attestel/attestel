package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// brief.go — the AI "Market Brief".
//
// It assembles the day's ALREADY-COLLECTED facts (price change, regime + confluence note, top
// headlines, today's new 8-K filings, most-recent earnings beat/miss, crowd sentiment) into a
// compact context object and asks the llm service to READ and COMPRESS them into a structured,
// plain-language digest. This is AI SYNTHESIS, not prediction — descriptive, grounded ONLY in the
// supplied facts, never a buy/sell verdict.
//
// Additive + fail-silent: every input is optional and simply omitted when unavailable (the brief
// never blocks on one source); if the llm service is unreachable the gateway returns a deterministic
// stub built from the same facts. Cached at BriefTTL.

const briefDisclaimer = "AI-generated summary — informational, not investment advice"

// marketIndices are the optional broad-market context tickers for the market brief (best-effort).
var marketIndices = []string{"SPY", "QQQ"}

// briefFor returns the brief for a ticker ("MARKET" for the market-wide brief). The brief BODY is
// the LLM synthesis of PUBLIC market facts and is cached globally; the caller's per-thesis checks
// are PRIVATE, so they're computed/cached PER USER and merged in outside the shared cache — a
// signed-in user's thesis checks never leak into another account's (or a guest's) brief.
func (s *Server) briefFor(ctx context.Context, ticker string, id identity) map[string]any {
	out := s.briefBody(ctx, ticker)
	if ticker != "MARKET" {
		if checks := s.userThesisChecks(ctx, ticker, id); len(checks) > 0 {
			merged := make(map[string]any, len(out)+1)
			for k, v := range out {
				merged[k] = v
			}
			merged["thesisChecks"] = checks
			return merged
		}
	}
	return out
}

// briefBody is the shared, cacheable brief (no per-user data). Cache key is "brief:"+ticker.
func (s *Server) briefBody(ctx context.Context, ticker string) map[string]any {
	key := "brief:" + ticker
	if b, ok := s.cache.get(key); ok {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			return m
		}
	}

	var contextObj map[string]any
	if ticker == "MARKET" {
		contextObj = s.marketBriefContext(ctx)
	} else {
		contextObj = s.briefContextFor(ctx, ticker)
	}

	out := s.requestBrief(ctx, ticker, contextObj)
	out["ticker"] = ticker
	out["contextUsed"] = contextSections(contextObj) // which fact sections were available (provenance)
	// The News tab renders the same deterministic market facts that grounded the synthesis. Returning
	// them beside the read lets the UI show an honest market pulse without parsing model prose or
	// making a second universe-wide request.
	if ticker == "MARKET" {
		out["marketSnapshot"] = contextObj
	}

	if data, err := json.Marshal(out); err == nil {
		s.cache.set(key, data, s.cfg.BriefTTL)
	}
	return out
}

// userThesisChecks re-checks the SIGNED-IN user's active theses for a ticker (auto-on-brief),
// cached PER USER so a private result never rides the shared brief cache and adds no polling. A
// guest (empty user id) has no theses -> nil. Results also persist onto each thesis in the journal.
func (s *Server) userThesisChecks(ctx context.Context, ticker string, id identity) []map[string]any {
	if id.userID == "" {
		return nil
	}
	key := "thesischecks:" + id.userID + ":" + ticker
	if b, ok := s.cache.get(key); ok {
		var checks []map[string]any
		if json.Unmarshal(b, &checks) == nil {
			return checks
		}
	}
	checks := s.thesisChecksFor(ctx, ticker, id.cookie)
	if data, err := json.Marshal(checks); err == nil {
		s.cache.set(key, data, s.cfg.BriefTTL) // caches the "no theses" state too (marshals to null)
	}
	return checks
}

// requestBrief POSTs the assembled context to the llm /brief endpoint. The llm service already
// degrades to its own deterministic stub when the model (Ollama/OpenAI) is down; if the whole llm
// SERVICE is unreachable, we fall back to a gateway-built stub from the same facts.
func (s *Server) requestBrief(ctx context.Context, ticker string, contextObj map[string]any) map[string]any {
	res, err := s.postJSON(ctx, s.cfg.LLMURL+"/brief", map[string]any{
		"ticker":  ticker,
		"context": contextObj,
	})
	if err != nil {
		return gatewayStubBrief(ticker, contextObj)
	}
	return res
}

// briefContextFor gathers the compact fact set for a single ticker. Best-effort throughout.
func (s *Server) briefContextFor(ctx context.Context, ticker string) map[string]any {
	c := map[string]any{"ticker": ticker, "asOf": time.Now().UTC().Format(time.RFC3339)}

	// price + regime (one analysis call).
	if res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+ticker+"?timeframe=1D"); err == nil {
		if p, ok := res["price"].(map[string]any); ok {
			price := map[string]any{}
			if v, ok := p["lastClose"]; ok {
				price["last"] = v
			}
			if v, ok := p["changePct"]; ok {
				price["changePct"] = v
			}
			if v, ok := res["source"]; ok {
				price["source"] = v
			}
			c["price"] = price
		}
		if reg, ok := res["regime"].(map[string]any); ok {
			c["regime"] = trimRegime(reg)
		}
	}

	// multi-timeframe confluence note (cached endpoint). Trim to the descriptive fields.
	if res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+ticker+"/confluence?timeframes="+confluenceTimeframes); err == nil {
		if conf, ok := res["confluence"].(map[string]any); ok {
			c["confluence"] = map[string]any{
				"trendAlignment": conf["trendAlignment"],
				"conflicts":      conf["conflicts"],
				"note":           conf["note"],
			}
		}
	}

	// news (top ~5) + today's NEW 8-K filings (reuse the cached merged feed).
	items, _ := s.newsFor(ctx, ticker)
	today := time.Now().UTC().Format("2006-01-02")
	var news []map[string]any
	var filings []map[string]any
	for _, it := range items {
		if cat, _ := it["category"].(string); cat == "filing" {
			if pub, _ := it["publishedAt"].(string); strings.HasPrefix(pub, today) {
				filings = append(filings, map[string]any{"date": pub, "headline": it["headline"]})
			}
			continue
		}
		if len(news) < 5 {
			news = append(news, map[string]any{
				"headline": it["headline"], "source": it["source"],
				"sentiment": it["sentiment"], "publishedAt": it["publishedAt"],
			})
		}
	}
	if len(news) > 0 {
		c["news"] = news
	}
	if len(filings) > 0 {
		c["filingsToday"] = filings
	}

	// most-recent earnings beat/miss.
	if earn, _ := s.earningsFor(ctx, ticker); len(earn) > 0 {
		c["earnings"] = earn[0]
	}

	// crowd sentiment (descriptive; already fail-silent). Only include when actually available.
	if sent := s.sentimentFor(ctx, ticker); sent != nil {
		if avail, _ := sent["available"].(bool); avail {
			c["sentiment"] = map[string]any{
				"buzz": sent["buzz"], "label": sent["sentimentLabel"],
				"bullish": sent["bullish"], "bearish": sent["bearish"], "spike": sent["spike"],
			}
		}
	}
	return c
}

// marketBriefContext aggregates a compact snapshot across the configured universe (+ optional
// SPY/QQQ), fetched concurrently. Best-effort: a ticker that fails just contributes less.
func (s *Server) marketBriefContext(ctx context.Context) map[string]any {
	c := map[string]any{"scope": "MARKET", "asOf": time.Now().UTC().Format(time.RFC3339)}

	var mu sync.Mutex
	var wg sync.WaitGroup

	// optional broad-market indices.
	var indices []map[string]any
	for _, ix := range marketIndices {
		ix := ix
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+ix+"?timeframe=1D")
			if err != nil {
				return
			}
			if p, ok := res["price"].(map[string]any); ok {
				mu.Lock()
				indices = append(indices, map[string]any{
					"ticker": ix, "changePct": p["changePct"], "last": p["lastClose"],
					"source": res["source"], "sourceIsSynthetic": res["sourceIsSynthetic"],
				})
				mu.Unlock()
			}
		}()
	}

	// per-name snapshot across the tracked universe.
	movers := make([]map[string]any, 0, len(s.cfg.Tickers))
	for _, t := range s.cfg.Tickers {
		sym := t.Symbol
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := map[string]any{"ticker": sym}
			if res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+sym+"?timeframe=1D"); err == nil {
				if p, ok := res["price"].(map[string]any); ok {
					m["changePct"] = p["changePct"]
				}
				m["source"] = res["source"]
				m["sourceIsSynthetic"] = res["sourceIsSynthetic"]
				if reg, ok := res["regime"].(map[string]any); ok {
					m["trend"] = reg["trend"]
				}
			}
			if sent := s.sentimentFor(ctx, sym); sent != nil {
				if avail, _ := sent["available"].(bool); avail {
					m["sentiment"] = sent["sentimentLabel"]
				}
			}
			if items, _ := s.newsFor(ctx, sym); len(items) > 0 {
				m["topHeadline"] = items[0]["headline"]
			}
			mu.Lock()
			movers = append(movers, m)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(indices) > 0 {
		sort.Slice(indices, func(i, j int) bool {
			return indexOf(marketIndices, indices[i]["ticker"]) < indexOf(marketIndices, indices[j]["ticker"])
		})
		c["indices"] = indices
	}
	// restore the configured order (goroutines finish out of order).
	order := map[string]int{}
	for i, t := range s.cfg.Tickers {
		order[t.Symbol] = i
	}
	sort.Slice(movers, func(i, j int) bool {
		return order[asString(movers[i]["ticker"])] < order[asString(movers[j]["ticker"])]
	})
	c["tickers"] = movers
	return c
}

// trimRegime keeps only the descriptive regime fields worth summarizing (drops raw numbers/levels).
func trimRegime(reg map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"trend", "shortTermTrend", "momentum", "macdState", "volatility", "volume", "trendStrength", "priceVsBands", "stochastic", "summary"} {
		if v, ok := reg[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

// contextSections lists which fact groups made it into the context, for a "grounded in …" provenance
// line in the UI. (Order-stable so the cached value is deterministic.)
func contextSections(c map[string]any) []string {
	want := []string{"price", "regime", "expectedClose", "confluence", "signal", "news", "filingsToday", "earnings", "sentiment", "indices", "tickers"}
	var out []string
	for _, k := range want {
		if v, ok := c[k]; ok && v != nil {
			out = append(out, k)
		}
	}
	return out
}

// gatewayStubBrief is the fallback-of-last-resort: used only when the llm SERVICE itself is
// unreachable. A deterministic, honestly-labelled fact list from the same context — never advice.
func gatewayStubBrief(ticker string, c map[string]any) map[string]any {
	what := []any{}
	why := []any{}
	headline := ticker + " — offline brief from collected facts"
	if ticker == "MARKET" {
		if ixs, ok := c["indices"].([]map[string]any); ok {
			var indexFacts []string
			for _, ix := range ixs {
				fact := fmt.Sprintf("%s %s", asString(ix["ticker"]), pctString(ix["changePct"]))
				indexFacts = append(indexFacts, fact)
				what = append(what, fact+" on the day.")
			}
			if len(indexFacts) > 0 {
				headline = "Market snapshot: " + strings.Join(indexFacts, " · ")
			}
		}
		if ms, ok := c["tickers"].([]map[string]any); ok {
			for _, m := range ms {
				what = append(what, moverLine(m))
			}
		}
	} else {
		if p, ok := c["price"].(map[string]any); ok {
			what = append(what, fmt.Sprintf("%s changed %s (%s data).", ticker, pctString(p["changePct"]), asString(p["source"])))
		}
		if reg, ok := c["regime"].(map[string]any); ok && reg["trend"] != nil {
			what = append(what, fmt.Sprintf("Technical regime — trend %s, momentum %s.", asString(reg["trend"]), asString(reg["momentum"])))
		}
		if news, ok := c["news"].([]map[string]any); ok {
			for _, n := range news {
				why = append(why, asString(n["headline"]))
			}
		}
	}
	if len(what) == 0 {
		what = append(what, "No collected facts were available (providers offline).")
	}
	return map[string]any{
		"structured": map[string]any{
			"headline":     headline,
			"whatHappened": what,
			"whyItMoved":   why,
			"calendar":     []any{},
			"watch":        []any{"Reconnect the LLM service for the full synthesized brief."},
			"riskFlags":    []any{},
			"summary": "The AI summarizer is unreachable; this is a deterministic fact list from the day's " +
				"collected data. Descriptive only, not investment advice.",
			"_stub": true,
		},
		"modelUsed":  "stub:gateway-offline",
		"warnings":   []any{},
		"retried":    false,
		"disclaimer": briefDisclaimer,
	}
}

func moverLine(m map[string]any) string {
	parts := []string{asString(m["ticker"])}
	if m["changePct"] != nil {
		parts = append(parts, pctString(m["changePct"]))
	}
	if m["trend"] != nil {
		parts = append(parts, "trend "+asString(m["trend"]))
	}
	if m["sentiment"] != nil {
		parts = append(parts, "crowd "+asString(m["sentiment"]))
	}
	line := strings.Join(parts, " · ")
	if m["topHeadline"] != nil {
		line += " — " + asString(m["topHeadline"])
	}
	return line
}

func pctString(v any) string {
	f, ok := toFloat(v)
	if !ok {
		return "n/a"
	}
	sign := ""
	if f >= 0 {
		sign = "+"
	}
	return fmt.Sprintf("%s%.2f%%", sign, f)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f, true
		}
	}
	return 0, false
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func indexOf(list []string, v any) int {
	s := asString(v)
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return len(list)
}
