package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// confluenceTimeframes is the fixed multi-timeframe set the dashboard computes confluence over.
const confluenceTimeframes = "1D,1H,15m"

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleHealth and handleReady live in health.go. They fan out to the upstreams instead of
// returning a literal — a health endpoint that cannot fail cannot diagnose.

// handleTickers returns the configured universe for the frontend switcher.
func (s *Server) handleTickers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tickers": s.cfg.Tickers})
}

// handleQuote proxies the cheap analysis /quote endpoint for the polled header price. Cached for a
// short QuoteTTL so a 20s frontend poll across a few open tabs doesn't hammer the upstream.
func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	cacheKey := "quote:" + ticker
	if cached, ok := s.cache.get(cacheKey); ok {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cached)
		return
	}
	res, err := s.getJSON(r.Context(), s.cfg.AnalysisURL+"/quote/"+ticker)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	buf, _ := json.Marshal(res)
	s.cache.set(cacheKey, buf, s.cfg.QuoteTTL)
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf)
}

// handleConfluence proxies the analysis multi-timeframe confluence endpoint. Cached ~2 min (it runs
// the full pipeline on 3 timeframes). An optional ?timeframes= override is passed through.
func (s *Server) handleConfluence(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	tfs := r.URL.Query().Get("timeframes")
	if tfs == "" {
		tfs = confluenceTimeframes
	}
	cacheKey := "confluence:" + ticker + ":" + tfs
	if cached, ok := s.cache.get(cacheKey); ok {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cached)
		return
	}
	res, err := s.getJSON(r.Context(), s.cfg.AnalysisURL+"/analysis/"+ticker+"/confluence?timeframes="+tfs)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	buf, _ := json.Marshal(res)
	s.cache.set(cacheKey, buf, s.cfg.ConfluenceTTL)
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf)
}

// handlePredict proxies the prediction service's backtest-gated directional signal. Cached ~2 min
// (loading a model + scoring is cheap but not free). The response is either a signal WITH its
// backtest track record, or {signal:null, reason:"insufficient validation"} — never a bare guess.
func (s *Server) handlePredict(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	q := r.URL.RawQuery
	cacheKey := "predict:" + ticker + ":" + q
	if cached, ok := s.cache.get(cacheKey); ok {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cached)
		return
	}
	url := s.cfg.PredictionURL + "/predict/" + ticker
	if q != "" {
		url += "?" + q
	}
	res, err := s.getJSON(r.Context(), url)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	buf, _ := json.Marshal(res)
	s.cache.set(cacheKey, buf, s.cfg.PredictTTL)
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf)
}

// handleTrain creates one immutable candidate. It never replaces the active model: promotion is a
// separate, audited admin action. Training shares the evaluator-admin boundary because it is heavy
// and because an anonymous caller must never be able to fill the candidate registry.
func (s *Server) handleTrain(w http.ResponseWriter, r *http.Request) {
	uid := s.userIDFrom(r)
	if uid == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	if !s.isEvalAdmin(uid) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "evaluation admin access required — this deployment's EVAL_ADMIN_UIDS " +
				"allow-list does not include your user id",
		})
		return
	}
	ticker := strings.ToUpper(r.PathValue("ticker"))
	s.proxyPrediction(w, r, http.MethodPost, withQuery("/train/"+ticker, r), 125*time.Second)
}

