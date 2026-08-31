package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// PaperCfg is one auto-papered configuration: which (ticker, timeframe, horizon) the engine trades
// on the validated signal.
//
// HORIZON IS NOT A HOLDING PERIOD. It names the model's LABEL horizon — the H in "will price be
// higher in H bars?" — and it selects which trained record /predict serves. The engine re-decides
// its position on EVERY bar (docs/PAPER_EXECUTION_CONTRACT.md §1), exactly as the backtest does, so
// nothing here is held for H of anything.
//
// The previous comment claimed the H-bar hold "mirrors the backtest's fixed-horizon exit
// (apples-to-apples)". The backtest has no fixed-horizon exit — it re-decides every bar and pays the
// next bar's return. The hold, the claim, and the comparison built on it were all wrong.
type PaperCfg struct {
	Ticker    string `json:"ticker"`
	Timeframe string `json:"timeframe"`
	Horizon   int    `json:"horizon"`
}

func (c PaperCfg) Key() string {
	return c.Ticker + ":" + c.Timeframe + ":" + strconv.Itoa(c.Horizon)
}

// Config is resolved from environment variables. Localhost defaults for zero-config local dev;
// docker-compose overrides the URLs with the in-network service hostnames.
type Config struct {
	Port           string
	PredictionURL  string
	AnalysisURL    string
	JournalURL     string
	Configs        []PaperCfg
	EvalInterval   time.Duration
	DataDir        string
	DatabaseURL    string
	DatabaseSchema string

	// StartingCash is the simulated book's opening balance (docs/PAPER_EXECUTION_CONTRACT.md §5.2).
	// It is fake money: there is no account, no broker and no balance anybody can withdraw. Returns
	// are scale-free, so the number changes no statistic this service reports.
	StartingCash float64

	// TESTING/DEMO ONLY. When > 0, every EVAL_INTERVAL tick is counted as a new bar so a demo can
	// fast-forward. The value itself is no longer a bar LENGTH — bar identity comes from the
	// analysis service's own bar timestamps (contract §2), never from wall-clock arithmetic. 0
	// (the default, and the only correct production value) means real bars only.
	BarSeconds int

	// Freshness gates (contract §4, gate 2). Sessions are approximated by counting WEEKDAYS, which
	// over-counts across holidays — i.e. it calls data stale slightly early. That is the correct
	// direction to be wrong in.
	MaxBarAgeSessions   int // latest bar vs today
	MaxModelAgeSessions int // the record's dataThrough vs the latest bar

	// The engine's service credential (D-20 interim (a), see auth.go). Simulated trades are recorded
	// against a DEDICATED SYSTEM USER — the platform's validation book, never a person's. With no
	// AUTH_SECRET the engine sends no credential and behaves exactly as it did before.
	AuthSecret string
	CookieName string
	SystemUID  string

	// Exact allow-list of browser origins, mirroring the feedback service's pattern. EMPTY BY
	// DEFAULT, which means NO CORS headers at all: this service's reads are proxied same-origin in
	// every shipped deployment, and `Access-Control-Allow-Origin: *` — what it used to answer — is
	// forbidden by browsers alongside credentials, so it made the two authenticated POSTs
	// un-callable while advertising that anything could call them.
	CORSOrigins []string
}

// systemBookLabel is what the UI must call this book while the interim credential is in use: the
// platform validation engine's, not the user's. D-20's caveat is explicit that presenting a shared
// global book as "my experiment" is a contract violation.
const systemBookLabel = "platform validation engine"

func loadConfig() Config {
	interval, err := time.ParseDuration(env("EVAL_INTERVAL", "5m"))
	if err != nil || interval <= 0 {
		interval = 5 * time.Minute
	}
	cash, err := strconv.ParseFloat(env("PAPER_STARTING_CASH", ""), 64)
	if err != nil || cash <= 0 {
		cash = defaultStartingCash
	}
	barSecs, _ := strconv.Atoi(env("PAPER_BAR_SECONDS", "0"))
	return Config{
		Port:           env("PORT", "8097"),
		PredictionURL:  strings.TrimRight(env("PREDICTION_URL", "http://localhost:8003"), "/"),
		AnalysisURL:    strings.TrimRight(env("ANALYSIS_URL", "http://localhost:8001"), "/"),
		JournalURL:     strings.TrimRight(env("JOURNAL_URL", "http://localhost:8096"), "/"),
		Configs:        parseConfigs(env("CONFIGS", "NVDA:1D:5,GOOGL:1D:5,TSLA:1D:5")),
		EvalInterval:   interval,
		DataDir:        env("DATA_DIR", "./data/paper"),
		DatabaseURL:    env("PAPER_DATABASE_URL", env("DATABASE_URL", "")),
		DatabaseSchema: env("PAPER_DATABASE_SCHEMA", "paper"),
		StartingCash:   cash,
		BarSeconds:     barSecs,

		MaxBarAgeSessions:   posInt(env("PAPER_MAX_BAR_AGE_SESSIONS", "3"), 3),
		MaxModelAgeSessions: posInt(env("PAPER_MAX_MODEL_AGE_SESSIONS", "10"), 10),

		AuthSecret: os.Getenv("AUTH_SECRET"), // no default: an unset secret means "no credential"
		CookieName: env("COOKIE_NAME", "nvda_session"),
		SystemUID:  env("PAPER_SYSTEM_UID", "paper-engine"),

		// No default. An unset CORS_ORIGINS means "no browser may call this cross-origin", which is
		// the correct answer for a service every shipped deployment reaches same-origin.
		CORSOrigins: splitCSV(os.Getenv("CORS_ORIGINS")),
	}
}

// fastForward reports whether every tick should be counted as a new bar (demo/testing only).
func (c Config) fastForward() bool { return c.BarSeconds > 0 }

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// splitCSV splits a comma-separated env value, trims, drops blanks. Copied from feedback/config.go,
// which is where this CORS pattern comes from.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// posInt parses a positive int, falling back to def. A zero or negative freshness budget would mean
// "nothing is ever fresh enough" by accident rather than by decision, so it is refused.
func posInt(s string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// parseConfigs parses "NVDA:1D:5,GOOGL:1D:5" -> []PaperCfg. Malformed entries are skipped.
func parseConfigs(s string) []PaperCfg {
	var out []PaperCfg
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bits := strings.Split(part, ":")
		if len(bits) != 3 {
			continue
		}
		h, err := strconv.Atoi(strings.TrimSpace(bits[2]))
		if err != nil || h <= 0 {
			continue
		}
		c := PaperCfg{
			Ticker:    strings.ToUpper(strings.TrimSpace(bits[0])),
			Timeframe: normalizeTimeframe(strings.TrimSpace(bits[1])),
			Horizon:   h,
		}
		if c.Ticker == "" || seen[c.Key()] {
			continue
		}
		seen[c.Key()] = true
		out = append(out, c)
	}
	return out
}

var allowedTimeframes = map[string]bool{"1D": true, "1H": true, "15m": true, "5m": true}

func normalizeTimeframe(tf string) string {
	if allowedTimeframes[tf] {
		return tf
	}
	switch strings.ToLower(strings.TrimSpace(tf)) {
	case "1h", "hour":
		return "1H"
	case "15min":
		return "15m"
	case "5min":
		return "5m"
	default:
		return "1D"
	}
}
