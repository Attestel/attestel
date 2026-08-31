package main

import (
	"context"
	"encoding/json"
	"time"
)

// assistant.go — the AI assistant beside the Directional Signal.
//
// It assembles the ticker's LIVE context at the selected timeframe (price + regime + the COMPUTED
// expected-close range + confluence + the backtest-gated signal + top news + crowd sentiment) and
// proxies two llm endpoints:
//   - /assistant : a short grounded situational note (cached ~AssistantTTL).
//   - /chat      : a grounded free-text reply to a follow-up (NEVER cached — messages pass through).
//
// The expected-close RANGE is computed by the analysis service, never the LLM. Additive + fail-silent:
// missing inputs are omitted; if the llm service is unreachable the gateway returns a deterministic
// note / an "assistant offline" chat reply, and the computed range still renders on the client.

const assistantDisclaimer = "Informational, not financial advice — your decision and your risk."

// assistantContextFor gathers the compact, timeframe-aware live context. Best-effort throughout.
func (s *Server) assistantContextFor(ctx context.Context, ticker, timeframe string) map[string]any {
	c := map[string]any{"ticker": ticker, "timeframe": timeframe, "asOf": time.Now().UTC().Format(time.RFC3339)}

	// analysis at the selected timeframe: price + regime + the computed expected-close range.
	if res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+ticker+"?timeframe="+timeframe); err == nil {
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
			c["regime"] = trimRegimeForAssistant(reg)
		}
		if ec, ok := res["expectedClose"]; ok && ec != nil {
			c["expectedClose"] = ec // COMPUTED band — the assistant only explains it
		}
	}

	// multi-timeframe confluence note.
	if res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+ticker+"/confluence?timeframes="+confluenceTimeframes); err == nil {
		if conf, ok := res["confluence"].(map[string]any); ok {
			c["confluence"] = map[string]any{
				"trendAlignment": conf["trendAlignment"], "conflicts": conf["conflicts"], "note": conf["note"],
			}
		}
	}

	// backtest-gated directional signal (a suggestion WITH its track record, or null). Kept compact.
	if res, err := s.getJSON(ctx, s.cfg.PredictionURL+"/predict/"+ticker+"?timeframe="+timeframe+"&horizon=5"); err == nil {
		sig := map[string]any{}
		for _, k := range []string{"signal", "backtest", "reason", "horizon"} {
			if v, ok := res[k]; ok {
				sig[k] = v
			}
		}
		c["signal"] = sig
	}

	// top news headlines (3) + crowd sentiment (if available).
	if items, _ := s.newsFor(ctx, ticker); len(items) > 0 {
		var news []map[string]any
		for _, it := range items {
			if len(news) >= 3 {
				break
			}
			news = append(news, map[string]any{"headline": it["headline"], "source": it["source"], "sentiment": it["sentiment"]})
		}
		c["news"] = news
	}
	if sent := s.sentimentFor(ctx, ticker); sent != nil {
		if avail, _ := sent["available"].(bool); avail {
			c["sentiment"] = map[string]any{"label": sent["sentimentLabel"], "buzz": sent["buzz"], "spike": sent["spike"]}
		}
	}
	return c
}

