package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	dashboardDefaultPoints = 252
	dashboardMaxPoints     = 1000
	dashboardDecisionTail  = 100
)

type experimentGenerationSummary struct {
	Generation        int64    `json:"generation"`
	Current           bool     `json:"current"`
	ResetAt           string   `json:"resetAt,omitempty"`
	OfficialStartedAt string   `json:"officialStartedAt,omitempty"`
	OfficialConfigs   []string `json:"officialConfigs"`
	NSnapshots        int      `json:"nSnapshots"`
	NGapDates         int      `json:"nGapDates"`
	NFills            int      `json:"nFills"`
	NDecisions        int      `json:"nDecisions"`
}

func (a *API) handleExperiments(w http.ResponseWriter, r *http.Request) {
	ledger := a.ledger()
	if ledger == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "experiment generations are unavailable because the paper ledger is not running",
		})
		return
	}
	payload := ledger.Payload(0)
	metrics, _ := payload["metrics"].(ledgerMetrics)
	gapDates, _ := payload["gapDates"].([]string)
	startedAt, _ := payload["officialStartedAt"].(string)
	configs, _ := payload["officialConfigs"].([]string)
	if configs == nil {
		configs = []string{}
	}
	events, _ := a.store.DecisionEvents(ledger.Generation(), 0)
	current := experimentGenerationSummary{
		Generation: ledger.Generation(), Current: true, OfficialStartedAt: startedAt,
		OfficialConfigs: configs, NSnapshots: metrics.NSnapshots, NGapDates: len(gapDates),
		NFills: intFromAny(payload["lastFillSeq"]), NDecisions: len(events),
	}
	archived := []experimentGenerationSummary{}
	storage := "files"
	note := "file fallback retains timestamped fill/snapshot archives; generation metadata is current-only"
	if a.store.db != nil {
		storage = "postgresql"
		note = "archived generations remain read-only evidence and are never mixed with the current dashboard"
		var err error
		archived, err = a.store.db.archivedExperiments(r.Context())
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"paper": true, "simulation": true, "current": current, "archived": archived,
		"storage": storage, "asOf": a.currentTime().Format(time.RFC3339), "note": note,
	})
}

func intFromAny(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func (a *API) handleDashboard(w http.ResponseWriter, r *http.Request) {
	points := dashboardDefaultPoints
	if raw := r.URL.Query().Get("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > dashboardMaxPoints {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("days must be an integer from 1 to %d", dashboardMaxPoints),
			})
			return
		}
		points = parsed
	}

	// A reset changes the experiment generation. Compose once more if one crossed the read; never
	// serve a page whose equity came from one generation and decisions from another.
	for attempt := 0; attempt < 2; attempt++ {
		asOf := a.currentTime()
		payload, startToken, endToken := a.dashboardPayload(r.Context(), asOf, points)
		if startToken == endToken {
			writeJSON(w, http.StatusOK, payload)
			return
		}
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": "the paper experiment changed generation or configured scope while the dashboard was being read; retry",
	})
}