// handleDashboard is the composite endpoint. It fans out concurrently:
//   - analysis service:  GET /analysis/{ticker}   (price + indicators + regime)
//   - llm service:       POST /read               (needs the regime, so it runs after analysis)
//   - seed data:         fundamentals + catalysts + news (local, instant)
//
// If the LLM is down, the offline stub still returns a read; if analysis is down, we surface a
// clear error but still return seed fundamentals/catalysts so the UI degrades gracefully.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	timeframe := normalizeTimeframe(r.URL.Query().Get("timeframe"))
	cacheKey := "dashboard:" + ticker + ":" + timeframe // cache is per ticker AND timeframe
	if cached, ok := s.cache.get(cacheKey); ok {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cached)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 130*time.Second)
	defer cancel()

	dash := map[string]any{
		"ticker":    ticker,
		"timeframe": timeframe,
		"asOf":      time.Now().UTC().Format(time.RFC3339),
	}
	var mu sync.Mutex
	var wg sync.WaitGroup

	// --- analysis (price + indicators + regime) ---
	var regime map[string]any
	var recentBars []any
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+ticker+"?timeframe="+timeframe)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			dash["price"] = map[string]any{"error": err.Error()}
			dash["indicators"] = nil
			dash["regime"] = nil
			return
		}
		dash["price"] = res["price"]
		dash["indicators"] = res["indicators"]
		dash["regime"] = res["regime"]
		dash["expectedClose"] = res["expectedClose"] // deterministic volatility band (not a prediction)
		dash["priceSource"] = res["source"]
		dash["priceSourceIsSynthetic"] = res["sourceIsSynthetic"]
		if reg, ok := res["regime"].(map[string]any); ok {
			regime = reg
		}
		if p, ok := res["price"].(map[string]any); ok {
			if bars, ok := p["ohlcv"].([]any); ok && len(bars) > 10 {
				recentBars = bars[len(bars)-10:]
			}
		}
	}()

	// --- seed-backed fields (instant, local) — NVDA-only; other tickers rely on live sources ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		mu.Lock()
		defer mu.Unlock()
		dash["hasSeed"] = isSeedTicker(ticker)
		if !isSeedTicker(ticker) {
			// No rich seed for GOOGL/TSLA/etc. Leave these null so the UI can note it.
			dash["fundamentals"] = nil
			dash["catalysts"] = nil
			dash["accountingTraps"] = nil
			return
		}
		dash["fundamentals"] = seedData["fundamentals"]
		dash["catalysts"] = map[string]any{
			"roadmap":   seedData["roadmap"],
			"suppliers": seedData["suppliers"],
			"events":    seedData["catalysts"],
			"risks":     seedData["risks"],
			"monitor":   seedData["monitor"],
		}
		dash["accountingTraps"] = seedData["accountingTraps"]
		dash["fiscalCalendarNote"] = seedData["fiscalCalendarNote"]
	}()

	// --- live (or seed-fallback) news: Marketaux ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		items, source := s.newsFor(ctx, ticker)
		mu.Lock()
		defer mu.Unlock()
		dash["news"] = items
		dash["newsSource"] = source
	}()

	// --- live (or seed-fallback) EPS beat/miss: Alpha Vantage ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		items, source := s.earningsFor(ctx, ticker)
		mu.Lock()
		defer mu.Unlock()
		dash["earnings"] = items
		dash["earningsSource"] = source
	}()

	// --- multi-timeframe confluence (deterministic; additive). Best-effort: if it fails the rest
	// of the dashboard still returns. Feeds the llm read so it reasons over cross-timeframe context.
	var confluence any
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+ticker+"/confluence?timeframes="+confluenceTimeframes)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			log.Printf("confluence fetch failed for %s: %v", ticker, err)
			return
		}
		dash["confluence"] = res["confluence"]
		confluence = res["confluence"]
	}()

	// --- directional signal (prediction service; backtest-gated). Best-effort and additive: if
	// prediction is down the dashboard still returns. This is a SUGGESTION with its track record,
	// never an order — execution stays with the user. horizon defaults to 5 bars.
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := s.getJSON(ctx, s.cfg.PredictionURL+"/predict/"+ticker+"?timeframe="+timeframe+"&horizon=5")
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			log.Printf("prediction fetch failed for %s: %v", ticker, err)
			return
		}
		dash["signal"] = res
	}()

	// --- next earnings date (Alpha Vantage EARNINGS_CALENDAR; seed fallback). Additive + best-effort:
	// a failure / rate-limit just omits it and the dashboard still returns. Descriptive date, never a
	// call. Cached hard (CalendarTTL) — the AV free tier is ~25/day. ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		date, source := s.nextEarningsFor(ctx, ticker)
		mu.Lock()
		defer mu.Unlock()
		if date != "" {
			dash["nextEarnings"] = map[string]any{"date": date, "source": source}
		}
	}()

	// --- crowd sentiment (StockTwits; keyless, additive, fail-silent). DESCRIPTIVE crowd color,
	// NOT a signal — it never feeds the prediction signal or the llm read. On any error the object
	// is { available:false } and the rest of the dashboard is unaffected. Cached at SentimentTTL. ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		sent := s.sentimentFor(ctx, ticker)
		mu.Lock()
		defer mu.Unlock()
		dash["sentiment"] = sent
	}()

	wg.Wait()

	// --- llm read (depends on regime from analysis) ---
	if regime != nil {
		read, err := s.postJSON(ctx, s.cfg.LLMURL+"/read", map[string]any{
			"ticker":     ticker,
			"regime":     regime,
			"recentBars": recentBars,
			"timeframe":  timeframe,
			"confluence": confluence, // nil if the confluence fetch failed; llm treats it as absent
		})
		if err != nil {
			dash["read"] = map[string]any{"error": err.Error()}
		} else {
			dash["read"] = read
			// Daily-snapshot diff only makes sense for 1D (intraday reads aren't persisted).
			if timeframe == "1D" {
				if d, derr := s.getJSON(ctx, s.cfg.LLMURL+"/reads/"+ticker+"/diff"); derr == nil {
					dash["readDiff"] = d["diff"]
				}
			}
		}
	} else {
		dash["read"] = map[string]any{"error": "no regime available (analysis service down)"}
	}

	buf, _ := json.Marshal(dash)
	s.cache.set(cacheKey, buf, s.cfg.CacheTTL)
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf)
}

