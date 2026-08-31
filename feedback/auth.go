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

// auth.go — session verification and the admin allow-list for the feedback service (D-16).
//
// Submissions are user-owned and may carry personal data, so every route except /health requires a
// signed-in tester. The feedback service is reachable DIRECTLY from the browser, so it verifies the
// session cookie ITSELF (it can't trust a header), with the SAME AUTH_SECRET as the auth service —
// no network hop, no new dependency.
//
// Two levels, and only two:
//
//	requireAuth   any signed-in tester — submit, and read your OWN submissions
//	requireAdmin  an exact FEEDBACK_ADMIN_UIDS allow-list — read everything, summary, triage, delete
//
// The allow-list is deliberately not a role system in `auth`: for a closed beta one env var of opaque
// user ids is the whole requirement, and it fails closed when unset. Network isolation of port 8098
// remains defence in depth — it is no longer the only boundary.

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// SHARED VERIFIER — copy-pasted BYTE-IDENTICAL from auth/token.go (also in gateway/auth.go,
// journal/auth.go and alerts/auth.go). A tiny pure verifier beats a shared module (these are
// independent stdlib-only Go modules). If you change it, change every copy.
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

// userIDFrom reads + verifies the session cookie. "" for a guest / malformed / forged / expired token.
func (s *Server) userIDFrom(r *http.Request) string {
	c, err := r.Cookie(s.cfg.CookieName)
	if err != nil {
		return ""
	}
	uid, err := parseToken(s.cfg.Secret, c.Value)
	if err != nil {
		return ""
	}
	return uid
}

// userID pulls the resolved id out of the request context (set by requireAuth/requireAdmin).
func userID(r *http.Request) string {
	if v, ok := r.Context().Value(userIDKey).(string); ok {
		return v
	}
	return ""
}

// isAdmin reports whether uid is on the configured allow-list. An empty allow-list means nobody is
// an admin, and a guest ("") is never one — the administrative routes fail closed by construction.
func (s *Server) isAdmin(uid string) bool {
	if uid == "" {
		return false
	}
	for _, a := range s.cfg.AdminUIDs {
		if a == uid {
			return true
		}
	}
	return false
}

// requireAuth rejects a guest with 401 (the UI routes guests to sign-in first; this is the backstop
// that keeps "you must be signed in to send or read feedback" true even if the UI is bypassed).
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := s.userIDFrom(r)
		if uid == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userIDKey, uid)))
	}
}

// requireAdmin gates the owner-triage surface: 401 for a guest, 403 for a signed-in non-admin.
//
// The check runs BEFORE the handler looks a record up, so a non-admin probing ids learns nothing
// about which ones exist — /feedback/{id} answers 403 identically for a real id, a foreign id and a
// nonexistent one.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := s.userIDFrom(r)
		if uid == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
			return
		}
		if !s.isAdmin(uid) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "admin access required"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userIDKey, uid)))
	}
}

// withCORS reflects an allow-listed Origin and enables credentials (cookies) — NOT a wildcard,
// which browsers forbid alongside credentials. Answers OPTIONS preflight (no session needed: the
// preflight itself carries none, and a browser will not send the real request until it succeeds).
func (s *Server) withCORS(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range s.cfg.CORSOrigins {
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