func (a *API) dashboardPayload(ctx context.Context, asOf time.Time, seriesLimit int) (map[string]any, string, string) {
	ledger := a.ledger()
	startGeneration := int64(0)
	if ledger != nil {
		startGeneration = ledger.Generation()
	}
	startToken := dashboardReadToken(startGeneration, a.store.Configs())

	status := a.statusPayload(ctx, asOf)
	comparisonPayload := a.comparisonPayload(ctx, asOf)
	shadowPayload, shadowErr := a.store.ShadowReport(dashboardDecisionTail)
	if shadowErr != nil {
		shadowPayload.Recording = false
	}
	ledgerPayload := map[string]any{"available": false}
	series := []dashboardSeriesPoint{}
	seriesErr := error(nil)
	if ledger != nil {
		ledgerPayload = ledger.Payload(0)
		series, seriesErr = ledger.DashboardSeries(seriesLimit)
		ledgerPayload["series"] = series
	}

	events, historyErr := a.store.DecisionEvents(startGeneration, 0)
	actions := map[string]int{}
	gates := map[string]int{}
	for _, event := range events {
		actions[event.Decision.Action]++
		if event.Decision.Gate != "" {
			gates[event.Decision.Gate]++
		}
	}
	recent := events
	if len(recent) > dashboardDecisionTail {
		recent = recent[:dashboardDecisionTail]
	}

	metrics, _ := ledgerPayload["metrics"].(ledgerMetrics)
	gapDates, _ := ledgerPayload["gapDates"].([]string)
	startedAt, _ := ledgerPayload["officialStartedAt"].(string)
	clock := "not-started"
	if startedAt != "" {
		clock = "running"
	}
	sample := "empty"
	if startedAt != "" && metrics.NSnapshots > 0 {
		sample = "collecting"
		if metrics.NSnapshots >= minSnapshotsForSharpe {
			sample = "measurable"
		}
	}

	comparisons, _ := comparisonPayload["comparisons"].([]comparison)
	result := "unjudged"
	tracking := false
	divergence := false
	closedEpisodes := 0
	for _, row := range comparisons {
		closedEpisodes += row.Live.NClosed
		if row.Portfolio != nil && row.Portfolio.Comparable && row.Portfolio.Live != nil &&
			row.Portfolio.Reference != nil && row.Portfolio.Live.DailySharpe != nil &&
			row.Portfolio.Reference.Sharpe != nil {
			tracking = true
			if *row.Portfolio.Live.DailySharpe <= 0 && *row.Portfolio.Reference.Sharpe > 0 {
				divergence = true
			}
		}
	}
	if divergence {
		result = "divergence"
	} else if tracking {
		result = "tracking"
	}

	integrityReasons := []string{}
	if ledger == nil {
		integrityReasons = append(integrityReasons, "the evaluator-comparable ledger is unavailable")
	}
	if err := a.store.Check(ctx); err != nil {
		integrityReasons = append(integrityReasons, "engine storage is unavailable: "+err.Error())
	}
	if err := a.store.PersistenceError(); err != nil {
		integrityReasons = append(integrityReasons, "the latest engine-state commit failed: "+err.Error())
	}
	if seriesErr != nil {
		integrityReasons = append(integrityReasons, "snapshot history could not be read: "+seriesErr.Error())
	}
	if historyErr != nil {
		integrityReasons = append(integrityReasons, "decision history could not be read: "+historyErr.Error())
	}
	if len(gapDates) > 0 {
		integrityReasons = append(integrityReasons, fmt.Sprintf("%d date(s) have no real complete-book mark", len(gapDates)))
	}
	if reconciliation, ok := status["reconciliation"].(map[string]any); ok {
		if desynced, ok := reconciliation["desyncedConfigs"].([]string); ok && len(desynced) > 0 {
			integrityReasons = append(integrityReasons, fmt.Sprintf("%d config(s) are desynchronized", len(desynced)))
		}
		if pending, ok := reconciliation["pendingBookings"].(int); ok && pending > 0 {
			integrityReasons = append(integrityReasons, fmt.Sprintf("%d ledger booking(s) are pending", pending))
		}
	}
	if book, ok := status["book"].(map[string]any); ok {
		if recording, ok := book["recording"].(bool); ok && !recording {
			integrityReasons = append(integrityReasons, "journal recording is unavailable")
		}
	}
	integrity := "healthy"
	if len(integrityReasons) > 0 {
		integrity = "degraded"
	}

	var coverage *float64
	observedDates := metrics.NSnapshots + len(gapDates)
	if observedDates > 0 {
		coverage = float64ptr(round5(float64(metrics.NSnapshots) / float64(observedDates)))
	}

	payload := map[string]any{
		"paper": true, "simulation": true,
		"asOf":       asOf.UTC().Format(time.RFC3339),
		"contract":   "paper-dashboard-v2",
		"generation": startGeneration,
		"experiment": map[string]any{
			"officialStartedAt": startedAt,
			"officialConfigs":   ledgerPayload["officialConfigs"],
			"generation":        startGeneration,
		},
		"dimensions": map[string]any{
			"clock": clock, "integrity": integrity, "sample": sample, "result": result,
			"integrityReasons": integrityReasons,
		},
		"progress": map[string]any{
			"snapshotDays": metrics.NSnapshots, "sharpeRequired": minSnapshotsForSharpe,
			"dailyReturns": metrics.NReturns, "closedEpisodes": closedEpisodes,
			"countingStatsRequired": minMeaningful, "gapDates": len(gapDates),
			"observedDates": observedDates, "coverage": coverage,
			"settledDecisions": len(events),
		},
		"status":     status,
		"ledger":     ledgerPayload,
		"comparison": comparisonPayload,
		"shadow":     shadowPayload,
		"decisionHistory": map[string]any{
			"total": len(events), "actions": actions, "gates": gates, "recent": recent,
			"error": errorString(historyErr),
		},
	}

	endGeneration := int64(0)
	if ledger != nil {
		endGeneration = ledger.Generation()
	}
	return payload, startToken, dashboardReadToken(endGeneration, a.store.Configs())
}

func dashboardReadToken(generation int64, configs []PaperCfg) string {
	keys := make([]string, 0, len(configs))
	for _, config := range configs {
		keys = append(keys, config.Key())
	}
	return strconv.FormatInt(generation, 10) + ":" + strings.Join(keys, ",")
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