func (s *Server) handleFundamentals(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	if !isSeedTicker(ticker) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ticker": ticker, "hasSeed": false,
			"fundamentals": nil, "accountingTraps": nil, "asOfNote": nil,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticker":          ticker,
		"hasSeed":         true,
		"fundamentals":    seedData["fundamentals"],
		"accountingTraps": seedData["accountingTraps"],
		"asOfNote":        seedData["asOfNote"],
	})
}

func (s *Server) handleCatalysts(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	if !isSeedTicker(ticker) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ticker": ticker, "hasSeed": false,
			"roadmap": nil, "suppliers": nil, "events": nil, "risks": nil, "monitor": nil,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticker":    ticker,
		"hasSeed":   true,
		"roadmap":   seedData["roadmap"],
		"suppliers": seedData["suppliers"],
		"events":    seedData["catalysts"],
		"risks":     seedData["risks"],
		"monitor":   seedData["monitor"],
	})
}

func (s *Server) handleNews(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	items, source := s.newsFor(r.Context(), ticker)
	writeJSON(w, http.StatusOK, map[string]any{
		"ticker": ticker, "news": items, "source": source,
	})
}

func (s *Server) handleEarnings(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	items, source := s.earningsFor(r.Context(), ticker)
	writeJSON(w, http.StatusOK, map[string]any{
		"ticker": ticker, "earnings": items, "source": source,
	})
}

// handleNextEarnings is a lightweight per-ticker next-earnings lookup (date + source) — used by the
// alerts service's calendar_event rule without pulling the whole dashboard. Additive + fail-soft.
func (s *Server) handleNextEarnings(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	date, source := s.nextEarningsFor(r.Context(), ticker)
	writeJSON(w, http.StatusOK, map[string]any{"ticker": ticker, "date": date, "source": source})
}

// handleCalendar returns the store-backed Catalyst Calendar for a window (default today..+30d).
// It never invokes a provider or a seeded schedule on a read. Descriptive facts only.
func (s *Server) handleCalendar(w http.ResponseWriter, r *http.Request) {
	from, to := normalizeCalWindow(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	calendar := s.calendarFor(r.Context(), from, to)
	writeJSON(w, http.StatusOK, map[string]any{
		"from": from, "to": to, "events": calendar.Events, "source": calendar.Source,
		"asOf": calendar.AsOf, "degraded": calendar.Degraded,
		// Phase 2C. Which companies have an OFFICIAL investor-relations source, and which do not.
		// Additive, store/config-backed, and never a provider call — see irCoverageFor.
		"irCoverage": s.irCoverageFor(r.Context()),
	})
}

// handleSentiment proxies the cached StockTwits crowd-sentiment summary. Additive + fail-silent:
// returns { available:false } when the source is disabled/unreachable. Descriptive, not a signal.
func (s *Server) handleSentiment(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	sent := s.sentimentFor(r.Context(), ticker)
	sent["ticker"] = ticker
	writeJSON(w, http.StatusOK, sent)
}

// handleBrief returns the AI Market Brief for one ticker: the gateway assembles the day's collected
// facts and the llm service reads+compresses them into a structured, plain-language digest. Additive
// + fail-silent + cached at BriefTTL. Descriptive synthesis, never a buy/sell verdict.
func (s *Server) handleBrief(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	ctx, cancel := context.WithTimeout(r.Context(), 130*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.briefFor(ctx, ticker, s.identityFrom(r)))
}

// handleMarketBrief returns the market-wide "what's moving today" brief, aggregated across the
// configured universe (+ optional SPY/QQQ).
func (s *Server) handleMarketBrief(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 130*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.briefFor(ctx, "MARKET", s.identityFrom(r)))
}

// handleAssistant returns the AI situational note for a (ticker, timeframe): the gateway assembles
// the live context (incl. the COMPUTED expected-close range) and the llm reasons over it. Cached at
// AssistantTTL, additive + fail-silent. Grounded, non-oracular, "your decision, not advice".
func (s *Server) handleAssistant(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	timeframe := normalizeTimeframe(r.URL.Query().Get("timeframe"))
	ctx, cancel := context.WithTimeout(r.Context(), 130*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.assistantFor(ctx, ticker, timeframe))
}

// handleChat proxies a grounded chat turn to the llm. NOT cached — the running message history is
// passed through. On llm failure it returns an "assistant offline" reply (fail-silent).
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	timeframe := normalizeTimeframe(r.URL.Query().Get("timeframe"))
	var body chatBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid chat body: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 130*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.chatReply(ctx, ticker, timeframe, body.PersonaID, body.Messages, s.userIDFrom(r)))
}

// handleReads / handleReadsDiff proxy the llm service's read history + diff to the frontend.
func (s *Server) handleReads(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	res, err := s.getJSON(r.Context(), s.cfg.LLMURL+"/reads/"+ticker)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleReadsDiff(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	res, err := s.getJSON(r.Context(), s.cfg.LLMURL+"/reads/"+ticker+"/diff")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
