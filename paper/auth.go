package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// auth.go — the service credential that lets the paper engine RECORD its simulated trades.
//
// D-20, interim option (a). The engine posts to `journal POST /trades`, which is gated behind
// requireAuth; today it sends no credential at all, so every simulated trade is rejected with 401 and
// Journal → Experiments shows a permanently empty book (audit §10.3). Nothing about that is a
// safety property — it is a missing credential, and the fix is to give the engine an identity.
//
// The identity is a DEDICATED SYSTEM USER, not a person. That distinction is the whole of D-20's
// interim caveat: the book this engine keeps belongs to the platform's validation harness, and the UI
// must say so rather than presenting a shared global book as "my experiment". The per-user engines of
// option (b) — each user opting in, simulating under their own id — remain the target.
//
// The signer below is the SIGNING half of the verifier copied byte-identically across auth/token.go,
// gateway/auth.go, journal/auth.go and alerts/auth.go. Same secret, same payload shape, so `journal`
// verifies this token with the code it already has and no service learns a new trick.
//
// The VERIFYING half now lives here too, for the opposite direction: this service's own MUTATING
// routes. `POST /paper/reset` deletes every paper trade in the journal and `POST /paper/config`
// rewrites what the engine trades, and both were reachable by anyone who could reach the port —
// `confirm=true` is a typo guard, not authentication. `parseToken` below is copied BYTE-IDENTICALLY
// from journal/auth.go (which took it from auth/token.go); if you change it, change every copy.
//
// SIMULATION IS UNAFFECTED. A credential changes who the record belongs to; it does not create an
// order, a broker connection, or a money movement, and there is none anywhere in this service.

var b64 = base64.RawURLEncoding

// sessionPayload mirrors the auth service's claim set exactly.
type sessionPayload struct {
	UID string `json:"uid"`
	IAT int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

func signPayload(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return b64.EncodeToString(mac.Sum(nil))
}

// systemCookie mints a short-lived session for the engine's system user and returns it as a Cookie
// header value. Empty when no AUTH_SECRET is configured — in which case the engine behaves exactly as
// it does today (the journal refuses the write, the service keeps running, nothing else is affected).
//
// Minted per request rather than cached: the token is cheap, and a long-lived one sitting in memory
// for the lifetime of the process is a worse trade than a few microseconds of HMAC.
func (c *Clients) systemCookie() string {
	if c.cfg.AuthSecret == "" || c.cfg.SystemUID == "" {
		return ""
	}
	now := time.Now()
	raw, err := json.Marshal(sessionPayload{
		UID: c.cfg.SystemUID,
		IAT: now.Unix(),
		Exp: now.Add(10 * time.Minute).Unix(),
	})
	if err != nil {
		return ""
	}
	body := b64.EncodeToString(raw)
	return c.cfg.CookieName + "=" + body + "." + signPayload(c.cfg.AuthSecret, body)
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// SHARED VERIFIER — copy-pasted BYTE-IDENTICAL from auth/token.go (also in gateway/auth.go,
// journal/auth.go, alerts/auth.go and feedback/auth.go). A tiny pure verifier beats a shared module
// (these are independent stdlib-only Go modules). If you change it, change every copy.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

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

// requireSession gates a MUTATING route behind a valid session cookie.
//
// FAIL-CLOSED ON MISSING CONFIGURATION. With no AUTH_SECRET there is nothing to verify against, so
// the route is refused with 403 NAMING the missing variable — not left open, and not silently
// pretending to be authenticated. That is the same posture the rest of the stack takes: an absent
// credential means "cannot", never "may". GET routes are unaffected and stay public.
//
// This authenticates; it does not authorize per user. The engine keeps ONE system book (D-20 interim
// (a)), so "a signed-in user" is the right granularity until per-user engines land — at which point
// the resolved uid becomes the scope, which is why it is resolved rather than discarded.
func (a *API) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.AuthSecret == "" {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "this route is disabled: no AUTH_SECRET is configured for the paper service, " +
					"so no session can be verified",
				"missingConfiguration": "AUTH_SECRET",
			})
			return
		}
		c, err := r.Cookie(a.cfg.CookieName)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
			return
		}
		if _, err := parseToken(a.cfg.AuthSecret, c.Value); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
			return
		}
		next(w, r)
	}
}
