package main

import (
	"net/http"
	"testing"
	"time"
)

func modelGateway(t *testing.T, up *evalUpstream, admins []string) (*Server, http.Handler) {
	t.Helper()
	cfg := loadConfig()
	cfg.PredictionURL = up.srv.URL
	cfg.Secret = "test-secret"
	cfg.CookieName = "nvda_session"
	cfg.EvalAdminUIDs = admins
	srv := &Server{cfg: cfg, cache: newCache(), http: &http.Client{Timeout: 5 * time.Second}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/train/{ticker}", srv.handleTrain)
	srv.registerModelRoutes(mux)
	return srv, mux
}

func TestTrainingIsFailClosedBehindTheEvaluatorAdminBoundary(t *testing.T) {
	up := newEvalUpstream(t)
	srv, h := modelGateway(t, up, []string{"admin-1"})

	code, _, _ := callEval(t, srv, h, http.MethodPost, "/api/train/NVDA?timeframe=1D", "", "")
	if code != http.StatusUnauthorized || up.hits != 0 {
		t.Fatalf("guest train: status=%d upstreamHits=%d, want 401/0", code, up.hits)
	}
	code, _, _ = callEval(t, srv, h, http.MethodPost, "/api/train/NVDA?timeframe=1D", "member", "")
	if code != http.StatusForbidden || up.hits != 0 {
		t.Fatalf("member train: status=%d upstreamHits=%d, want 403/0", code, up.hits)
	}
	code, _, _ = callEval(t, srv, h, http.MethodPost, "/api/train/NVDA?timeframe=1D", "admin-1", "")
	if code != http.StatusAccepted || up.lastURI != "/train/NVDA?timeframe=1D" {
		t.Fatalf("admin train: status=%d uri=%q", code, up.lastURI)
	}
}

func TestPromotionStampsVerifiedActorAndInvalidatesServingCaches(t *testing.T) {
	up := newEvalUpstream(t)
	up.status = http.StatusOK
	up.body = `{"ok":true}`
	srv, h := modelGateway(t, up, []string{"admin-1"})
	srv.cache.set("predict:NVDA:timeframe=1D", []byte("old"), time.Hour)
	srv.cache.set("dashboard:NVDA:1D", []byte("old"), time.Hour)

	code, _, _ := callEval(
		t, srv, h, http.MethodPost,
		"/api/models/NVDA/m1/promote?timeframe=1D&horizon=5", "admin-1", `{"reason":"reviewed"}`,
	)
	if code != http.StatusOK || up.lastAct != "admin-1" {
		t.Fatalf("promotion: status=%d actor=%q", code, up.lastAct)
	}
	if _, ok := srv.cache.get("predict:NVDA:timeframe=1D"); ok {
		t.Error("prediction cache survived a deployment pointer change")
	}
	if _, ok := srv.cache.get("dashboard:NVDA:1D"); ok {
		t.Error("dashboard cache survived a deployment pointer change")
	}
}

func TestModelStatusRequiresOnlyASignedInSession(t *testing.T) {
	up := newEvalUpstream(t)
	srv, h := modelGateway(t, up, nil)
	code, _, _ := callEval(t, srv, h, http.MethodGet, "/api/models/NVDA?timeframe=1D", "", "")
	if code != http.StatusUnauthorized || up.hits != 0 {
		t.Fatalf("guest status: status=%d hits=%d", code, up.hits)
	}
	code, _, _ = callEval(t, srv, h, http.MethodGet, "/api/models/NVDA?timeframe=1D", "member", "")
	if code != http.StatusAccepted || up.lastURI != "/models/NVDA?timeframe=1D" {
		t.Fatalf("member status: status=%d uri=%q", code, up.lastURI)
	}
}

func TestPredictionAutomationStatusIsReadOnlyAndRequiresSession(t *testing.T) {
	up := newEvalUpstream(t)
	srv, h := modelGateway(t, up, nil)
	code, _, _ := callEval(t, srv, h, http.MethodGet, "/api/prediction-automation/status", "", "")
	if code != http.StatusUnauthorized || up.hits != 0 {
		t.Fatalf("guest automation status: status=%d hits=%d, want 401/0", code, up.hits)
	}
	code, _, _ = callEval(t, srv, h, http.MethodGet, "/api/prediction-automation/status", "member", "")
	if code != http.StatusAccepted || up.lastMth != http.MethodGet || up.lastURI != "/automation/status" {
		t.Fatalf("member automation status: status=%d method=%q uri=%q", code, up.lastMth, up.lastURI)
	}
}
