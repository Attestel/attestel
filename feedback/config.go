package main

import (
	"os"
	"strings"
)

// Config is resolved from environment variables (12-factor). Defaults target zero-config local dev
// (a writable ./data/feedback dir); docker-compose overrides FEEDBACK_DIR with the mounted volume.
type Config struct {
	Port           string
	FeedbackDir    string
	DatabaseURL    string
	DatabaseSchema string

	// Accounts. Submissions are user-owned: the feedback service verifies the SAME httpOnly session
	// cookie the auth service issues, with the SAME AUTH_SECRET (no network hop). Sessions ride in a
	// cookie, so CORS reflects a specific allow-listed Origin with credentials (never "*").
	Secret      string
	CookieName  string
	CORSOrigins []string

	// AdminUIDs is an exact allow-list of opaque auth user ids (NOT email addresses) permitted to
	// read every submission, read the summary, triage and delete (D-16). It is the beta-sized
	// stand-in for a role system in `auth`: one env var, no schema, no new service.
	//
	// EMPTY MEANS NOBODY IS AN ADMIN — the administrative routes fail closed. The service still
	// starts, and authenticated testers can still submit and read their own feedback.
	AdminUIDs []string
}

func loadConfig() Config {
	return Config{
		Port:           env("PORT", "8098"),
		FeedbackDir:    env("FEEDBACK_DIR", "./data/feedback"),
		DatabaseURL:    env("FEEDBACK_DATABASE_URL", env("DATABASE_URL", "")),
		DatabaseSchema: env("FEEDBACK_DATABASE_SCHEMA", "feedback"),

		Secret:      env("AUTH_SECRET", "dev-insecure-change-me"),
		CookieName:  env("COOKIE_NAME", "nvda_session"),
		CORSOrigins: splitCSV(env("CORS_ORIGINS", "http://localhost:5173,http://localhost:4173")),

		AdminUIDs: splitCSV(env("FEEDBACK_ADMIN_UIDS", "")),
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
