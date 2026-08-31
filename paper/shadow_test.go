package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func shadowFixtureObservation(direction string) ShadowObservation {
	return ShadowObservation{
		ContractVersion: shadowContractVersion,
		Config:          "NVDA:1D:5", Ticker: "NVDA", Timeframe: "1D", ModelHorizon: 5,
		SignalBar: "2026-08-01", SignalBarUnix: 100,
		ObservedAt: "2026-08-01T21:00:00Z", Direction: direction, Target: "long",
		ProbUp: 0.62, Confidence: 0.31, ModelVersion: "m1", StrategyVersion: "s1",
		Evaluation: "NO EDGE", EvaluationCurrent: true, BacktestPassed: true, CostBps: 6,
		EntryPrice: 100, EntrySource: "tiingo", EntryAsOf: "2026-08-01T21:00:00Z",
		EntryEligible: true, EntryReason: "real quote",
	}
}

func shadowFixtureBars(n int) []ShadowBar {
	bars := make([]ShadowBar, 0, n)
	for i := 1; i <= n; i++ {
		bars = append(bars, ShadowBar{
			Ticker: "NVDA", Timeframe: "1D", Bar: time.Date(2026, 8, 1+i, 0, 0, 0, 0, time.UTC).Format("2006-01-02"),
			BarUnix: int64(100 + i), Close: 100 + float64(i), Source: "tiingo",
		})
	}
	return bars
}

func TestShadowSettlesFixedHorizonsWithRoundTripCosts(t *testing.T) {
	data := shadowDataset{
		Observations: []ShadowObservation{shadowFixtureObservation("Buy")},
		Bars:         shadowFixtureBars(10), Outcomes: []ShadowOutcome{},
	}
	outcomes := settleShadowOutcomes(data, time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC))
	if len(outcomes) != 4 {
		t.Fatalf("outcomes = %d, want H1/H3/H5/H10", len(outcomes))
	}
	if outcomes[0].Horizon != 1 || outcomes[0].Correct == nil || !*outcomes[0].Correct {
		t.Fatalf("H1 outcome = %+v", outcomes[0])
	}
	want := 0.01 - 2*6.0/10_000
	if math.Abs(outcomes[0].StrategyReturn-want) > 1e-12 || math.Abs(outcomes[0].EpisodePnl-shadowEpisodeNotional*want) > 1e-8 {
		t.Fatalf("H1 cost/P&L = return %.8f pnl %.4f, want %.8f / %.4f",
			outcomes[0].StrategyReturn, outcomes[0].EpisodePnl, want, shadowEpisodeNotional*want)
	}

	// Once persisted, an outcome is immutable: another settlement pass creates nothing.
	data.Outcomes = outcomes
	if again := settleShadowOutcomes(data, time.Now()); len(again) != 0 {
		t.Fatalf("settlement rewrote existing outcomes: %+v", again)
	}
}

func TestShadowHoldIsFlatAndNeverGivenASubjectiveCorrectnessLabel(t *testing.T) {
	obs := shadowFixtureObservation("Hold")
	obs.Target = "flat"
	outcomes := settleShadowOutcomes(shadowDataset{
		Observations: []ShadowObservation{obs}, Bars: shadowFixtureBars(1),
	}, time.Now())
	if len(outcomes) != 1 || outcomes[0].Correct != nil || outcomes[0].StrategyReturn != 0 || outcomes[0].EpisodePnl != 0 {
		t.Fatalf("Hold was turned into a tuned directional claim: %+v", outcomes)
	}
}

func TestFileShadowStoreIsIdempotentAndSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(dir, []PaperCfg{testCfg})
	if err != nil {
		t.Fatal(err)
	}
	obs := shadowFixtureObservation("Buy")
	if err := store.SaveShadowObservation(obs); err != nil {
		t.Fatal(err)
	}
	replacement := obs
	replacement.EntryPrice = 999
	replacement.ObservedAt = "2026-08-01T22:00:00Z"
	if err := store.SaveShadowObservation(replacement); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveShadowBars(shadowFixtureBars(10), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveShadowBars(shadowFixtureBars(10), time.Now()); err != nil {
		t.Fatal(err)
	}

	reopened, err := openStore(dir, []PaperCfg{testCfg})
	if err != nil {
		t.Fatal(err)
	}
	report, err := reopened.ShadowReport(10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Observations != 1 || report.SettledOutcomes != 4 || report.EligibleOutcomes != 4 || len(report.Rows) != 4 {
		t.Fatalf("shadow evidence was duplicated or lost: %+v", report)
	}
	if report.Recent[0].Observation.EntryPrice != obs.EntryPrice || report.Recent[0].Observation.ObservedAt != obs.ObservedAt {
		t.Fatalf("a retry replaced the first observation: %+v", report.Recent[0].Observation)
	}
}

func TestShadowKeepsSyntheticOutcomeVisibleButOutOfMetrics(t *testing.T) {
	bars := shadowFixtureBars(1)
	bars[0].Synthetic = true
	outcomes := settleShadowOutcomes(shadowDataset{
		Observations: []ShadowObservation{shadowFixtureObservation("Buy")}, Bars: bars,
	}, time.Now())
	report := buildShadowReport(shadowDataset{
		Observations: []ShadowObservation{shadowFixtureObservation("Buy")}, Bars: bars, Outcomes: outcomes,
	}, 10, nil)
	if report.SettledOutcomes != 1 || report.EligibleOutcomes != 0 || len(report.Rows) != 0 {
		t.Fatalf("synthetic outcome entered aggregate metrics: %+v", report)
	}
	if len(report.Recent) != 1 || len(report.Recent[0].Outcomes) != 1 || report.Recent[0].Outcomes[0].Eligible {
		t.Fatalf("synthetic exclusion was hidden instead of retained: %+v", report.Recent)
	}
}

func TestNoEdgeStillRecordsShadowWithoutOpeningOfficialPaper(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	f.set(func(f *fakes) {
		f.pred = cleanPredict(now, "Buy")
		f.pred["evaluation"].(map[string]any)["verdict"] = "NO EDGE"
	})
	e, store := harness(t, f, t.TempDir())
	e.evalConfig(context.Background(), testCfg, now)

	if calls := f.journalCalls(); len(calls) != 0 {
		t.Fatalf("NO EDGE opened official paper: %v", calls)
	}
	if d := lastDecision(t, store); d.Gate != "evaluator-verdict" {
		t.Fatalf("official gate was relaxed: %+v", d)
	}
	report, err := store.ShadowReport(10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Observations != 1 || report.Executable != 1 || len(report.Recent) != 1 {
		t.Fatalf("refused call was not shadowed: %+v", report)
	}
	got := report.Recent[0].Observation
	if got.Direction != "Buy" || got.Evaluation != "NO EDGE" || got.EntryPrice != 100 {
		t.Fatalf("shadow observation lost signal evidence: %+v", got)
	}
}

func TestShadowEndpointIsReadOnlyEvidence(t *testing.T) {
	store, err := openStore(t.TempDir(), []PaperCfg{testCfg})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveShadowObservation(shadowFixtureObservation("Buy")); err != nil {
		t.Fatal(err)
	}
	api := newAPI(Config{}, store, &Clients{}, nil)
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/shadow", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body shadowReport
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ContractVersion != shadowContractVersion || body.Observations != 1 {
		t.Fatalf("shadow payload = %+v", body)
	}
}
