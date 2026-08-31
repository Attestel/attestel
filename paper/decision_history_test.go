package main

import (
	"testing"
	"time"
)

func TestDecisionHistoryIsGenerationScopedDurableAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := PaperCfg{Ticker: "NVDA", Timeframe: "1D", Horizon: 5}
	store, err := openStore(dir, []PaperCfg{cfg})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	state := store.StateFor(cfg)
	state.LastBarActedOn = "2026-08-25"
	state.LastBarUnix = 1787616000
	event := DecisionEvent{
		Generation: 2, Config: cfg.Key(), Ticker: cfg.Ticker, Timeframe: cfg.Timeframe, Horizon: cfg.Horizon,
		Decision: Decision{
			At: "2026-08-26T01:02:03Z", Bar: state.LastBarActedOn, BarUnix: state.LastBarUnix,
			From: "flat", Target: "long", Action: "open", Reason: "settled",
			ModelVersion: "model-v1", StrategyVersion: "strategy-v1",
		},
	}
	if err := store.SaveDecision(state, event); err != nil {
		t.Fatalf("save decision: %v", err)
	}
	// A retry after an ambiguous caller result must update state without duplicating evidence.
	if err := store.SaveDecision(state, event); err != nil {
		t.Fatalf("idempotent save: %v", err)
	}

	other := event
	other.Generation = 3
	other.Decision.Bar = "2026-08-26"
	other.Decision.BarUnix++
	if err := store.SaveDecision(state, other); err != nil {
		t.Fatalf("save next generation: %v", err)
	}

	reopened, err := openStore(dir, []PaperCfg{cfg})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	got, err := reopened.DecisionEvents(2, 0)
	if err != nil {
		t.Fatalf("read decisions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("generation 2 should contain one settled decision, got %+v", got)
	}
	if got[0].Decision.ModelVersion != "model-v1" || got[0].Decision.StrategyVersion != "strategy-v1" {
		t.Fatalf("model lineage was not retained: %+v", got[0].Decision)
	}
	if current := reopened.StateFor(cfg); current.LastBarActedOn != "2026-08-25" {
		t.Fatalf("bar cursor was not persisted with decision history: %+v", current)
	}
	if got, _ := reopened.DecisionEvents(3, 0); len(got) != 1 {
		t.Fatalf("generation 3 should remain separate, got %+v", got)
	}
}

func TestEngineHistoryCountsOnlySettledBars(t *testing.T) {
	now := ledgerNow
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())

	e.evalConfig(t.Context(), testCfg, now)
	e.evalConfig(t.Context(), testCfg, now.Add(5*time.Minute)) // polling is not another decision

	events, err := store.DecisionEvents(e.ledger.generation, 0)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("one completed bar must produce one history event, got %+v", events)
	}
	if events[0].Decision.ModelVersion != "model-v1" || events[0].Decision.StrategyVersion == "" {
		t.Fatalf("settled event must name the model and strategy used: %+v", events[0].Decision)
	}
}

func TestFileFallbackResetAdvancesTheExperimentGeneration(t *testing.T) {
	dir := t.TempDir()
	ledger, err := openLedger(dir, defaultStartingCash)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	if ledger.Generation() != 0 {
		t.Fatalf("first file generation = %d, want 0", ledger.Generation())
	}
	if err := ledger.Reset(ledgerNow); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if ledger.Generation() != 1 {
		t.Fatalf("reset did not advance the file generation: %d", ledger.Generation())
	}
	reopened, err := openLedger(dir, defaultStartingCash)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Generation() != 1 {
		t.Fatalf("file generation did not survive restart: %d", reopened.Generation())
	}
}
