package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// investigation.go — one bounded research job, streamed as truthful progress events.
//
// The browser submits one question and a finite set of optional research methods. The gateway
// resolves every source itself, emits progress only when real work begins/completes, and hands the
// resulting source bundle to the LLM for one structured synthesis. This is intentionally separate
// from chat: no message history, no persistent composer, no model-driven tool loop.

const investigationDisclaimer = "AI-synthesized research over the collected sources — " +
	"informational, incomplete, and not investment advice. The report does not place or recommend a trade."

var investigationAddOns = map[string]string{
	"my_thesis":              "My thesis",
	"sec_filings":            "SEC filings",
	"earnings_history":       "Earnings & calls",
	"news_events":            "News & events",
	"technical_context":      "Technical context",
	"opposing_perspective":   "Opposing perspective",
	"management_credibility": "Management credibility",
	"scenario_analysis":      "Scenario analysis",
}

var defaultInvestigationAddOns = []string{
	"news_events", "technical_context", "opposing_perspective",
}

type investigationBody struct {
	Question string   `json:"question"`
	AddOns   []string `json:"addOns"`
}

type investigationTrailItem struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Status string `json:"status"` // used | unavailable | method
	Detail string `json:"detail"`
	Source string `json:"source,omitempty"`
}

func normalizedInvestigationAddOns(in []string) ([]string, bool) {
	if in == nil {
		return append([]string(nil), defaultInvestigationAddOns...), true
	}
	if len(in) > len(investigationAddOns) {
		return nil, false
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		key := strings.ToLower(strings.TrimSpace(raw))
		if _, ok := investigationAddOns[key]; !ok {
			return nil, false
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out, true
}

func hasInvestigationAddOn(addOns []string, want string) bool {
	for _, key := range addOns {
		if key == want {
			return true
		}
	}
	return false
}

func writeInvestigationEvent(w http.ResponseWriter, event map[string]any) error {
	if err := json.NewEncoder(w).Encode(event); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func (s *Server) handleInvestigation(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(strings.TrimSpace(r.PathValue("ticker")))
	var body investigationBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	body.Question = strings.TrimSpace(body.Question)
	if len(body.Question) < 8 || len(body.Question) > 2000 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "research question must be between 8 and 2000 characters",
		})
		return
	}
	addOns, ok := normalizedInvestigationAddOns(body.AddOns)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown research add-on"})
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	_ = writeInvestigationEvent(w, map[string]any{
		"type": "stage", "stage": "understanding",
		"message": "Question scoped to " + ticker + " and the selected research methods.",
	})
	_ = writeInvestigationEvent(w, map[string]any{
		"type": "stage", "stage": "gathering",
		"message": "Collecting the selected evidence and its provenance.",
	})

	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	sourceBundle, trail, dataState := s.investigationContextFor(ctx, ticker, addOns, s.identityFrom(r))
	if err := writeInvestigationEvent(w, map[string]any{
		"type": "sources", "trail": trail, "dataState": dataState,
	}); err != nil {
		return
	}
	if err := writeInvestigationEvent(w, map[string]any{
		"type": "stage", "stage": "synthesizing",
		"message": "Testing the apparent answer against contradictions, then synthesizing the report.",
	}); err != nil {
		return
	}

	res, err := s.postJSON(ctx, s.cfg.LLMURL+"/investigation", map[string]any{
		"ticker": ticker, "question": body.Question, "addons": addOns, "context": sourceBundle,
	})
	if err != nil {
		res = gatewayStubInvestigation(ticker, body.Question, sourceBundle)
	}
	res["addOns"] = addOns
	res["trail"] = trail
	res["dataState"] = dataState
	res["contextUsed"] = investigationContextKeys(sourceBundle)
	res["sourceManifest"] = investigationSourceManifest(sourceBundle)
	if res["disclaimer"] == nil {
		res["disclaimer"] = investigationDisclaimer
	}
	_ = writeInvestigationEvent(w, map[string]any{"type": "result", "result": res})
}

