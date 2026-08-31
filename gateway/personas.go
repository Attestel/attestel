package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// personas.go — thin proxy for the chat "instructor" personas (Gemini-Gem-style custom instructions)
// that live in the llm service. Personas only shape the grounded chat's role/tone/teaching style; the
// llm keeps its hard grounding rules regardless (invariant #2 — never a prediction or a buy/sell call).
//
// The generic get/postJSON helpers flatten upstream 4xx into an opaque error, but persona CRUD needs
// to surface validation status/messages to the UI, so these handlers forward the request verbatim and
// mirror the llm's status + body back to the client. Not cached — the list is small and changes on edit.

// proxyLLM forwards the current request (method + body) to a target llm URL and copies the upstream
// status, content-type, and body back to the client. It stamps the VERIFIED identity as X-User-Id
// (empty for a guest) so the llm scopes personas per user — the llm isn't browser-exposed on this
// path, so it trusts the header. Fails soft: a 502 JSON error if the llm is down.
func (s *Server) proxyLLM(w http.ResponseWriter, r *http.Request, method, url, userID string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var body io.Reader
	if r.Body != nil {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // cap at 1 MiB — persona text is tiny
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", userID) // gateway-verified; the llm never sees a client-set value

	resp, err := s.http.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "personas service unavailable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(payload)
}

// handlePersonas serves the collection: GET (list built-ins + the user's personas) and POST
// (create). GET is open to guests (built-ins only); creating requires a signed-in user.
func (s *Server) handlePersonas(w http.ResponseWriter, r *http.Request) {
	userID := s.userIDFrom(r)
	if r.Method == http.MethodPost && userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in to create a persona"})
		return
	}
	s.proxyLLM(w, r, r.Method, s.cfg.LLMURL+"/personas", userID)
}

// handlePersonaItem serves a single user persona by id: PATCH (edit) and DELETE. Both mutate, so
// both require a signed-in user (the guest UI routes to sign-in before reaching here; 401 is the
// backstop).
func (s *Server) handlePersonaItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "persona id required"})
		return
	}
	userID := s.userIDFrom(r)
	if userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in to edit personas"})
		return
	}
	s.proxyLLM(w, r, r.Method, s.cfg.LLMURL+"/personas/"+id, userID)
}
