package main

import (
	"os"
	"strings"
	"time"
)

// Config is resolved from environment variables (12-factor). Defaults target zero-config local
// dev (localhost upstreams, a writable ./data/alerts dir); docker-compose overrides ANALYSIS_URL /
// GATEWAY_URL with the service hostnames and RULES_DIR with the mounted volume.
type Config struct {
	Port           string
	AnalysisURL    string
	GatewayURL     string
	Tickers        []string
	EvalInterval   time.Duration
	RulesDir       string
	DatabaseURL    string
	DatabaseSchema string

	// Notifications (all optional — the in-app events feed always works regardless).
	WebhookURL string // generic / Slack / Discord-compatible POST target
	SMTPHost   string
	SMTPPort   string
	SMTPUser   string
	SMTPPass   string
	EmailTo    []string

	// Accounts. Rules + events are scoped per user; the alerts service verifies the SAME httpOnly
	// session cookie the auth service issues, with the SAME AUTH_SECRET (no network hop). CORS
	// reflects a specific allow-listed Origin with credentials (never "*").
	Secret      string
	CookieName  string
	CORSOrigins []string

	// Wave 5 Lane 5B — the thesis monitor. OFF BY DEFAULT, like `EVENT_ENRICH_ENABLED` and
	// `INGEST_ENABLED`: the sweep fans out per followed ticker, and the whole stack must still run
	// with an empty `.env` (invariant #1). It writes stale markers and the re-synthesis QUEUE and
	// makes zero calls to the llm service (§9.23); its tick reads at the last stored bar, never at
	// `now` (§9.24).
	MonitorEnabled  bool
	MonitorInterval time.Duration
	JournalURL      string
}

func loadConfig() Config {
	interval, err := time.ParseDuration(env("EVAL_INTERVAL", "60s"))
	if err != nil || interval <= 0 {
		interval = 60 * time.Second
	}
	return Config{
		Port:           env("PORT", "8095"),
		AnalysisURL:    strings.TrimRight(env("ANALYSIS_URL", "http://localhost:8001"), "/"),
		GatewayURL:     strings.TrimRight(env("GATEWAY_URL", "http://localhost:8080"), "/"),
		Tickers:        parseList(env("TICKERS", "NVDA,GOOGL,TSLA")),
		EvalInterval:   interval,
		RulesDir:       env("RULES_DIR", "./data/alerts"),
		DatabaseURL:    env("ALERTS_DATABASE_URL", env("DATABASE_URL", "")),
		DatabaseSchema: env("ALERTS_DATABASE_SCHEMA", "alerts"),

		WebhookURL: env("ALERT_WEBHOOK_URL", ""),
		SMTPHost:   env("SMTP_HOST", ""),
		SMTPPort:   env("SMTP_PORT", "587"),
		SMTPUser:   env("SMTP_USER", ""),
		SMTPPass:   env("SMTP_PASS", ""),
		EmailTo:    parseList(env("ALERT_EMAIL_TO", "")),

		Secret:      env("AUTH_SECRET", "dev-insecure-change-me"),
		CookieName:  env("COOKIE_NAME", "nvda_session"),
		CORSOrigins: parseList(env("CORS_ORIGINS", "http://localhost:5173,http://localhost:4173")),

		MonitorEnabled:  strings.EqualFold(strings.TrimSpace(env("THESIS_MONITOR_ENABLED", "")), "true"),
		MonitorInterval: monitorInterval(),
		JournalURL:      strings.TrimRight(env("JOURNAL_URL", "http://localhost:8090"), "/"),
	}
}

// monitorInterval is the sweep cadence. A floor of one minute is enforced rather than trusted: the
// sweep reads one bar per distinct followed ticker, and a 5-second interval typed by accident would
// turn a monitor into a load test against the analysis service.
func monitorInterval() time.Duration {
	d, err := time.ParseDuration(env("THESIS_MONITOR_INTERVAL", "15m"))
	if err != nil || d < time.Minute {
		return 15 * time.Minute
	}
	return d
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// parseList splits a comma-separated env value, trims/uppercases-agnostic, drops blanks.
func parseList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
