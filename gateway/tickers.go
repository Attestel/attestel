package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tickers.go — the configurable ticker universe + ticker→CIK resolution.
//
// The universe drives the frontend switcher (GET /api/tickers). CIKs are needed for SEC 8-K
// filings and are resolved from SEC's company_tickers.json (any ticker, refreshed ~daily) with a
// small built-in fallback so the default universe always works offline.

// Ticker is one entry in the configurable universe surfaced to the frontend.
type Ticker struct {
	Symbol string `json:"symbol"`
	Name   string `json:"name"`
}

// displayNames maps common symbols to a friendly name. Unknown symbols fall back to the symbol.
var displayNames = map[string]string{
	"NVDA": "NVIDIA", "GOOGL": "Alphabet (Google)", "TSLA": "Tesla",
	"AAPL": "Apple", "MSFT": "Microsoft", "AMD": "AMD", "AMZN": "Amazon",
	"META": "Meta", "AVGO": "Broadcom",
}

// commonTickers is the ordered offline search directory. The configured universe is prepended at
// request time; these names fill the remaining suggestions when SEC is unavailable.
var commonTickers = []Ticker{
	{"NVDA", "NVIDIA"}, {"MSFT", "Microsoft"}, {"AAPL", "Apple"},
	{"GOOGL", "Alphabet (Google)"}, {"TSLA", "Tesla"}, {"AMZN", "Amazon"},
	{"META", "Meta"}, {"AMD", "AMD"}, {"AVGO", "Broadcom"},
}

// parseTickers turns a comma-separated env value into a deduped universe. Falls back to the
// default NVDA/GOOGL/TSLA set if the value is empty.
func parseTickers(env string) []Ticker {
	var out []Ticker
	seen := map[string]bool{}
	for _, s := range strings.Split(env, ",") {
		sym := strings.ToUpper(strings.TrimSpace(s))
		if sym == "" || seen[sym] {
			continue
		}
		seen[sym] = true
		name := displayNames[sym]
		if name == "" {
			name = sym
		}
		out = append(out, Ticker{Symbol: sym, Name: name})
	}
	if len(out) == 0 {
		out = []Ticker{
			{"NVDA", "NVIDIA"}, {"GOOGL", "Alphabet (Google)"}, {"TSLA", "Tesla"},
		}
	}
	return out
}

// allowedTimeframes is the set threaded through to the analysis service.
var allowedTimeframes = map[string]bool{"1D": true, "1H": true, "15m": true, "5m": true}

// normalizeTimeframe validates a requested timeframe, defaulting to 1D (backward compatible).
func normalizeTimeframe(tf string) string {
	if allowedTimeframes[tf] {
		return tf
	}
	return "1D"
}

// isSeedTicker reports whether we hold rich seed data for this ticker (NVDA only in v1).
func isSeedTicker(ticker string) bool {
	sym, _ := seedData["ticker"].(string)
	return strings.EqualFold(sym, ticker)
}

// ---------------- ticker → CIK (SEC company_tickers.json) ----------------

// builtinCIK is the fallback used when SEC's company_tickers.json can't be fetched/parsed.
var builtinCIK = map[string]string{
	"NVDA": "0001045810", "GOOGL": "0001652044", "TSLA": "0001318605",
}

// cikCache holds the resolved ticker→CIK map, refreshed roughly daily.
type cikCache struct {
	mu        sync.RWMutex
	m         map[string]string
	directory []Ticker
	fetchedAt time.Time
}

// handleTickerSearch searches the server-side company directory without exposing the full list to
// the browser. Results are deliberately capped at seven: this is a switcher, not a market screener.
func (s *Server) handleTickerSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "query is too long"})
		return
	}
	limit := 7
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n < limit {
			limit = n
		}
	}

	// An empty query is the fast, deterministic suggestion set. A real query may refresh the SEC
	// directory, but the request context still bounds that network attempt and offline always works.
	candidates := offlineTickerDirectory(s.cfg.Tickers)
	if query != "" {
		s.cik.mu.RLock()
		stale := len(s.cik.directory) == 0 || time.Since(s.cik.fetchedAt) >= 24*time.Hour
		s.cik.mu.RUnlock()
		if stale {
			// Keep the lookup bounded and independent of a superseded browser query. The UI debounces,
			// but a user may still type again while SEC is responding; that should not poison the shared
			// cache with a cancelled request.
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			s.refreshCIK(ctx)
			cancel()
		}
		s.cik.mu.RLock()
		if len(s.cik.directory) > 0 {
			// Curated/configured companies come first so familiar exact names win duplicate SEC entries
			// and equally-ranked broad searches remain useful ("micro" should surface Microsoft).
			candidates = append(candidates, s.cik.directory...)
		}
		s.cik.mu.RUnlock()
	}

	writeJSON(w, http.StatusOK, map[string]any{"tickers": searchTickers(candidates, query, limit)})
}

