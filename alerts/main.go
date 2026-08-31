// Command alerts is the NVDA-platform alerting microservice.
//
// It lets a user define DESCRIPTIVE alert rules over the deterministic signals already computed by
// the analysis + gateway services, evaluates them on a schedule, records triggered events, and
// (optionally) delivers webhook/email notifications. It NEVER emits buy/sell advice and NEVER
// places an order — an alert only ever says "this condition became true".
//
// PostgreSQL is the production store; a local JSON backend remains available when no database URL
// is configured for deterministic tests and zero-configuration development.
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

	store, err := openStoreWithDatabase(cfg.RulesDir, cfg.DatabaseURL, cfg.DatabaseSchema)
	if err != nil {
		log.Fatalf("cannot open rules dir %q: %v", cfg.RulesDir, err)
	}

	notifier := newNotifier(cfg)
	evaluator := newEvaluator(cfg, store, notifier)

	// Background evaluation loop. Cancelled on SIGINT/SIGTERM for a clean shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go evaluator.Run(ctx)

	// Wave 5 Lane 5B. Constructed unconditionally so `GET /monitoring/theses` can answer honestly
	// ("disabled, so no sweep has run") rather than 404ing; `Run` returns immediately when
	// THESIS_MONITOR_ENABLED is not true. It writes stale markers and the re-synthesis QUEUE and
	// makes zero calls to the llm service (§9.23).
	monitor := newThesisMonitor(cfg, store, notifier)
	go monitor.Run(ctx)

	api := newAPI(store, cfg)
	api.monitor = monitor
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           withLogging(api.routes()),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		storage := "files"
		if store.db != nil {
			storage = "postgresql"
		}
		log.Printf("alerts listening on :%s (analysis=%s gateway=%s storage=%s)",
			cfg.Port, cfg.AnalysisURL, cfg.GatewayURL, storage)
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
