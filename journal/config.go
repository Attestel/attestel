package main

import (
	"os"
	"strings"
)

// Config is resolved from environment variables (12-factor). Defaults target zero-config local dev
// (localhost upstreams, a writable ./data/journal dir); docker-compose overrides ANALYSIS_URL /
// GATEWAY_URL with the service hostnames and TRADES_DIR with the mounted volume.
type Config struct {
	Port        string
	AnalysisURL string
	GatewayURL  string
	LLMURL      string
	// Phase 3. The canonical event store, read ONLY for the store-backed relationship view that
	// event-impact reviews join against. The journal never writes to it and never fetches a
	// provider through it — the events service refuses to fetch on a read path.
	EventsURL      string
	TradesDir      string
	DatabaseURL    string
	DatabaseSchema string

	// Accounts. Trades + theses are scoped per user; the journal verifies the SAME httpOnly session
	// cookie the auth service issues, with the SAME AUTH_SECRET (no network hop). Sessions ride in a
	// cookie, so CORS reflects a specific allow-listed Origin with credentials (never "*").
	Secret      string
	CookieName  string
	CORSOrigins []string

	// Self-hosted product analytics (PARALLEL_CONTRACTS.md §6). AnalyticsSalt salts the one-way
	// identity hash written to the event log; it defaults to AUTH_SECRET so the hash is stable across
	// restarts (retention cohorts depend on that) and non-guessable in a real deployment, without
	// adding a second secret to configure. AnalyticsDemoUIDs flags accounts whose activity is DEMO
	// activity — excluded from the default readout. Server-side only: a browser cannot flag itself.
	AnalyticsSalt     string
	AnalyticsDemoUIDs []string

	// Hermes research agency (agency.go). Both default to OFF and both fail closed.
	//
	// AgencyOwnerUIDs is the allowlist of accounts that may create a research run. EMPTY MEANS
	// NOBODY — the same posture gateway's EVAL_ADMIN_UIDS takes, and for the same reason: an
	// unconfigured deployment must not expose an owner-only capability to whoever signs up first.
	//
	// AgencyWorkerToken authenticates the local bridge, and it is DELIBERATELY NOT `AUTH_SECRET`.
	// AUTH_SECRET signs session cookies; a credential that lives on a laptop and can be turned into
	// a session for an arbitrary user is a credential whose compromise is an account compromise.
	// Empty disables the worker routes outright (403 naming this variable).
	AgencyOwnerUIDs   []string
	AgencyWorkerToken string
}

func loadConfig() Config {
	return Config{
		Port:           env("PORT", "8096"),
		AnalysisURL:    strings.TrimRight(env("ANALYSIS_URL", "http://localhost:8001"), "/"),
		GatewayURL:     strings.TrimRight(env("GATEWAY_URL", "http://localhost:8080"), "/"),
		LLMURL:         strings.TrimRight(env("LLM_URL", "http://localhost:8002"), "/"),
		EventsURL:      strings.TrimRight(env("EVENTS_URL", "http://localhost:8004"), "/"),
		TradesDir:      env("TRADES_DIR", "./data/journal"),
		DatabaseURL:    env("JOURNAL_DATABASE_URL", env("DATABASE_URL", "")),
		DatabaseSchema: env("JOURNAL_DATABASE_SCHEMA", "journal"),

		Secret:      env("AUTH_SECRET", "dev-insecure-change-me"),
		CookieName:  env("COOKIE_NAME", "nvda_session"),
		CORSOrigins: splitCSV(env("CORS_ORIGINS", "http://localhost:5173,http://localhost:4173")),

		AnalyticsSalt:     env("ANALYTICS_SALT", env("AUTH_SECRET", "dev-insecure-change-me")),
		AnalyticsDemoUIDs: splitCSV(env("ANALYTICS_DEMO_UIDS", "")),

		// No fallback to AUTH_SECRET on either line, deliberately. See the field comments.
		//
		// TrimSpace on the token is load-bearing, not tidiness. The credential is generated with
		// something like `openssl rand -hex 32`, which appends a newline, and several deployment
		// platforms set a secret from a file verbatim — newline included. The bridge trims its own
		// copy (`readToken`), so an untrimmed server value made the two differ by one byte and the
		// constant-time compare failed on LENGTH. The symptom is a flat 401 on every claim with two
		// values that look identical wherever you print them, which is close to undebuggable.
		AgencyOwnerUIDs:   splitCSV(env("AGENCY_OWNER_UIDS", "")),
		AgencyWorkerToken: strings.TrimSpace(env("AGENCY_WORKER_TOKEN", "")),
	}
}

// splitCSV splits a comma-separated env value, trims, drops blanks.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
