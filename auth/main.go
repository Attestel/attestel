// Command auth is the NVDA-platform accounts microservice.
//
// It owns a LOCAL user store (email + PBKDF2 password hash) and mints HMAC-signed session cookies
// that every other service verifies with the same AUTH_SECRET (no network hop). Accounts only SCOPE
// user-owned data (personas, journal, theses, alert rules) — they unlock no trading action, no
// order execution, no money movement (invariant #2). The whole stack still boots with ZERO keys and
// ZERO network (invariant #1): a local file fallback, hand-rolled crypto, Google OAuth deferred.
package main

import (
	"log"
	"net/http"
	"time"
)

func main() {
	cfg := loadConfig()

	var store *Store
	var settings *SettingsStore
	var err error
	if cfg.DatabaseURL != "" {
		store, settings, err = openPostgresStores(cfg.UsersDir, cfg.DatabaseURL, cfg.DatabaseSchema)
	} else {
		store, err = openStore(cfg.UsersDir)
		if err == nil {
			settings, err = openSettingsStore(cfg.UsersDir)
		}
	}
	if err != nil {
		log.Fatalf("cannot open auth storage: %v", err)
	}

	srv := &Server{cfg: cfg, store: store, settings: settings}

	handler := withLogging(srv.routes())
	storage := "files"
	if cfg.DatabaseURL != "" {
		storage = "postgresql"
	}
	log.Printf("auth listening on :%s (storage=%s googleOAuth=%v)",
		cfg.Port, storage, cfg.GoogleClientID != "")
	if err := http.ListenAndServe(":"+cfg.Port, handler); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// withLogging logs method + path + latency. It NEVER logs bodies — passwords must not reach the log.
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
