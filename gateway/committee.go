package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// committee.go — the analyst-committee agent pipeline (ai-hedge-fund style, invariant-preserving).
//
// The gateway assembles the FULL fact set (computed regime incl. key levels, multi-timeframe
// confluence, EPS beat/miss history, news + filings, crowd sentiment) and hands it to the llm
// service's /committee endpoint, which runs 3 persona analysts -> bull/bear debate -> judge,
// sequentially. Stances are analytical leans, NEVER buy/sell verdicts — the only actionable signal
// remains the backtest-gated prediction service.
//
// ON-DEMAND ONLY (invariant #4): this is not part of the dashboard fan-out and must never be
// polled. Cached at CommitteeTTL; ?refresh=1 busts the cache for an explicit re-run.

const committeeDisclaimer = "Analytical stances from AI personas — descriptive leans, NOT " +
	"trade recommendations or targets."

// handleCommittee returns the (cached) committee run for a ticker.
func (s *Server) handleCommittee(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	key := "committee:" + ticker

	if r.URL.Query().Get("refresh") == "" {
		if cached, ok := s.cache.get(key); ok {
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cached)
			return
		}
	}

	// The pipeline makes up to 12 sequential local-model calls — give it room.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	contextObj := s.committeeContextFor(ctx, ticker)
	res, err := s.postJSON(ctx, s.cfg.LLMURL+"/committee", map[string]any{
		"ticker":  ticker,
		"context": contextObj,
	})
	if err != nil {
		// The llm SERVICE itself is unreachable (its own model-down path returns stubs, not errors).
		writeJSON(w, http.StatusOK, map[string]any{
			"ticker": ticker, "available": false, "error": err.Error(),
			"disclaimer": committeeDisclaimer,
		})
		return
	}
	res["available"] = true
	res["contextUsed"] = contextSections(contextObj)

	buf, _ := json.Marshal(res)
	s.cache.set(key, buf, s.cfg.CommitteeTTL)
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf)
}

// handleCommitteeHistory proxies the llm service's persisted committee snapshots.
func (s *Server) handleCommitteeHistory(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	res, err := s.getJSON(r.Context(), s.cfg.LLMURL+"/committee/"+ticker)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// committeeContextFor gathers the committee's evidence. Richer than the brief context on purpose:
// the technical analyst gets the FULL computed regime (incl. keyLevels), fundamentals gets the
// earnings HISTORY (not just the latest quarter), news gets more headlines. Best-effort throughout —
// a missing source just means that analyst sees a thin slice and says so.
func (s *Server) committeeContextFor(ctx context.Context, ticker string) map[string]any {
	c := map[string]any{"ticker": ticker, "asOf": time.Now().UTC().Format(time.RFC3339)}

	// full computed regime + expected-close band (one analysis call).
	if res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+ticker+"?timeframe=1D"); err == nil {
		if reg, ok := res["regime"].(map[string]any); ok {
			c["regime"] = reg
		}
		if ec, ok := res["expectedClose"]; ok && ec != nil {
			c["expectedClose"] = ec
		}
	}

	// multi-timeframe confluence (deterministic).
	if res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+ticker+"/confluence?timeframes="+confluenceTimeframes); err == nil {
		if conf, ok := res["confluence"].(map[string]any); ok {
			c["confluence"] = conf
		}
	}

	// earnings HISTORY (up to 8 quarters) for the fundamentals analyst.
	if earn, _ := s.earningsFor(ctx, ticker); len(earn) > 0 {
		if len(earn) > 8 {
			earn = earn[:8]
		}
		c["earnings"] = earn
	}

	// news (top ~8) + today's new 8-K filings, from the cached merged feed.
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
		if len(news) < 8 {
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

	// crowd sentiment (descriptive; fail-silent).
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
