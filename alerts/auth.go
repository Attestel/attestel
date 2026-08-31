package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// auth.go — session verification for the alerts service.
//
// Alert rules and their events are user-owned. The monitor keeps evaluating EVERY user's rules (so
// nobody's alerts go quiet), but each event is tagged with its owner and the API only ever returns /
// mutates the caller's own. The alerts service is reachable DIRECTLY from the browser, so it verifies
// the session cookie ITSELF, with the SAME AUTH_SECRET as the auth service — no network hop.

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// SHARED VERIFIER — copy-pasted BYTE-IDENTICAL from auth/token.go (also in gateway/auth.go,
// journal/auth.go and feedback/auth.go). A tiny pure verifier beats a shared module (independent
// stdlib-only Go modules). If you change it, change every copy.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// b64 is URL-safe base64 without padding — compact and cookie-safe.
var b64 = base64.RawURLEncoding

// sessionPayload is the signed claim set. Tiny and stable so the token stays small.
type sessionPayload struct {
	UID string `json:"uid"`
	IAT int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// signPayload computes base64url(HMAC-SHA256(secret, body)).
func signPayload(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return b64.EncodeToString(mac.Sum(nil))
}

// parseToken verifies the signature (constant-time) and the expiry, returning the user id. Pure:
// no globals beyond b64, stdlib only. "" + error for any bad/expired token.
func parseToken(secret, tok string) (string, error) {
	body, sig, ok := strings.Cut(tok, ".")
	if !ok || body == "" || sig == "" {
		return "", errors.New("malformed token")
	}
	if !hmac.Equal([]byte(sig), []byte(signPayload(secret, body))) {
		return "", errors.New("bad signature")
	}
	raw, err := b64.DecodeString(body)
	if err != nil {
		return "", errors.New("bad payload encoding")
	}
	var p sessionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", errors.New("bad payload json")
	}
	if p.UID == "" {
		return "", errors.New("no subject")
	}
	if time.Now().Unix() >= p.Exp {
		return "", errors.New("token expired")
	}
	return p.UID, nil
}

// ───────────────────────────────── end shared verifier ─────────────────────────────────

type ctxKey int

const userIDKey ctxKey = 0

// userIDFrom reads + verifies the session cookie. "" for a guest / invalid / expired token.
func (a *API) userIDFrom(r *http.Request) string {
	c, err := r.Cookie(a.cfg.CookieName)
	if err != nil {
		return ""
	}
	uid, err := parseToken(a.cfg.Secret, c.Value)
	if err != nil {
		return ""
	}
	return uid
}

// userID pulls the resolved id out of the request context (set by optionalAuth/requireAuth).
func userID(r *http.Request) string {
	if v, ok := r.Context().Value(userIDKey).(string); ok {
		return v
	}
	return ""
}

// optionalAuth populates the user id for reads (may be "" — a guest sees an empty rules/events feed).
func (a *API) optionalAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(context.WithValue(r.Context(), userIDKey, a.userIDFrom(r))))
	}
}

// requireAuth rejects a guest with 401 (the UI routes guests to sign-in first; this is the backstop).
func (a *API) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := a.userIDFrom(r)
		if uid == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userIDKey, uid)))
	}
}

// withCORS reflects an allow-listed Origin and enables credentials (cookies) — NOT a wildcard,
// which browsers forbid alongside credentials. Answers OPTIONS preflight.
func (a *API) withCORS(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range a.cfg.CORSOrigins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
