package main

// models.go — authenticated lifecycle surface for immutable prediction model versions.

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (s *Server) registerModelRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/prediction-automation/status", s.handlePredictionAutomationStatus)
	mux.HandleFunc("GET /api/models/{ticker}", s.handleModelStatus)
	mux.HandleFunc("POST /api/models/{ticker}/{version}/promote", s.handleModelPromote)
	mux.HandleFunc("POST /api/models/{ticker}/{version}/rollback", s.handleModelRollback)
}

func (s *Server) handlePredictionAutomationStatus(w http.ResponseWriter, r *http.Request) {
	if s.userIDFrom(r) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	s.proxyPrediction(w, r, http.MethodGet, "/automation/status", 20*time.Second)
}

func (s *Server) handleModelStatus(w http.ResponseWriter, r *http.Request) {
	if s.userIDFrom(r) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	ticker := strings.ToUpper(r.PathValue("ticker"))
	s.proxyPrediction(w, r, http.MethodGet, withQuery("/models/"+url.PathEscape(ticker), r), 20*time.Second)
}

func (s *Server) handleModelPromote(w http.ResponseWriter, r *http.Request) {
	s.handleModelDeployment(w, r, "promote")
}

func (s *Server) handleModelRollback(w http.ResponseWriter, r *http.Request) {
	s.handleModelDeployment(w, r, "rollback")
}

func (s *Server) handleModelDeployment(w http.ResponseWriter, r *http.Request, action string) {
	uid := s.userIDFrom(r)
	if uid == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	if !s.isEvalAdmin(uid) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "evaluation admin access required — this deployment's EVAL_ADMIN_UIDS " +
				"allow-list does not include your user id",
		})
		return
	}
	ticker := strings.ToUpper(r.PathValue("ticker"))
	version := r.PathValue("version")
	path := "/models/" + url.PathEscape(ticker) + "/" + url.PathEscape(version) + "/" + action
	s.proxyPrediction(w, r, http.MethodPost, withQuery(path, r), 20*time.Second)
	// A failed deployment only causes a harmless cache miss. A successful one must not leave the old
	// active model visible for PredictTTL after the deployment pointer changed.
	s.cache.deletePrefix("predict:"+ticker+":", "dashboard:"+ticker+":")
}