func (s *Server) investigationContextFor(
	ctx context.Context,
	ticker string,
	addOns []string,
	id identity,
) (map[string]any, []investigationTrailItem, string) {
	bundle := map[string]any{}
	trail := []investigationTrailItem{}
	dataState := "unavailable"

	// Market context is the common base for every investigation and also supplies the technical
	// add-on without making a duplicate analysis request.
	analysis, analysisErr := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+ticker+"?timeframe=1D")
	if analysisErr == nil {
		price := map[string]any{}
		if rawPrice, ok := analysis["price"].(map[string]any); ok {
			for _, key := range []string{"lastClose", "changePct"} {
				if rawPrice[key] != nil {
					price[key] = rawPrice[key]
				}
			}
		}
		market := map[string]any{
			"asOf":              time.Now().UTC().Format(time.RFC3339),
			"price":             price,
			"source":            analysis["source"],
			"sourceIsSynthetic": analysis["sourceIsSynthetic"],
		}
		bundle["market_context"] = market
		dataState = "live"
		if synthetic, _ := analysis["sourceIsSynthetic"].(bool); synthetic {
			dataState = "synthetic"
		}
		trail = append(trail, investigationTrailItem{
			Key: "market_context", Label: "Market context", Status: "used",
			Detail: "Daily price context collected.", Source: asString(analysis["source"]),
		})
	} else {
		trail = append(trail, investigationTrailItem{
			Key: "market_context", Label: "Market context", Status: "unavailable",
			Detail: "The analysis service did not return market context.",
		})
	}

	if hasInvestigationAddOn(addOns, "technical_context") {
		technical := map[string]any{}
		if analysisErr == nil {
			technical["regime"] = analysis["regime"]
		}
		if confluence, err := s.getJSON(
			ctx, s.cfg.AnalysisURL+"/analysis/"+ticker+"/confluence?timeframes="+confluenceTimeframes,
		); err == nil {
			technical["confluence"] = confluence["confluence"]
		}
		if technical["regime"] != nil || technical["confluence"] != nil {
			bundle["technical_context"] = technical
			trail = append(trail, investigationTrailItem{
				Key: "technical_context", Label: investigationAddOns["technical_context"], Status: "used",
				Detail: "Computed regime and multi-timeframe context collected.", Source: "computed",
			})
		} else {
			trail = append(trail, investigationTrailItem{
				Key: "technical_context", Label: investigationAddOns["technical_context"], Status: "unavailable",
				Detail: "No computed technical context was available.",
			})
		}
	}

	needNews := hasInvestigationAddOn(addOns, "news_events") || hasInvestigationAddOn(addOns, "sec_filings")
	if needNews {
		items, source := s.newsFor(ctx, ticker)
		var news []map[string]any
		var filings []map[string]any
		for _, item := range items {
			if category, _ := item["category"].(string); category == "filing" {
				if len(filings) < 8 {
					filings = append(filings, item)
				}
			} else if len(news) < 10 {
				news = append(news, item)
			}
		}
		if hasInvestigationAddOn(addOns, "news_events") {
			if len(news) > 0 {
				bundle["news_events"] = news
				trail = append(trail, investigationTrailItem{
					Key: "news_events", Label: investigationAddOns["news_events"], Status: "used",
					Detail: "Recent headlines collected with source labels.", Source: source,
				})
			} else {
				trail = append(trail, investigationTrailItem{
					Key: "news_events", Label: investigationAddOns["news_events"], Status: "unavailable",
					Detail: "No recent headlines were available.", Source: source,
				})
			}
		}
		if hasInvestigationAddOn(addOns, "sec_filings") {
			if len(filings) > 0 {
				bundle["sec_filings"] = filings
				trail = append(trail, investigationTrailItem{
					Key: "sec_filings", Label: investigationAddOns["sec_filings"], Status: "used",
					Detail: "Recent SEC filing records collected.", Source: "SEC EDGAR",
				})
			} else {
				trail = append(trail, investigationTrailItem{
					Key: "sec_filings", Label: investigationAddOns["sec_filings"], Status: "unavailable",
					Detail: "No filing records were present in the collected feed.", Source: "SEC EDGAR",
				})
			}
		}
	}

	if hasInvestigationAddOn(addOns, "earnings_history") {
		earnings, source := s.earningsFor(ctx, ticker)
		if len(earnings) > 0 {
			if len(earnings) > 8 {
				earnings = earnings[:8]
			}
			bundle["earnings_history"] = earnings
			trail = append(trail, investigationTrailItem{
				Key: "earnings_history", Label: investigationAddOns["earnings_history"], Status: "used",
				Detail: "Earnings history collected.", Source: source,
			})
		} else {
			trail = append(trail, investigationTrailItem{
				Key: "earnings_history", Label: investigationAddOns["earnings_history"], Status: "unavailable",
				Detail: "No earnings history was available.", Source: source,
			})
		}
	}

	needTranscripts := hasInvestigationAddOn(addOns, "earnings_history") ||
		hasInvestigationAddOn(addOns, "management_credibility")
	if needTranscripts {
		if history, err := s.getJSON(ctx, s.cfg.LLMURL+"/transcript/"+ticker+"?limit=4"); err == nil {
			if analyses, ok := history["analyses"].([]any); ok && len(analyses) > 0 {
				bundle["stored_transcripts"] = analyses
				trail = append(trail, investigationTrailItem{
					Key: "stored_transcripts", Label: "Stored call analyses", Status: "used",
					Detail: "Saved transcript analyses collected.", Source: "stored research",
				})
			} else if needTranscripts {
				trail = append(trail, investigationTrailItem{
					Key: "stored_transcripts", Label: "Stored call analyses", Status: "unavailable",
					Detail: "Analyze an earnings call first to assess management language.",
				})
			}
		} else if needTranscripts {
			trail = append(trail, investigationTrailItem{
				Key: "stored_transcripts", Label: "Stored call analyses", Status: "unavailable",
				Detail: "Stored call analyses could not be loaded.",
			})
		}
	}

	if hasInvestigationAddOn(addOns, "my_thesis") {
		theses := s.activeTheses(ctx, ticker, id.cookie)
		if len(theses) > 0 {
			if len(theses) > 3 {
				theses = theses[:3]
			}
			bundle["my_thesis"] = trimInvestigationTheses(theses)
			trail = append(trail, investigationTrailItem{
				Key: "my_thesis", Label: investigationAddOns["my_thesis"], Status: "used",
				Detail: "Active thesis loaded read-only; this run cannot rewrite it.", Source: "user",
			})
		} else {
			trail = append(trail, investigationTrailItem{
				Key: "my_thesis", Label: investigationAddOns["my_thesis"], Status: "unavailable",
				Detail: "No active thesis was available for this company.", Source: "user",
			})
		}
	}

	if hasInvestigationAddOn(addOns, "scenario_analysis") {
		if isSeedTicker(ticker) {
			bundle["scenario_context"] = map[string]any{
				"suppliers": seedData["suppliers"], "roadmap": seedData["roadmap"], "risks": seedData["risks"],
			}
			trail = append(trail, investigationTrailItem{
				Key: "scenario_context", Label: investigationAddOns["scenario_analysis"], Status: "used",
				Detail: "Verified roadmap, dependency, and risk context collected.", Source: "verified seed",
			})
		} else {
			trail = append(trail, investigationTrailItem{
				Key: "scenario_context", Label: investigationAddOns["scenario_analysis"], Status: "unavailable",
				Detail: "No verified scenario dataset is available for this company.",
			})
		}
	}

	if hasInvestigationAddOn(addOns, "opposing_perspective") {
		trail = append(trail, investigationTrailItem{
			Key: "opposing_perspective", Label: investigationAddOns["opposing_perspective"], Status: "method",
			Detail: "The synthesis was instructed to seek contradictions and missing evidence.",
		})
	}

	return bundle, trail, dataState
}

