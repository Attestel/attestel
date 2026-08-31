package main

import (
	"net/http"
	"os"
	"strings"
	"time"
)

// Config is resolved from environment variables (12-factor). Sensible localhost defaults so it
// runs with zero configuration in dev.
type Config struct {
	Port          string
	AnalysisURL   string
	LLMURL        string
	PredictionURL string        // directional-signal quant service (backtest-gated)
	JournalURL    string        // journal service (theses live there; brief runs thesis checks)
	AlertsURL     string        // alerts service — read-only, for the change set's alert_event items
	EventsURL     string        // events service (global event corpus, D-26); see gateway/events.go
	AuthURL       string        // auth service — read ONLY for the one-time activeTicker migration (§9.39)
	CacheTTL      time.Duration // dashboard composite
	QuoteTTL      time.Duration // fresh quote — short (frontend polls this)
	ConfluenceTTL time.Duration // multi-timeframe confluence (3 timeframes -> a bit expensive)
	PredictTTL    time.Duration // prediction /predict (loads a model + scores; cache a couple min)
	NewsTTL       time.Duration // Marketaux (100/day free) — cache hard
	EarningsTTL   time.Duration // Alpha Vantage (25/day free) — cache very hard
	SentimentTTL  time.Duration // StockTwits (unofficial, rate-limited) — cache hard
	BriefTTL      time.Duration // AI Market Brief (LLM synthesis of the day's facts) — cache ~30 min
	AssistantTTL  time.Duration // AI assistant situational note — cache ~90s (chat is never cached)
	CommitteeTTL  time.Duration // analyst-committee pipeline (many sequential LLM calls) — cache hard
	DigestTTL     time.Duration // news-cascade digest (clusters + impact scores) — cache like the brief
	CalendarTTL   time.Duration // canonical event-store calendar — short enough for lifecycle changes

	Tickers []Ticker // configurable universe surfaced to the frontend switcher

	AlphaVantageKey string // live historical EPS beat/miss; empty -> seed fallback
	MarketauxKey    string // live news+sentiment; empty -> seed fallback
	SECUserAgent    string // SEC requires a UA with contact info; no key needed

	StockTwitsEnabled bool   // crowd-sentiment source (keyless); disable to skip it entirely
	StockTwitsBase    string // unofficial public API base

	// ReadyRequire names the upstreams `/ready` treats as load-bearing (see health.go). Default
	// "analysis,events" — the two PostgreSQL-backed services, which cannot answer a single real
	// query without the database. `llm` is excluded on purpose: invariant #1b makes `stub:offline`
	// a shipped shape, not an outage.
	ReadyRequire []string

	// Accounts (shared session verification). The gateway verifies the SAME httpOnly cookie the
	// auth service issues, using the SAME AUTH_SECRET (no network hop), and forwards the resolved
	// identity to the llm as X-User-Id. Because sessions ride in a cookie, CORS reflects a specific
	// allow-listed Origin with credentials (never "*").
	Secret      string   // AUTH_SECRET — must match auth/journal/alerts/llm
	CookieName  string   // session cookie name (default nvda_session)
	CORSOrigins []string // exact allow-list for credentialed CORS

	// EvalAdminUIDs gates price/PEAD evaluation and estimate-collection POST routes (evaluate.go):
	// opaque auth user ids allowed to start a run. Same shape and same rule as the feedback
	// service's FEEDBACK_ADMIN_UIDS (D-16) — user ids from `GET /auth/me` -> `user.id`, NEVER email
	// addresses, and **empty means nobody**, so an unconfigured deployment cannot have a
	// minutes-long CPU-heavy job started by whoever finds the URL. Evaluator status reads need only
	// a signed-in session.
	EvalAdminUIDs []string
}

func loadConfig() Config {
	return Config{
		Port:          env("PORT", "8080"),
		AnalysisURL:   env("ANALYSIS_URL", "http://localhost:8001"),
		LLMURL:        env("LLM_URL", "http://localhost:8002"),
		PredictionURL: env("PREDICTION_URL", "http://localhost:8003"),
		JournalURL:    env("JOURNAL_URL", "http://localhost:8096"),
		AlertsURL:     env("ALERTS_URL", "http://localhost:8095"),
		EventsURL:     env("EVENTS_URL", "http://localhost:8004"),
		AuthURL:       env("AUTH_URL", "http://localhost:8099"),
		CacheTTL:      5 * time.Minute,
		QuoteTTL:      15 * time.Second,
		ConfluenceTTL: 2 * time.Minute,
		PredictTTL:    2 * time.Minute,
		NewsTTL:       30 * time.Minute,
		EarningsTTL:   12 * time.Hour,
		SentimentTTL:  envDur("SENTIMENT_TTL", 10*time.Minute),
		BriefTTL:      envDur("BRIEF_TTL", 30*time.Minute),
		AssistantTTL:  envDur("ASSISTANT_TTL", 90*time.Second),
		CommitteeTTL:  envDur("COMMITTEE_TTL", 30*time.Minute),
		DigestTTL:     envDur("DIGEST_TTL", 30*time.Minute),
		CalendarTTL:   envDur("CALENDAR_TTL", 15*time.Minute),

		Tickers: parseTickers(env("TICKERS", "NVDA,GOOGL,TSLA")),

		AlphaVantageKey: env("ALPHAVANTAGE_API_KEY", ""),
		MarketauxKey:    env("MARKETAUX_API_KEY", ""),
		SECUserAgent:    env("SEC_USER_AGENT", "NVDA-Platform/0.1 (you@example.com)"),

		StockTwitsEnabled: envBool("STOCKTWITS_ENABLED", true),
		StockTwitsBase:    env("STOCKTWITS_BASE", "https://api.stocktwits.com/api/2"),

		ReadyRequire: splitCSV(env("READY_REQUIRE", "analysis,events")),

		Secret:      env("AUTH_SECRET", "dev-insecure-change-me"),
		CookieName:  env("COOKIE_NAME", "nvda_session"),
		CORSOrigins: splitCSV(env("CORS_ORIGINS", "http://localhost:5173,http://localhost:4173")),

		EvalAdminUIDs: splitCSV(env("EVAL_ADMIN_UIDS", "")), // empty default = nobody; fail closed
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

func envBool(k string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// Server holds shared dependencies for handlers.
type Server struct {
	cfg   Config
	cache *ttlCache
	http  *http.Client
	cik   cikCache // ticker->CIK, lazily loaded from SEC company_tickers.json
	// jobs holds in-flight and just-finished ANALYST runs (analystjobs.go). Lazily built by
	// `s.analystRuns()`, so a hand-built `&Server{…}` literal — every test in this package — keeps
	// working without naming it.
	jobs *analystJobs
}
