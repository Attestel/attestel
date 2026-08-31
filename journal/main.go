// Command journal is the NVDA-platform trade-journal microservice.
//
// It is a manual RECORD of trades the user made themselves: log entries/exits, compute P&L and
// performance stats, and attach the analytical read that was live at entry so evidence can be
// reviewed against outcome. It executes NOTHING — no broker, no orders, no money movement — and it
// emits no buy/sell advice.
package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	cfg := loadConfig()

	// The internal product readout is a local command, not a route: §6.1 forbids an Analytics
	// destination, and an internal HTTP surface would need an access-control policy that D-16 has not
	// resolved. `journal -analytics-report` prints and exits without ever binding a port.
	if len(os.Args) > 1 && (os.Args[1] == "-analytics-report" || os.Args[1] == "--analytics-report") {
		os.Exit(runAnalyticsReport(os.Args[2:], cfg.TradesDir, cfg.DatabaseURL, cfg.DatabaseSchema))
	}

	store, err := openStoreWithDatabase(cfg.TradesDir, cfg.DatabaseURL, cfg.DatabaseSchema)
	if err != nil {
		log.Fatalf("cannot open trades dir %q: %v", cfg.TradesDir, err)
	}
	documents := newDocumentRepository(store.db, store.schema, cfg.TradesDir)
	theses, err := openThesisStoreWithRepository(cfg.TradesDir, documents)
	if err != nil {
		log.Fatalf("cannot open theses store in %q: %v", cfg.TradesDir, err)
	}
	portfolios, err := openPortfolioStoreWithRepository(cfg.TradesDir, documents)
	if err != nil {
		log.Fatalf("cannot open portfolios store in %q: %v", cfg.TradesDir, err)
	}
	portfolioSnapshots, err := openPortfolioSnapshotStoreWithRepository(cfg.TradesDir, documents)
	if err != nil {
		log.Fatalf("cannot open portfolio snapshot store in %q: %v", cfg.TradesDir, err)
	}
	portfolioReviews, err := openPortfolioReviewStoreWithRepository(cfg.TradesDir, documents)
	if err != nil {
		log.Fatalf("cannot open portfolio review store in %q: %v", cfg.TradesDir, err)
	}

	// Self-hosted, server-side product events (§6). A package-level sink rather than a Server field:
	// every emitter is a durable-write site inside this package, and this keeps the analytics lane's
	// footprint in files it does not own down to one call each.
	analytics = openAnalyticsSinkWithRepository(cfg.TradesDir, cfg.AnalyticsSalt, cfg.AnalyticsDemoUIDs, documents)

	srv := &Server{
		cfg:                cfg,
		store:              store,
		theses:             theses,
		portfolios:         portfolios,
		portfolioSnapshots: portfolioSnapshots,
		portfolioReviews:   portfolioReviews,
		documents:          documents,
		http:               &http.Client{Timeout: 15 * time.Second},
	}

	handler := withLogging(srv.routes())
	storage := "files"
	if cfg.DatabaseURL != "" {
		storage = "postgresql"
	}
	log.Printf("journal listening on :%s (analysis=%s gateway=%s tradeStorage=%s)",
		cfg.Port, cfg.AnalysisURL, cfg.GatewayURL, storage)
	if err := http.ListenAndServe(":"+cfg.Port, handler); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
