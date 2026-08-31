package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// google.go — real Google sign-in via the OAuth 2.0 Authorization Code flow, stdlib-only (net/http +
// encoding/json + crypto/rand; no golang.org/x/oauth2, invariant #5). It mints the SAME nvda_session
// HMAC cookie the rest of the app already trusts (setSessionCookie), so gateway/journal/alerts accept
// it unchanged — Google unlocks NO trading action, it only scopes data (invariant #2).
//
// Confidential-client flow: we exchange the code server-to-server with our client secret over TLS,
// then read sub/email/email_verified from Google's userinfo endpoint. Because that data comes straight
// from Google to our server (not via the browser), we don't hand-verify an id_token signature. CSRF is
// covered by a random `state` (+ PKCE S256 verifier) stored in a short-lived httpOnly cookie and
// required to match at the callback. Tokens/codes/secret are NEVER logged.

const (
	gStateCookie = "g_oauth_state"
	gStateTTL    = 10 * time.Minute
	gAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	gTokenURL    = "https://oauth2.googleapis.com/token"
	gUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
)

// googleHTTP is a small client with a timeout for the two server-to-server Google calls.
var googleHTTP = &http.Client{Timeout: 15 * time.Second}

// googleConfigured reports whether the real flow is wired: BOTH client id and secret must be set.
func (s *Server) googleConfigured() bool {
	return s.cfg.GoogleClientID != "" && s.cfg.GoogleClientSecret != ""
}

// gStateData is packed (base64url JSON) into the g_oauth_state cookie: the CSRF token, the PKCE
// verifier, and the sanitized post-login view to return to.
type gStateData struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
}

// viewIDRE constrains the returnTo hint to a view-id shape — no scheme, host, slash, or dot — so the
// post-login redirect (AppURL + "#" + view) can never become an open redirect.
var viewIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// sanitizeReturnTo strips a leading '#' and accepts only a known view-id shape; anything else (an
// absolute URL, a path, junk) falls back to "terminal". NEVER returns an attacker-controlled URL.
func sanitizeReturnTo(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "#")
	if viewIDRE.MatchString(v) {
		return v
	}
	return "terminal"
}

// handleGoogleConfig is a side-effect-free probe the button polls to choose "live" vs "coming soon".
func (s *Server) handleGoogleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"configured": s.googleConfigured()})
}

// handleGoogleStart (GET /auth/google) — 501 until configured; otherwise set the state cookie and
// 302 to Google's consent screen with state + PKCE S256.
func (s *Server) handleGoogleStart(w http.ResponseWriter, r *http.Request) {
	if !s.googleConfigured() {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "google sign-in not configured"})
		return
	}
	state := randToken(24)
	verifier := randToken(32) // PKCE code_verifier (base64url of 32 bytes = 43 chars, within 43..128)
	ret := sanitizeReturnTo(r.URL.Query().Get("returnTo"))

	blob, _ := json.Marshal(gStateData{State: state, Verifier: verifier, Return: ret})
	http.SetCookie(w, &http.Cookie{
		Name:     gStateCookie,
		Value:    b64.EncodeToString(blob),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode, // Lax (not Strict): the cookie must ride the top-level redirect back from Google
		Secure:   isHTTPS(r),
		MaxAge:   int(gStateTTL / time.Second),
	})

	q := url.Values{}
	q.Set("client_id", s.cfg.GoogleClientID)
	q.Set("redirect_uri", s.cfg.GoogleRedirectURL)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	q.Set("access_type", "online")
	q.Set("prompt", "select_account")
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	http.Redirect(w, r, gAuthURL+"?"+q.Encode(), http.StatusFound)
}