func investigationContextKeys(bundle map[string]any) []string {
	order := []string{
		"market_context", "technical_context", "news_events", "sec_filings",
		"earnings_history", "stored_transcripts", "my_thesis", "scenario_context",
	}
	out := []string{}
	for _, key := range order {
		if bundle[key] != nil {
			out = append(out, key)
		}
	}
	return out
}

func trimInvestigationTheses(theses []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(theses))
	for _, thesis := range theses {
		trimmed := map[string]any{}
		for _, key := range []string{
			"id", "ticker", "text", "claim", "horizon", "assumptions", "catalysts", "risks",
			"invalidation", "invalidationConditions", "confidence", "nextQuestions", "status",
		} {
			if thesis[key] != nil {
				trimmed[key] = thesis[key]
			}
		}
		out = append(out, trimmed)
	}
	return out
}

// investigationSourceManifest is the small, display-safe provenance index returned to the browser
// when the user expands the research trail. It exposes source labels and links, never the full
// server-to-model bundle or a private thesis body.
func investigationSourceManifest(bundle map[string]any) []map[string]any {
	manifest := []map[string]any{}
	for _, spec := range []struct {
		key   string
		label string
	}{
		{"news_events", "News & events"},
		{"sec_filings", "SEC filings"},
	} {
		raw, ok := bundle[spec.key].([]map[string]any)
		if !ok || len(raw) == 0 {
			continue
		}
		items := make([]map[string]any, 0, len(raw))
		for _, source := range raw {
			items = append(items, map[string]any{
				"title": source["headline"], "url": source["url"], "source": source["source"],
				"publishedAt": source["publishedAt"],
			})
		}
		manifest = append(manifest, map[string]any{"key": spec.key, "label": spec.label, "items": items})
	}
	if earnings, ok := bundle["earnings_history"].([]map[string]any); ok && len(earnings) > 0 {
		items := make([]map[string]any, 0, len(earnings))
		for _, earning := range earnings {
			title := "Earnings record"
			if label := asString(earning["label"]); label != "" {
				title = label
			} else if date := asString(earning["fiscalDateEnding"]); date != "" {
				title = "Quarter ending " + date
			}
			items = append(items, map[string]any{"title": title, "source": earning["source"]})
		}
		manifest = append(manifest, map[string]any{
			"key": "earnings_history", "label": "Earnings history", "items": items,
		})
	}
	if transcripts, ok := bundle["stored_transcripts"].([]any); ok && len(transcripts) > 0 {
		items := make([]map[string]any, 0, len(transcripts))
		for _, raw := range transcripts {
			analysis, _ := raw.(map[string]any)
			title := asString(analysis["label"])
			if title == "" {
				title = "Saved call analysis"
			}
			items = append(items, map[string]any{
				"title": title, "source": "stored research", "publishedAt": analysis["createdAt"],
			})
		}
		manifest = append(manifest, map[string]any{
			"key": "stored_transcripts", "label": "Stored call analyses", "items": items,
		})
	}
	return manifest
}

func gatewayStubInvestigation(ticker, question string, bundle map[string]any) map[string]any {
	return map[string]any{
		"ticker":   ticker,
		"question": question,
		"structured": map[string]any{
			"answer": "The research service is unreachable, so this question was not synthesized. " +
				"The source collection completed and remains visible in the research trail.",
			"keyFindings":     []any{},
			"counterEvidence": []any{},
			"uncertainty":     "No interpretive or contradiction analysis was performed while the research service was offline.",
			"thesisImpact": map[string]any{
				"status": "not_assessed", "reason": "The research service did not evaluate a thesis.",
			},
			"nextQuestion": question,
			"_stub":        true,
		},
		"modelUsed":      "stub:gateway-offline",
		"warnings":       []any{},
		"retried":        false,
		"disclaimer":     investigationDisclaimer,
		"contextUsed":    investigationContextKeys(bundle),
		"sourceManifest": investigationSourceManifest(bundle),
	}
}
