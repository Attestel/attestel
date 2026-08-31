package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// auth.go — session verification at the gateway.
//
// The gateway proxies /api/personas and /api/chat to the llm and reads/writes the journal for thesis
// checks. It verifies the SAME httpOnly session cookie the auth service issues, with the SAME
// AUTH_SECRET, LOCALLY (no network hop), then:
//   - forwards the resolved identity to the llm as X-User-Id (the llm isn't browser-exposed on that
//     path, so it trusts the header) — a user's custom persona resolves; a guest gets built-ins.
//   - requires a non-empty user for persona mutations (POST/PATCH/DELETE) — GET stays open.
//   - forwards the browser's Cookie to the journal so per-user theses scope correctly.

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// SHARED VERIFIER — copy-pasted BYTE-IDENTICAL from auth/token.go (also in journal/auth.go,
// alerts/auth.go and feedback/auth.go). A tiny pure verifier beats a shared module: the gateway is
// stdlib-only Go with zero deps (invariant #5). If you change it, change every copy.
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

// userIDFrom reads + verifies the session cookie. "" for a guest / invalid / expired token.
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

// identity carries the verified user id (for scoping/routing) plus the raw Cookie header (to forward
// to the journal, which verifies it itself). userID is "" for a guest.
type identity struct {
	userID string
	cookie string
}

func (s *Server) identityFrom(r *http.Request) identity {
	return identity{userID: s.userIDFrom(r), cookie: r.Header.Get("Cookie")}
}

// journalGet does a GET to the journal forwarding the browser's session cookie so per-user scoping
// applies server-side. Best-effort (nil on any error), like the plain getJSON.
func (s *Server) journalGet(ctx context.Context, url, cookie string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, errors.New("journal " + url + ": " + truncate(body, 120))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// postJSONUser POSTs like postJSON but stamps the gateway-verified X-User-Id so the llm resolves the
// caller's own personas (a guest — "" — gets built-ins only).
func (s *Server) postJSONUser(ctx context.Context, url string, payload any, userID string) (map[string]any, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", userID)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream %s -> %d: %s", url, resp.StatusCode, truncate(body, 200))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", url, err)
	}
	return out, nil
}

// withCORS reflects an allow-listed Origin and enables credentials (cookies) — NOT a wildcard,
// which browsers forbid alongside credentials. Answers OPTIONS preflight. Allows the mutating
// methods persona CRUD needs. Non-allowlisted origins simply get no CORS headers.
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
