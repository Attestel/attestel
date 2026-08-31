// Command paper is the NVDA-platform paper-trading VALIDATION service.
//
// It runs the validated directional signal as a PER-BAR ALLOCATION RULE — the same rule the
// walk-forward backtest scores — and exposes live paper performance NEXT TO that backtest, so you
// can see whether the edge holds out-of-sample. Once per trading session it derives a target
// position from /predict and reconciles what it holds against that target: open, close, flip, or
// hold. There is no fixed holding period and no scheduled exit; `horizon` names the model's LABEL
// horizon, not a hold. The normative rule is docs/PAPER_EXECUTION_CONTRACT.md, and both this engine
// and services/prediction are held to it.
//
// It is FAIL-CLOSED. Nothing opens or flips unless the data is real (never synthetic), the data is
// fresh, the model's walk-forward backtest passes, AND the offline evaluator has recorded an EDGE
// verdict for the current strategy. Today no EDGE verdict exists, so the engine trades nothing and
// says exactly why, per config, at GET /paper/status.
//
// It KEEPS SCORE the way the offline evaluator does (contract §5): one simulated book, opening
// balance PAPER_STARTING_CASH, equal-weight 1/N sizing, the model's own validated cost_bps charged on
// every fill, and one equity snapshot per date marked at that bar's close — a synthetic or missing
// bar is a recorded gap, never a substituted price. That is what lets a live number and a backtest
// number finally disagree meaningfully. The book keeps score; it creates nothing to score.
//
// SIMULATION ONLY. There is no order execution, no broker, and no money movement anywhere, and the
// ledger is simulated bookkeeping rather than an account: nothing in it is withdrawable. Trades are
// recorded in the journal service with mode="paper" and are kept strictly separate from live P&L.
package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg := loadConfig()

	var database *paperDatabase
	var err error
	if cfg.DatabaseURL != "" {
		database, err = openPaperDatabase(cfg.DatabaseURL, cfg.DatabaseSchema, cfg.DataDir)
		if err != nil {
			log.Fatalf("cannot open paper PostgreSQL storage: %v", err)
		}
	}
	var store *Store
	if database != nil {
		store, err = openStoreWithDatabase(cfg.DataDir, cfg.Configs, database)
	} else {
		store, err = openStore(cfg.DataDir, cfg.Configs)
	}
	if err != nil {
		log.Fatalf("cannot open paper engine storage: %v", err)
	}
	clients := newClients(cfg)

	// The fake-money book (docs/PAPER_EXECUTION_CONTRACT.md §5). Initialization failure keeps the
	// service up for diagnosis, but blocks every position change. It never substitutes a fixed-size
	// allocation that would make the live experiment incomparable with the evaluator.
	var ledger *Ledger
	var ledgerErr error
	if database != nil {
		ledger, ledgerErr = openLedgerWithDatabase(cfg.DataDir, cfg.StartingCash, database)
	} else {
		ledger, ledgerErr = openLedger(cfg.DataDir, cfg.StartingCash)
	}
	if ledgerErr != nil {
		log.Printf("paper: LEDGER UNAVAILABLE: %v", ledgerErr)
		ledger = nil
	}

	engine := newEngine(cfg, store, clients, ledger, ledgerErr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go engine.Run(ctx)

	api := newAPI(cfg, store, clients, engine)
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           withLogging(api.routes()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		storage := "files"
		if database != nil {
			storage = "postgresql"
		}
		log.Printf("paper listening on :%s (prediction=%s analysis=%s journal=%s storage=%s)",
			cfg.Port, cfg.PredictionURL, cfg.AnalysisURL, cfg.JournalURL, storage)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down…")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
