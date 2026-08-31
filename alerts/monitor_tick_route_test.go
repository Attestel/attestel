package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// monitor_tick_route_test.go — Phase 1's on-demand sweep trigger.
//
// The route exists so an operator-invoked automation runner can drive ONE deterministic sweep and
// record it durably, instead of the sweep existing only inside this service's own optional timer.
// The three things that must remain true of it are asserted here: it is internal-only, it honours
// the monitor's own enable flag rather than routing around it, and it drives the SAME Tick the
// timer drives (no second code path, and therefore no second place for the model-free rule to be
// broken).

func tickAPI(t *testing.T) (*API, *ThesisMonitor) {
	t.Helper()
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false), nil)
	api := newAPI(m.store, m.cfg)
	api.monitor = m
	return api, m
}

func TestMonitorTickRouteRequiresTheInternalSecretAndRefusesBrowsers(t *testing.T) {
	api, m := tickAPI(t)

	anonymous := httptest.NewRequest(http.MethodPost, "/_internal/monitor/tick", nil)
	w := httptest.NewRecorder()
	api.routes().ServeHTTP(w, anonymous)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("without secret status=%d, want 401", w.Code)
	}

	browser := httptest.NewRequest(http.MethodPost, "/_internal/monitor/tick", nil)
	browser.Header.Set("X-Internal-Secret", m.cfg.Secret)
	browser.AddCookie(&http.Cookie{Name: m.cfg.CookieName, Value: "anything"})
	w = httptest.NewRecorder()
	api.routes().ServeHTTP(w, browser)
	if w.Code != http.StatusNotFound {
		t.Fatalf("browser-shaped request status=%d, want 404", w.Code)
	}
}

func TestMonitorTickRouteHonoursTheDisableFlag(t *testing.T) {
	api, m := tickAPI(t)
	m.cfg.MonitorEnabled = false
	api.cfg.MonitorEnabled = false

	req := httptest.NewRequest(http.MethodPost, "/_internal/monitor/tick", nil)
	req.Header.Set("X-Internal-Secret", m.cfg.Secret)
	w := httptest.NewRecorder()
	api.routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["ran"] != false {
		t.Fatalf("a disabled monitor must not sweep: %v", body)
	}
	if body["flag"] != "THESIS_MONITOR_ENABLED" {
		t.Fatalf("the refusal must name the flag that would enable it: %v", body)
	}
}

func TestMonitorTickRouteRunsOneSweepAndReportsIt(t *testing.T) {
	api, m := tickAPI(t)

	req := httptest.NewRequest(http.MethodPost, "/_internal/monitor/tick", nil)
	req.Header.Set("X-Internal-Secret", m.cfg.Secret)
	w := httptest.NewRecorder()
	api.routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["ran"] != true {
		t.Fatalf("an enabled monitor must sweep: %v", body)
	}
	if _, ok := body["tick"].(map[string]any); !ok {
		t.Fatalf("the response must carry the sweep summary: %v", body)
	}
	if _, ok := body["queue"].(map[string]any); !ok {
		t.Fatalf("the response must carry the queue depth: %v", body)
	}
	// The sweep is recorded exactly as the timer's would be — same Tick, same state.
	if m.LastTick().At == 0 {
		t.Fatalf("the manual sweep must be recorded like any other tick")
	}
}
