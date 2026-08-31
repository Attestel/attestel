package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDashboardIsOneGenerationScopedMeasurementPayload(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	e.evalConfig(context.Background(), testCfg, now)

	api := newAPI(e.cfg, store, e.clients, e)
	api.now = func() time.Time { return now }
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/dashboard?days=252", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Contract   string `json:"contract"`
		Generation int64  `json:"generation"`
		Dimensions struct {
			Clock     string `json:"clock"`
			Integrity string `json:"integrity"`
			Sample    string `json:"sample"`
			Result    string `json:"result"`
		} `json:"dimensions"`
		Progress struct {
			SnapshotDays     int `json:"snapshotDays"`
			SettledDecisions int `json:"settledDecisions"`
		} `json:"progress"`
		Ledger struct {
			Series []dashboardSeriesPoint `json:"series"`
		} `json:"ledger"`
		DecisionHistory struct {
			Total  int             `json:"total"`
			Recent []DecisionEvent `json:"recent"`
		} `json:"decisionHistory"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Contract != "paper-dashboard-v2" || body.Generation != e.ledger.Generation() {
		t.Fatalf("dashboard identity = %+v", body)
	}
	if body.Dimensions.Clock != "not-started" || body.Dimensions.Sample != "empty" {
		t.Fatalf("setup-era evidence must not masquerade as an official sample: %+v", body.Dimensions)
	}
	if body.Progress.SnapshotDays != 1 || body.Progress.SettledDecisions != 1 {
		t.Fatalf("progress does not reconcile with the book and decision record: %+v", body.Progress)
	}
	if len(body.Ledger.Series) != 1 || body.Ledger.Series[0].Equity == nil {
		t.Fatalf("dated equity series missing: %+v", body.Ledger.Series)
	}
	if body.DecisionHistory.Total != 1 || len(body.DecisionHistory.Recent) != 1 {
		t.Fatalf("decision history missing: %+v", body.DecisionHistory)
	}
	decision := body.DecisionHistory.Recent[0].Decision
	if decision.ModelVersion != "model-v1" || decision.StrategyVersion == "" {
		t.Fatalf("dashboard lost active model lineage: %+v", decision)
	}
}

func TestDashboardRejectsAnUnboundedSeriesRequest(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	api := newAPI(e.cfg, store, e.clients, e)
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/dashboard?days=1001", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unbounded dashboard request status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestExperimentIndexKeepsTheCurrentGenerationExplicit(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	e.evalConfig(context.Background(), testCfg, now)
	api := newAPI(e.cfg, store, e.clients, e)
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/experiments", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Storage  string                        `json:"storage"`
		Current  experimentGenerationSummary   `json:"current"`
		Archived []experimentGenerationSummary `json:"archived"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Storage != "files" || !body.Current.Current || body.Current.Generation != e.ledger.Generation() {
		t.Fatalf("current generation is ambiguous: %+v", body)
	}
	if body.Current.NDecisions != 1 || body.Current.NSnapshots != 1 {
		t.Fatalf("current generation counts do not reconcile: %+v", body.Current)
	}
	if len(body.Archived) != 0 {
		t.Fatalf("fresh fallback book should have no indexed archives: %+v", body.Archived)
	}
}

func TestDashboardSeriesPreservesGapAsMissingEvidence(t *testing.T) {
	ledger, err := openLedger(t.TempDir(), defaultStartingCash)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	key := testCfg.Key()
	if err := ledger.Mark([]string{key}, key, "2026-08-18", 100, true); err != nil {
		t.Fatalf("real mark: %v", err)
	}
	if err := ledger.Mark([]string{key}, key, "2026-08-19", 0, false); err != nil {
		t.Fatalf("gap mark: %v", err)
	}
	series, err := ledger.DashboardSeries(252)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(series) != 2 || !series[1].Gap {
		t.Fatalf("expected a measured point followed by a gap, got %+v", series)
	}
	if series[1].Equity != nil || series[1].DailyReturn != nil || series[1].Drawdown != nil {
		t.Fatalf("a gap must carry null measurements, never zero or a carried mark: %+v", series[1])
	}
}
