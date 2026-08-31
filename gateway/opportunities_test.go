package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpportunitiesReadsStoredSnapshotWithoutCallingModelOrPrediction(t *testing.T) {
	events := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/opportunities" || r.URL.Query().Get("limit") != "12" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"runId":"opp_1","detectorVersion":"early-opportunity@2",` +
			`"coverage":{"state":"ok"},"candidates":[{"ticker":"NVDA","state":"emerging"}],` +
			`"degraded":[]}`))
	}))
	defer events.Close()

	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		http.Error(w, "must not be reached", http.StatusInternalServerError)
	}))
	defer model.Close()

	cfg := loadConfig()
	cfg.EventsURL = events.URL
	cfg.LLMURL = model.URL
	cfg.PredictionURL = model.URL
	srv := &Server{cfg: cfg, cache: newCache(), http: http.DefaultClient}
	mux := http.NewServeMux()
	srv.registerEventRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/opportunities?limit=12", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"emerging"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if modelCalls != 0 {
		t.Fatalf("opportunity read made %d model/prediction calls", modelCalls)
	}
}

func TestOpportunitiesFailsClosedWhenEventsIsUnavailable(t *testing.T) {
	cfg := loadConfig()
	cfg.EventsURL = "http://127.0.0.1:1"
	srv := &Server{cfg: cfg, cache: newCache(), http: http.DefaultClient}
	mux := http.NewServeMux()
	srv.registerEventRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/opportunities", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"candidates":[]`) ||
		!strings.Contains(rec.Body.String(), degradedEventsUnreachable) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
