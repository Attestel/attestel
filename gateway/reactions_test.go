package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// reactions_test.go — Phase 4's read-through. The property that matters is the failure mode: an
// aggregate the gateway could not read must never be renderable as a percentage.

func reactionRequest(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	mux := http.NewServeMux()
	srv.registerEventRoutes(mux)
	mux.ServeHTTP(w, req)
	return w
}

func TestReactionsReadIsStoreBackedAndCallsNoProvider(t *testing.T) {
	srv, transport := automationServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reactions" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"reactions": []map[string]any{{
				"eventId": "sch_1", "ticker": "NVDA", "session": "after_market",
				"referenceTs": "2026-08-19", "referenceClose": 100.0,
				"calcVersion": "reaction@1", "synthetic": false,
				"windows": []map[string]any{{
					"horizon": "1d", "state": "resolved", "rawReturn": 0.1,
					"benchmarkReturn": 0.05, "excessReturn": 0.05, "barSource": "yfinance",
				}},
			}},
			"horizons": []string{"1d", "5d", "20d"},
		})
	})

	w := reactionRequest(t, srv, "/api/reactions?eventId=sch_1")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	rows, _ := body["reactions"].([]any)
	if len(rows) != 1 {
		t.Fatalf("reactions = %v", body["reactions"])
	}
	for _, host := range transport.attempted() {
		if host != "" && !transport.allow[host] {
			t.Fatalf("a reaction read reached a provider host: %s", host)
		}
	}
}

func TestReactionsReadRequiresASubject(t *testing.T) {
	srv, _ := automationServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	if code := reactionRequest(t, srv, "/api/reactions").Code; code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", code)
	}
}

func TestSensitivityPassesTheStoresRefusalThrough(t *testing.T) {
	srv, _ := automationServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sensitivity" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"sufficient": false, "reason": "insufficient history",
			"sampleCount": 3, "minimumSample": 12, "shortBy": 9,
			"raw": nil, "excess": nil,
		})
	})

	w := reactionRequest(t, srv, "/api/sensitivity?ticker=NVDA&horizon=1d")
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["sufficient"] != false {
		t.Fatalf("the refusal must survive the proxy: %v", body)
	}
	if body["raw"] != nil {
		t.Fatalf("a refused aggregate must carry no statistic: %v", body["raw"])
	}
}

func TestAnUnreadableSensitivityIsInsufficientNotBlank(t *testing.T) {
	srv, _ := automationServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})

	w := reactionRequest(t, srv, "/api/sensitivity?ticker=NVDA")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["sufficient"] != false {
		t.Fatalf("an unreadable aggregate must not read as sufficient: %v", body)
	}
	if body["raw"] != nil || body["sampleCount"] != nil {
		t.Fatalf("no number may survive an unreadable upstream: %v", body)
	}
	degraded, _ := body["degraded"].([]any)
	if len(degraded) == 0 {
		t.Fatalf("the failure must be stated: %v", body)
	}
}

func TestOnlyAllowlistedParametersReachTheStore(t *testing.T) {
	var seen string
	srv, _ := automationServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RawQuery
		writeJSON(w, http.StatusOK, map[string]any{"sufficient": false})
	})
	reactionRequest(t, srv, "/api/sensitivity?ticker=NVDA&horizon=5d&limit=99999&secret=x")
	if seen == "" {
		t.Fatalf("the store was never reached")
	}
	for _, forbidden := range []string{"secret", "limit"} {
		if contains(seen, forbidden) {
			t.Fatalf("an unallowlisted parameter reached the store: %s in %s", forbidden, seen)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
