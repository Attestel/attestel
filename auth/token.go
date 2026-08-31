package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// token.go — the session token: a signed, opaque, stateless value carried in an httpOnly cookie.
//
//	token = base64url(payloadJSON) + "." + base64url(HMAC-SHA256(secret, payloadb64))
//
// A single AUTH_SECRET (shared by every service via env) signs and verifies it, so each service can
// authenticate the SAME cookie LOCALLY with no network hop — the browser talks to several services
// directly and all localhost ports are the same site, so one cookie covers them.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────────
// SHARED VERIFIER — the block below (b64, sessionPayload, signPayload, parseToken) is copy-pasted
// BYTE-IDENTICAL into gateway/auth.go, journal/auth.go, alerts/auth.go and feedback/auth.go. This
// mirrors the repo's existing "seed data is duplicated on purpose" pragmatism: a tiny pure verifier
// beats a shared module (the gateway is stdlib-only Go with zero deps — invariant #5). If you change
// it, change every copy. issueToken() below is auth-only (only this service mints tokens).
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

// issueToken mints a signed session token for uid, valid for ttl. auth-only.
func issueToken(secret, uid string, ttl time.Duration) string {
	now := time.Now()
	raw, _ := json.Marshal(sessionPayload{UID: uid, IAT: now.Unix(), Exp: now.Add(ttl).Unix()})
	body := b64.EncodeToString(raw)
	return body + "." + signPayload(secret, body)
}