// handleGoogleCallback (GET /auth/google/callback) — validate state, exchange the code, read userinfo,
// require email_verified, link-or-create the account, mint the session cookie, and redirect back into
// the app. ANY failure redirects to AppURL?authError=1 — never a 500, never a token in a log.
func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	if !s.googleConfigured() {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "google sign-in not configured"})
		return
	}
	// Read + clear the state cookie regardless of outcome (one-shot).
	sd, haveState := s.readClearState(w, r)
	q := r.URL.Query()

	// User denied consent / Google returned an error / missing or mismatched state → fail closed.
	if q.Get("error") != "" || !haveState || q.Get("state") == "" || q.Get("state") != sd.State {
		s.googleFail(w, r)
		return
	}
	code := q.Get("code")
	if code == "" {
		s.googleFail(w, r)
		return
	}

	accessToken, err := s.googleExchange(r.Context(), code, sd.Verifier)
	if err != nil {
		s.googleFail(w, r)
		return
	}
	info, err := s.googleUserinfo(r.Context(), accessToken)
	if err != nil || info.Email == "" || !bool(info.EmailVerified) {
		s.googleFail(w, r) // require a Google-verified email
		return
	}

	u, err := s.store.FindOrLinkGoogleUser(info.Email, info.Sub, info.Name)
	if err != nil {
		s.googleFail(w, r)
		return
	}
	s.setSessionCookie(w, r, u.ID, true) // same HMAC cookie as email/password; remember=true
	http.Redirect(w, r, s.appRedirect("#"+sanitizeReturnTo(sd.Return)), http.StatusFound)
}

// googleFail redirects the browser back to the app with a generic error flag — no details leak.
func (s *Server) googleFail(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, s.appRedirect("?authError=1"), http.StatusFound)
}

// appRedirect builds AppURL + suffix (suffix is "#view" on success or "?authError=1" on failure).
func (s *Server) appRedirect(suffix string) string {
	return strings.TrimRight(s.cfg.AppURL, "/") + suffix
}

// readClearState decodes the g_oauth_state cookie and always clears it (single-use). false if absent
// or undecodable.
func (s *Server) readClearState(w http.ResponseWriter, r *http.Request) (gStateData, bool) {
	http.SetCookie(w, &http.Cookie{
		Name: gStateCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r), MaxAge: -1,
	})
	var sd gStateData
	c, err := r.Cookie(gStateCookie)
	if err != nil {
		return sd, false
	}
	raw, err := b64.DecodeString(c.Value)
	if err != nil {
		return sd, false
	}
	if err := json.Unmarshal(raw, &sd); err != nil || sd.State == "" {
		return sd, false
	}
	return sd, true
}

// googleExchange POSTs the code to Google's token endpoint (server-to-server, with our secret + PKCE
// verifier) and returns the access token. Errors are generic — the response body (which carries
// tokens) is never logged or surfaced.
func (s *Server) googleExchange(ctx context.Context, code, verifier string) (string, error) {
	form := url.Values{}
	form.Set("client_id", s.cfg.GoogleClientID)
	form.Set("client_secret", s.cfg.GoogleClientSecret)
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", s.cfg.GoogleRedirectURL)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := googleHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token exchange failed: status %d", resp.StatusCode)
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
		return "", errors.New("token exchange: no access_token")
	}
	return t.AccessToken, nil
}

// flexBool decodes a JSON value that may be a real boolean or a quoted string ("true"). Google's
// userinfo returns email_verified as a boolean, but some responses have historically quoted it.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	*b = flexBool(strings.Trim(string(data), `"`) == "true")
	return nil
}

type googleInfo struct {
	Sub           string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified flexBool `json:"email_verified"`
	Name          string   `json:"name"`
}

// googleUserinfo reads the profile with the access token. Generic errors; token never logged.
func (s *Server) googleUserinfo(ctx context.Context, accessToken string) (googleInfo, error) {
	var info googleInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gUserInfoURL, nil)
	if err != nil {
		return info, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := googleHTTP.Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("userinfo failed: status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return info, err
	}
	return info, nil
}

// randToken returns n cryptographically-random bytes as base64url (no padding) — used for both the
// CSRF state and the PKCE code_verifier.
func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b64.EncodeToString(b)
}

// pkceChallenge is base64url(SHA-256(verifier)) — the S256 code_challenge.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return b64.EncodeToString(sum[:])
}
