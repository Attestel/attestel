package main

import (
	"log"
	"os"
	"strings"
	"time"
)

// Config is resolved from environment variables (12-factor). Defaults target zero-config local dev
// (a writable ./data/auth dir, an insecure dev secret, no Google OAuth) so the whole stack still
// boots with ZERO keys and ZERO network (invariant #1). docker-compose overrides USERS_DIR with the
// mounted volume and MUST set AUTH_SECRET to the SAME value as gateway/journal/alerts/llm so every
// service verifies the same cookie.
type Config struct {
	Port           string
	UsersDir       string
	DatabaseURL    string
	DatabaseSchema string
	Secret         string        // HMAC signing key for session tokens (shared across services)
	SessionTTL     time.Duration // default session lifetime
	RememberTTL    time.Duration // "keep me signed in" lifetime
	CookieName     string
	CORSOrigins    []string // exact allow-list (cookies => no wildcard)

	// Google OAuth (Authorization Code flow). DEFERRED until BOTH ClientID and ClientSecret are set:
	// while either is empty the /auth/google endpoints return 501 and the frontend shows a "coming
	// soon" toast — the offline demo boots with zero keys / zero network (invariant #1).
	GoogleClientID     string
	GoogleClientSecret string
	GoogleRedirectURL  string // must match the Google Console redirect URI byte-for-byte
	AppURL             string // where to send the browser after login (success #view / ?authError=1)
}

const defaultSecret = "dev-insecure-change-me"

func loadConfig() Config {
	// Local-dev convenience: load auth/.env (next to `go run .`) into the environment before reading
	// config, so the Google OAuth keys in auth/.env are picked up without exporting them by hand. The
	// real environment / docker-compose always wins, and a missing .env is a no-op (invariant #1).
	loadDotEnv(".env")
	secret := env("AUTH_SECRET", defaultSecret)
	if secret == defaultSecret {
		log.Printf("WARNING: AUTH_SECRET is the built-in dev default — set a real AUTH_SECRET " +
			"(and the SAME value on gateway/journal/alerts/llm) before exposing this beyond localhost")
	}
	corsOrigins := parseList(env("CORS_ORIGINS", "http://localhost:5173,http://localhost:4173"))
	// APP_URL defaults to the first allow-listed web origin (or localhost) so the post-login redirect
	// lands on the app with zero extra config in dev.
	appDefault := "http://localhost:5173"
	if len(corsOrigins) > 0 {
		appDefault = corsOrigins[0]
	}
	return Config{
		Port:               env("PORT", "8099"),
		UsersDir:           env("USERS_DIR", "./data/auth"),
		DatabaseURL:        env("AUTH_DATABASE_URL", env("DATABASE_URL", "")),
		DatabaseSchema:     env("AUTH_DATABASE_SCHEMA", "auth"),
		Secret:             secret,
		SessionTTL:         envDur("SESSION_TTL", 12*time.Hour),
		RememberTTL:        envDur("REMEMBER_TTL", 30*24*time.Hour),
		CookieName:         env("COOKIE_NAME", "nvda_session"),
		CORSOrigins:        corsOrigins,
		GoogleClientID:     env("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: env("GOOGLE_CLIENT_SECRET", ""),
		GoogleRedirectURL:  env("GOOGLE_REDIRECT_URL", "http://localhost:8099/auth/google/callback"),
		AppURL:             env("APP_URL", appDefault),
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// parseList splits a comma-separated env value, trims, drops blanks.
func parseList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
