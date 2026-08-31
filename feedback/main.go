// Command feedback is the NVDA-platform tester-feedback microservice.
//
// Testers submit ratings, bug reports, and ideas from the frontend; an admin reviews and triages them
// (new -> reviewed -> resolved) in Settings → Help & feedback. Each submission can carry an
// auto-captured context snapshot (ticker, view, timeframe, data-source flags) so bugs are
// reproducible without interrogating the tester. PostgreSQL is authoritative in production; JSON
// remains only as the explicit local fallback and one-time legacy import source.
//
// Submissions are user-owned and may contain personal data, so every route except /health requires a
// signed-in tester (the shared session cookie, verified locally), reads are scoped to the caller, and
// the global read / summary / triage / delete surface is restricted to the FEEDBACK_ADMIN_UIDS
// allow-list (D-16). It executes nothing, and it still runs with zero keys and zero network.
package main

import (
	"log"
	"net/http"
	"time"
)

func main() {
	cfg := loadConfig()

	store, err := openStoreWithDatabase(cfg.FeedbackDir, cfg.DatabaseURL, cfg.DatabaseSchema)
	if err != nil {
		log.Fatalf("cannot open feedback dir %q: %v", cfg.FeedbackDir, err)
	}

	srv := &Server{cfg: cfg, store: store}

	// Fail closed, and say so. An empty allow-list is a valid (and default) configuration: testers can
	// still sign in and submit, but nobody can read the whole corpus, triage or delete.
	if len(cfg.AdminUIDs) == 0 {
		log.Printf("WARNING: FEEDBACK_ADMIN_UIDS is empty — no admin. Global read, summary, triage " +
			"and delete are disabled; testers can still submit and read their own feedback.")
	} else {
		log.Printf("feedback admin allow-list: %d uid(s)", len(cfg.AdminUIDs))
	}

	handler := withLogging(srv.routes())
	storage := "files"
	if store.db != nil {
		storage = "postgresql"
	}
	log.Printf("feedback listening on :%s (storage=%s)", cfg.Port, storage)
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