func offlineTickerDirectory(configured []Ticker) []Ticker {
	out := make([]Ticker, 0, len(configured)+len(commonTickers))
	seen := map[string]bool{}
	for _, group := range [][]Ticker{configured, commonTickers} {
		for _, ticker := range group {
			symbol := strings.ToUpper(strings.TrimSpace(ticker.Symbol))
			if symbol == "" || seen[symbol] {
				continue
			}
			seen[symbol] = true
			name := strings.TrimSpace(ticker.Name)
			if name == "" {
				name = symbol
			}
			out = append(out, Ticker{Symbol: symbol, Name: name})
		}
	}
	return out
}

func searchTickers(candidates []Ticker, query string, limit int) []Ticker {
	if limit <= 0 {
		return []Ticker{}
	}
	q := strings.ToUpper(strings.TrimSpace(query))
	if q == "" {
		if len(candidates) < limit {
			limit = len(candidates)
		}
		return append([]Ticker(nil), candidates[:limit]...)
	}

	type match struct {
		ticker Ticker
		rank   int
		order  int
	}
	matches := make([]match, 0)
	seen := map[string]bool{}
	for order, ticker := range candidates {
		symbol := strings.ToUpper(strings.TrimSpace(ticker.Symbol))
		name := strings.TrimSpace(ticker.Name)
		nameUpper := strings.ToUpper(name)
		rank := -1
		switch {
		case symbol == q:
			rank = 0
		case strings.HasPrefix(symbol, q):
			rank = 1
		case strings.HasPrefix(nameUpper, q):
			rank = 2
		case strings.Contains(symbol, q):
			rank = 3
		case strings.Contains(nameUpper, q):
			rank = 4
		}
		if rank < 0 || symbol == "" || seen[symbol] {
			continue
		}
		seen[symbol] = true
		if name == "" {
			name = symbol
		}
		matches = append(matches, match{ticker: Ticker{Symbol: symbol, Name: name}, rank: rank, order: order})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].rank != matches[j].rank {
			return matches[i].rank < matches[j].rank
		}
		return matches[i].order < matches[j].order
	})
	if len(matches) < limit {
		limit = len(matches)
	}
	out := make([]Ticker, limit)
	for i := 0; i < limit; i++ {
		out[i] = matches[i].ticker
	}
	return out
}

// cikFor returns the zero-padded 10-digit CIK for a ticker, refreshing the SEC map if stale and
// falling back to the built-in map on any miss/failure.
func (s *Server) cikFor(ctx context.Context, ticker string) (string, bool) {
	ticker = strings.ToUpper(ticker)

	s.cik.mu.RLock()
	stale := s.cik.m == nil || time.Since(s.cik.fetchedAt) >= 24*time.Hour
	s.cik.mu.RUnlock()
	if stale {
		s.refreshCIK(ctx) // best-effort; never blocks longer than one HTTP timeout
	}

	s.cik.mu.RLock()
	cik, ok := s.cik.m[ticker]
	s.cik.mu.RUnlock()
	if ok {
		return cik, true
	}
	cik, ok = builtinCIK[ticker]
	return cik, ok
}

// refreshCIK (re)loads company_tickers.json. On any failure it ensures at least the built-in map
// is present and stamps fetchedAt so we don't hammer SEC on every request.
func (s *Server) refreshCIK(ctx context.Context) {
	fallback := func() {
		s.cik.mu.Lock()
		if s.cik.m == nil {
			s.cik.m = builtinCIK
		}
		if len(s.cik.directory) == 0 {
			s.cik.directory = offlineTickerDirectory(s.cfg.Tickers)
		}
		s.cik.fetchedAt = time.Now()
		s.cik.mu.Unlock()
	}

	body, err := s.rawGetUA(ctx, "https://www.sec.gov/files/company_tickers.json", s.cfg.SECUserAgent)
	if err != nil {
		fallback()
		return
	}
	// company_tickers.json is an object keyed by index: {"0":{"cik_str":1045810,"ticker":"NVDA",...}}
	var raw map[string]struct {
		CIK    json.Number `json:"cik_str"`
		Ticker string      `json:"ticker"`
		Title  string      `json:"title"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		fallback()
		return
	}
	m := make(map[string]string, len(raw))
	directory := make([]Ticker, 0, len(raw))
	for _, e := range raw {
		if e.Ticker == "" {
			continue
		}
		symbol := strings.ToUpper(e.Ticker)
		m[symbol] = padCIK(e.CIK.String())
		name := strings.TrimSpace(e.Title)
		if name == "" {
			name = symbol
		}
		directory = append(directory, Ticker{Symbol: symbol, Name: name})
	}
	if len(m) == 0 {
		fallback()
		return
	}
	s.cik.mu.Lock()
	s.cik.m = m
	s.cik.directory = directory
	s.cik.fetchedAt = time.Now()
	s.cik.mu.Unlock()
}

// padCIK left-pads a numeric CIK string to SEC's 10-digit form.
func padCIK(s string) string {
	s = strings.TrimSpace(s)
	for len(s) < 10 {
		s = "0" + s
	}
	return s
}
