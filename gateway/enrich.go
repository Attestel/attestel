package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// enrich.go — cached wrappers that prefer live providers and fall back to seed data.
// Each returns the payload plus a source label ("live" | "seed") the frontend can display.

type cachedResult struct {
	Source string          `json:"source"`
	Data   json.RawMessage `json:"data"`
}

// newsFor returns news items + source, cached at NewsTTL.
func (s *Server) newsFor(ctx context.Context, ticker string) ([]map[string]any, string) {
	key := "news:" + ticker
	if b, ok := s.cache.get(key); ok {
		var r cachedResult
		if json.Unmarshal(b, &r) == nil {
			var items []map[string]any
			_ = json.Unmarshal(r.Data, &items)
			return items, r.Source
		}
	}

	// Two streams, merged: (1) market news — live Marketaux if keyed, else seed catalysts;
	// (2) SEC 8-K filings — always attempted (free, no key). Merged newest-first.
	var parts []string
	var items []map[string]any

	if live, err := s.fetchNews(ctx, ticker, 12); err == nil {
		parts = append(parts, "marketaux")
		for _, n := range live {
			items = append(items, map[string]any{
				"headline": n.Headline, "source": n.Source, "url": n.URL,
				"publishedAt": n.PublishedAt, "sentiment": n.Sentiment, "score": n.Score,
				"category": "news",
			})
		}
	} else if isSeedTicker(ticker) {
		// Seed catalyst-news is NVDA-specific; only use it for the seeded ticker.
		parts = append(parts, "seed")
		items = append(items, seedNews()...)
	}

	// (1b) keyless Google News RSS — additive; fills the gap when Marketaux is unkeyed/exhausted
	// and widens the feed for the digest's clustering. Fail-soft like every provider.
	if rss, err := s.fetchRSSNews(ctx, ticker, 8); err == nil {
		parts = append(parts, "rss")
		seen := map[string]bool{}
		for _, it := range items {
			if h, _ := it["headline"].(string); h != "" {
				seen[strings.ToLower(h)] = true
			}
		}
		for _, it := range rss {
			if h, _ := it["headline"].(string); h != "" && !seen[strings.ToLower(h)] {
				items = append(items, it)
			}
		}
	}

	if filings, err := s.fetchSECFilings(ctx, ticker, 8); err == nil {
		parts = append(parts, "sec8k")
		for _, f := range filings {
			items = append(items, map[string]any{
				"headline": f.Headline, "source": f.Source, "url": f.URL,
				"publishedAt": f.PublishedAt, "sentiment": f.Sentiment, "category": "filing",
			})
		}
	}

	sortByPublishedDesc(items)
	if len(items) > 20 {
		items = items[:20]
	}
	source := strings.Join(parts, "+")
	if source == "" {
		source = "none" // no live key, non-seed ticker, and SEC unreachable (e.g. offline)
	}

	if data, err := json.Marshal(items); err == nil {
		if wrapped, err := json.Marshal(cachedResult{Source: source, Data: data}); err == nil {
			s.cache.set(key, wrapped, s.cfg.NewsTTL)
		}
	}
	return items, source
}

// sortByPublishedDesc orders items newest-first by the first 10 chars of publishedAt
// (YYYY-MM-DD), which is comparable across SEC (date), Marketaux (ISO), and seed formats.
func sortByPublishedDesc(items []map[string]any) {
	datePrefix := func(m map[string]any) string {
		s, _ := m["publishedAt"].(string)
		if len(s) > 10 {
			return s[:10]
		}
		return s
	}
	sort.SliceStable(items, func(i, j int) bool {
		return datePrefix(items[i]) > datePrefix(items[j])
	})
}

// earningsFor returns EPS beat/miss history + source, cached at EarningsTTL. On fallback it
// derives points from the seed quarters (which carry EPS actual/estimate for recent quarters).
func (s *Server) earningsFor(ctx context.Context, ticker string) ([]map[string]any, string) {
	key := "earnings:" + ticker
	if b, ok := s.cache.get(key); ok {
		var r cachedResult
		if json.Unmarshal(b, &r) == nil {
			var items []map[string]any
			_ = json.Unmarshal(r.Data, &items)
			return items, r.Source
		}
	}

	source := "none"
	var items []map[string]any
	if live, err := s.fetchEarnings(ctx, ticker, 8); err == nil {
		source = "live:alphavantage"
		for _, p := range live {
			items = append(items, map[string]any{
				"fiscalDateEnding": p.FiscalDateEnding, "reportedDate": p.ReportedDate,
				"reportedEPS": p.ReportedEPS, "estimatedEPS": p.EstimatedEPS,
				"surprise": p.Surprise, "surprisePercentage": p.SurprisePercent, "beat": p.Beat,
			})
		}
	} else if isSeedTicker(ticker) {
		// EPS seed is derived from NVDA's verified quarters; only for the seeded ticker.
		source = "seed"
		items = seedEarnings()
	}

	if data, err := json.Marshal(items); err == nil {
		if wrapped, err := json.Marshal(cachedResult{Source: source, Data: data}); err == nil {
			s.cache.set(key, wrapped, s.cfg.EarningsTTL)
		}
	}
	return items, source
}

// sentimentFor returns the StockTwits crowd-sentiment object, cached at SentimentTTL. Fail-silent:
// on any error (disabled/unreachable/rate-limited) it returns { available:false } and the NEGATIVE
// result is cached too, so a blocked endpoint isn't hammered. Never a buy/sell signal.
func (s *Server) sentimentFor(ctx context.Context, ticker string) map[string]any {
	unavailable := map[string]any{"source": "stocktwits", "available": false, "note": sentimentNote}
	if !s.cfg.StockTwitsEnabled {
		return unavailable
	}
	key := "sentiment:" + ticker
	if b, ok := s.cache.get(key); ok {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			return m
		}
	}
	out := unavailable
	if sum, err := s.fetchStockTwits(ctx, ticker); err == nil {
		out = sum
	}
	if data, err := json.Marshal(out); err == nil {
		s.cache.set(key, data, s.cfg.SentimentTTL) // cache success AND failure — don't hammer
	}
	return out
}

// seedEarnings derives an EPS beat/miss series from the seeded quarters so the UI shows the same
// shape as live Alpha Vantage data when no key is set.
func seedEarnings() []map[string]any {
	fund, _ := seedData["fundamentals"].(map[string]any)
	quarters, _ := fund["quarters"].([]any)
	out := make([]map[string]any, 0, len(quarters))
	for _, q := range quarters {
		m, ok := q.(map[string]any)
		if !ok {
			continue
		}
		eps, _ := m["epsNonGaap"].(map[string]any)
		if eps == nil {
			continue
		}
		reported, _ := eps["actual"].(float64)
		estimate, hasEst := eps["estimate"].(float64)
		point := map[string]any{
			"fiscalDateEnding": m["periodEnded"],
			"reportedDate":     m["reported"],
			"label":            m["label"],
			"reportedEPS":      reported,
			"basis":            eps["basis"],
		}
		if hasEst {
			surprise := reported - estimate
			point["estimatedEPS"] = estimate
			point["surprise"] = surprise
			point["surprisePercentage"] = surprise / estimate * 100
			point["beat"] = surprise >= 0
		}
		out = append(out, point)
	}
	return out
}
