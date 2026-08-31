package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresPaperBookSurvivesReopenAndArchivesReset(t *testing.T) {
	url := os.Getenv("PAPER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PAPER_TEST_DATABASE_URL is not set")
	}
	schema := fmt.Sprintf("test_paper_%d", time.Now().UnixNano())
	cfg := PaperCfg{Ticker: "NVDA", Timeframe: "1D", Horizon: 5}
	repo, err := openPaperDatabase(url, schema, t.TempDir())
	if err != nil {
		t.Fatalf("open PostgreSQL paper repository: %v", err)
	}
	store, err := openStoreWithDatabase("", []PaperCfg{cfg}, repo)
	if err != nil {
		t.Fatalf("open engine store: %v", err)
	}
	state := store.StateFor(cfg)
	state.LastBarActedOn = "2026-08-25"
	state.LastBarUnix = ledgerNow.Unix()
	decision := DecisionEvent{
		Generation: 1, Config: cfg.Key(), Ticker: cfg.Ticker, Timeframe: cfg.Timeframe, Horizon: cfg.Horizon,
		Decision: Decision{
			At: ledgerNow.Format(time.RFC3339), Bar: state.LastBarActedOn, BarUnix: state.LastBarUnix,
			From: "flat", Target: "long", Action: "open", Reason: "integration fixture",
			ModelVersion: "model-v1", StrategyVersion: "strategy-v1",
		},
	}
	if err := store.SaveDecision(state, decision); err != nil {
		t.Fatalf("save engine state and decision: %v", err)
	}
	ledger, err := openLedgerWithDatabase("", defaultStartingCash, repo)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	if _, err := ledger.Open(cfg, "long", "2026-08-25", "trade-1", 100, 6, 1, ledgerNow); err != nil {
		t.Fatalf("book fill: %v", err)
	}
	if err := ledger.Mark([]string{cfg.Key()}, cfg.Key(), "2026-08-25", 101, true); err != nil {
		t.Fatalf("mark ledger: %v", err)
	}
	if err := store.SaveShadowObservation(shadowFixtureObservation("Buy")); err != nil {
		t.Fatalf("save shadow observation: %v", err)
	}
	replacement := shadowFixtureObservation("Buy")
	replacement.EntryPrice = 999
	if err := store.SaveShadowObservation(replacement); err != nil {
		t.Fatalf("retry shadow observation: %v", err)
	}
	if err := store.SaveShadowBars(shadowFixtureBars(10), ledgerNow); err != nil {
		t.Fatalf("save shadow bars and outcomes: %v", err)
	}
	repo.db.Close()

	reopenedRepo, err := openPaperDatabase(url, schema, t.TempDir())
	if err != nil {
		t.Fatalf("reopen repository: %v", err)
	}
	defer func() {
		_, _ = reopenedRepo.db.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		reopenedRepo.db.Close()
	}()
	reopenedStore, err := openStoreWithDatabase("", []PaperCfg{cfg}, reopenedRepo)
	if err != nil || reopenedStore.StateFor(cfg).LastBarActedOn != "2026-08-25" {
		t.Fatalf("engine state did not survive reopen: state=%+v err=%v", reopenedStore.StateFor(cfg), err)
	}
	reopenedLedger, err := openLedgerWithDatabase("", defaultStartingCash, reopenedRepo)
	if err != nil || reopenedLedger.LotFor(cfg.Key()) == nil {
		t.Fatalf("ledger lot did not survive reopen: lot=%+v err=%v", reopenedLedger.LotFor(cfg.Key()), err)
	}
	if fills, err := reopenedLedger.readFills(0); err != nil || len(fills) != 1 {
		t.Fatalf("fills did not survive reopen: fills=%v err=%v", fills, err)
	}
	if events, err := reopenedStore.DecisionEvents(1, 0); err != nil || len(events) != 1 {
		t.Fatalf("decision history did not survive reopen: events=%v err=%v", events, err)
	}
	if report, err := reopenedStore.ShadowReport(10); err != nil || report.Observations != 1 || report.SettledOutcomes != 4 || report.Recent[0].Observation.EntryPrice != 100 {
		t.Fatalf("shadow evidence did not survive reopen: report=%+v err=%v", report, err)
	}
	if err := reopenedLedger.Reset(ledgerNow.Add(time.Hour)); err != nil {
		t.Fatalf("reset ledger: %v", err)
	}
	if fills, err := reopenedLedger.readFills(0); err != nil || len(fills) != 0 {
		t.Fatalf("new generation was not empty: fills=%v err=%v", fills, err)
	}
	var archived int
	if err := reopenedRepo.db.QueryRow(
		`SELECT count(*) FROM "`+schema+`".fills WHERE generation < $1`, reopenedLedger.generation,
	).Scan(&archived); err != nil || archived != 1 {
		t.Fatalf("previous fill was not archived by generation: count=%d err=%v", archived, err)
	}
	if events, err := reopenedStore.DecisionEvents(reopenedLedger.generation, 0); err != nil || len(events) != 0 {
		t.Fatalf("new generation must not inherit old decisions: events=%v err=%v", events, err)
	}
	if events, err := reopenedStore.DecisionEvents(1, 0); err != nil || len(events) != 1 {
		t.Fatalf("archived generation lost its decision history: events=%v err=%v", events, err)
	}
	if report, err := reopenedStore.ShadowReport(10); err != nil || report.Observations != 1 || report.SettledOutcomes != 4 {
		t.Fatalf("official reset changed all-generations shadow evidence: report=%+v err=%v", report, err)
	}
	archives, err := reopenedRepo.archivedExperiments(context.Background())
	if err != nil || len(archives) != 1 {
		t.Fatalf("archived experiment index = %+v err=%v", archives, err)
	}
	if archives[0].Generation != 1 || archives[0].NFills != 1 || archives[0].NDecisions != 1 || archives[0].NSnapshots != 1 {
		t.Fatalf("archived experiment counts do not reconcile: %+v", archives[0])
	}
}
