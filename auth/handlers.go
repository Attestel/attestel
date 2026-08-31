package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// handlers.go — the HTTP surface for accounts. Sessions ride in an httpOnly cookie, so CORS must
// reflect a specific allowed Origin (never "*") and send Access-Control-Allow-Credentials: true.
// Security hygiene: passwords are never logged, login returns a GENERIC error (no email enumeration),
// and the session token is opaque + HMAC-signed (token.go).

type Server struct {
	cfg      Config
	store    *Store
	settings *SettingsStore
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /auth/signup", s.handleSignup)
	mux.HandleFunc("POST /auth/login", s.handleLogin)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /auth/me", s.handleMe)
	// Per-user preferences (directional-signal auto train+refresh cadence + allow-short / horizon
	// defaults). Both require a session (401 backstop) — settings are per-user like every other write.
	mux.HandleFunc("GET /auth/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /auth/settings", s.handlePutSettings)
	// Google OAuth (Authorization Code flow, google.go). Deferred behind GOOGLE_CLIENT_ID +
	// GOOGLE_CLIENT_SECRET: /config reports whether it's wired (the button polls it), /google starts
	// the flow, /callback finishes it. All three 501 / report false until both keys are set.
	mux.HandleFunc("GET /auth/google/config", s.handleGoogleConfig)
	mux.HandleFunc("GET /auth/google", s.handleGoogleStart)
	mux.HandleFunc("GET /auth/google/callback", s.handleGoogleCallback)
	return s.withCORS(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	storage := "files"
	if s.store.db != nil {
		storage = "postgresql"
		if err := s.store.db.PingContext(r.Context()); err != nil {
			log.Printf("auth storage health: %v", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "service": "auth"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "auth", "storage": storage})
}

func accountInputError(err error) bool {
	return errors.Is(err, errDuplicateEmail) || errors.Is(err, errInvalidEmail) || errors.Is(err, errShortPassword)
}

func writeAuthStoreError(w http.ResponseWriter, err error) {
	log.Printf("auth storage: %v", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "account storage is unavailable"})
}

type credsBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Remember bool   `json:"remember"`
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	var body credsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	u, err := s.store.CreateUser(body.Email, body.Password)
	if err != nil {
		// Validation / duplicate messages are safe to surface (they're about the submitted data).
		if accountInputError(err) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		} else {
			writeAuthStoreError(w, err)
		}
		return
	}
	s.setSessionCookie(w, r, u.ID, body.Remember)
	writeJSON(w, http.StatusCreated, map[string]any{"user": u.public()})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body credsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	// GENERIC error on any failure — never reveal whether the email exists (no user enumeration).
	// Still run verifyPassword against a real-or-throwaway hash so timing doesn't leak existence.
	u, ok, err := s.store.LookupByEmail(body.Email)
	if err != nil {
		verifyPassword(dummyHash, body.Password)
		writeAuthStoreError(w, err)
		return
	}
	valid := ok && verifyPassword(u.PasswordHash, body.Password)
	if !ok {
		verifyPassword(dummyHash, body.Password) // constant-ish work for a missing user
	}
	if !valid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid email or password"})
		return
	}
	s.setSessionCookie(w, r, u.ID, body.Remember)
	writeJSON(w, http.StatusOK, map[string]any{"user": u.public()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// handleMe resolves the cookie to the current user. NEVER 401 — a guest is a valid state
// ({user:null}); the frontend hydrates from this on load.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	uid := s.userIDFrom(r)
	if uid == "" {
		writeJSON(w, http.StatusOK, map[string]any{"user": nil})
		return
	}
	u, ok, err := s.store.LookupByID(uid)
	if err != nil {
		writeAuthStoreError(w, err)
		return
	}
	if !ok {
		// Valid signature but the user was removed — treat as a guest and clear the stale cookie.
		s.clearSessionCookie(w, r)
		writeJSON(w, http.StatusOK, map[string]any{"user": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u.public()})
}

// handleGetSettings returns the signed-in user's saved preferences (defaults if never saved). Unlike
// handleMe it 401s for a guest: settings are per-user and require a session (a UI backstop — the
// frontend routes guests to sign-in before ever calling this).
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	uid := s.userIDFrom(r)
	if uid == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	settings, err := s.settings.ReadSettings(uid)
	if err != nil {
		writeAuthStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

// handlePutSettings validates + persists the signed-in user's preferences. 401 for a guest, 400 on
// any out-of-set value (interval/horizon are bounded presets — see validateSettings).
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	uid := s.userIDFrom(r)
	if uid == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	var in Settings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	if err := s.settings.PutSettings(uid, in); err != nil {
		if errors.Is(err, errInvalidSettings) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		} else {
			writeAuthStoreError(w, err)
		}
		return
	}
	settings, err := s.settings.ReadSettings(uid)
	if err != nil {
		writeAuthStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

// ---- cookies + identity ----

// userIDFrom reads the session cookie and verifies it locally. "" for a guest / invalid / expired.
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

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, uid string, remember bool) {
	ttl := s.cfg.SessionTTL
	if remember {
		ttl = s.cfg.RememberTTL
	}
	c := &http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    issueToken(s.cfg.Secret, uid, ttl),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	}
	if remember {
		// Persistent cookie. Without "remember" we set no Max-Age -> a SESSION cookie the browser
		// drops when it closes (but a page refresh keeps it, and the token is still valid ~SessionTTL).
		c.MaxAge = int(s.cfg.RememberTTL / time.Second)
	}
	http.SetCookie(w, c)
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   -1,
	})
}

// isHTTPS marks the cookie Secure when the request arrived over TLS (directly or via a proxy).
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// withCORS reflects an allow-listed Origin and enables credentials (cookies) — NOT a wildcard,
// which browsers forbid alongside credentials. Answers OPTIONS preflight.
func (s *Server) withCORS(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range s.cfg.CORSOrigins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