// trimRegimeForAssistant keeps the descriptive regime fields PLUS the computed key levels, so the
// assistant can quote support/resistance without inventing them.
func trimRegimeForAssistant(reg map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"trend", "shortTermTrend", "momentum", "macdState", "volatility", "volume",
		"trendStrength", "priceVsBands", "stochastic", "vwapState", "keyLevels"} {
		if v, ok := reg[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

// assistantFor returns the cached situational note for a (ticker, timeframe).
func (s *Server) assistantFor(ctx context.Context, ticker, timeframe string) map[string]any {
	key := "assistant:" + ticker + ":" + timeframe
	if b, ok := s.cache.get(key); ok {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			return m
		}
	}
	contextObj := s.assistantContextFor(ctx, ticker, timeframe)
	out := s.requestAssistant(ctx, ticker, timeframe, contextObj)
	out["ticker"] = ticker
	out["timeframe"] = timeframe
	out["contextUsed"] = contextSections(contextObj)
	if data, err := json.Marshal(out); err == nil {
		s.cache.set(key, data, s.cfg.AssistantTTL)
	}
	return out
}

// requestAssistant POSTs the context to llm /assistant; falls back to a gateway-built stub if the llm
// SERVICE is unreachable (the llm service itself already degrades to its own stub when the model is down).
func (s *Server) requestAssistant(ctx context.Context, ticker, timeframe string, contextObj map[string]any) map[string]any {
	res, err := s.postJSON(ctx, s.cfg.LLMURL+"/assistant", map[string]any{
		"ticker": ticker, "timeframe": timeframe, "context": contextObj,
	})
	if err != nil {
		return gatewayStubAssistant(ticker, timeframe, contextObj)
	}
	return res
}

// chatReply proxies llm /chat with the running message history (NEVER cached). On llm failure it
// returns an "assistant offline" reply so the panel degrades gracefully. `personaId` selects an
// optional "instructor" persona (style only — the llm keeps its grounding rules regardless);
// `userID` is the gateway-verified identity so a signed-in user's CUSTOM persona resolves (a guest
// resolves built-ins only). Chat REPLIES stay open to guests — chat is grounded market analysis.
func (s *Server) chatReply(ctx context.Context, ticker, timeframe, personaID string, messages []any, userID string) map[string]any {
	contextObj := s.assistantContextFor(ctx, ticker, timeframe)
	res, err := s.postJSONUser(ctx, s.cfg.LLMURL+"/chat", map[string]any{
		"ticker": ticker, "timeframe": timeframe, "context": contextObj, "messages": messages,
		"personaId": personaID,
	}, userID)
	if err != nil {
		return map[string]any{
			"ticker": ticker, "timeframe": timeframe,
			"reply": "The assistant is offline right now, so I can't chat — but the computed context and " +
				"the volatility range above still render. " + assistantDisclaimer,
			"modelUsed": "stub:gateway-offline", "disclaimer": assistantDisclaimer,
		}
	}
	return res
}

// gatewayStubAssistant is the fallback-of-last-resort when the llm SERVICE is down: a deterministic
// note from the same live context. Grounded, non-oracular, never a command.
func gatewayStubAssistant(ticker, timeframe string, c map[string]any) map[string]any {
	watch := []any{}
	change := []any{}
	regime, _ := c["regime"].(map[string]any)
	price, _ := c["price"].(map[string]any)
	ec, _ := c["expectedClose"].(map[string]any)

	trend := "n/a"
	if regime != nil {
		trend = asString(regime["trend"])
	}
	situation := ticker + " on " + timeframe + ": trend reads " + trend
	if price != nil && price["last"] != nil {
		situation += ", last around " + asString(price["last"])
	}
	situation += ". Context, not a call — the setup could shift."

	if regime != nil {
		if kl, ok := regime["keyLevels"].(map[string]any); ok {
			if res, ok := kl["resistance"].([]any); ok && len(res) > 0 {
				watch = append(watch, "Nearest resistance "+asString(res[0])+" — a break could open room above.")
				change = append(change, "A decisive close above "+asString(res[0])+" would likely improve the picture.")
			}
			if sup, ok := kl["support"].([]any); ok && len(sup) > 0 {
				watch = append(watch, "Nearest support "+asString(sup[0])+" — losing it could invite downside.")
				change = append(change, "A close below "+asString(sup[0])+" would likely weaken it.")
			}
		}
	}
	rangeComment := "No expected-close range is available right now."
	if ec != nil && ec["low"] != nil && ec["high"] != nil {
		rangeComment = "The expected-close range is " + asString(ec["low"]) + "–" + asString(ec["high"]) +
			" — a statistical band from volatility, not a prediction; price can gap through it, and it narrows into the close."
		watch = append(watch, "The computed expected-close band "+asString(ec["low"])+"–"+asString(ec["high"])+".")
	}
	change = append(change, "A trend flip on the "+timeframe+" timeframe would change the read.")
	if len(watch) == 0 {
		watch = append(watch, "Follow-through on the trend and momentum noted above.")
	}

	return map[string]any{
		"structured": map[string]any{
			"situation": situation, "whatToWatch": watch, "wouldChangeIt": change,
			"rangeComment": rangeComment, "_stub": true,
		},
		"modelUsed": "stub:gateway-offline", "warnings": []any{}, "retried": false,
		"disclaimer": assistantDisclaimer,
	}
}

// chatBody is the POST body for /api/chat: the running message history from the frontend, plus an
// optional persona id selecting the "instructor" style.
type chatBody struct {
	Messages  []any  `json:"messages"`
	PersonaID string `json:"personaId"`
}
