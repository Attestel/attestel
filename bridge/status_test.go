package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// status_test.go — `Status` fails closed.
//
// This is the call `-check` rests on, so what it accepts is what "configured correctly" means. It
// used to treat an absent schema version as "fine, carry on", which meant a response from something
// that was not this API at all — a login page, a proxy error rendered as JSON, another service on
// the same host — could satisfy the preflight by simply not mentioning the fields it was being
// checked on. Every field is now required.

// statusServer answers /_internal/agency/status with `body` and returns a client pointed at it.
func statusServer(t *testing.T, body any) *apiClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_internal/agency/status" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Worker-Token") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return newAPIClient(Config{BaseURL: srv.URL, Token: "a-token"})
}

// healthyStatus is the response a correctly configured deployment returns.
func healthyStatus() map[string]any {
	return map[string]any{
		"ok":                    true,
		"workflows":             []string{workflowCompanyResearch},
		"jobSchemaVersion":      jobSchemaVersion,
		"artifactSchemaVersion": artifactSchemaVersion,
		"queuedRuns":            2,
		"maxLeaseSeconds":       serverMaxLeaseSeconds,
	}
}

func TestStatusAcceptsACompleteHealthyResponse(t *testing.T) {
	// The control. Without it, every rejection below could be passing for the wrong reason.
	got, err := statusServer(t, healthyStatus()).Status(context.Background())
	if err != nil {
		t.Fatalf("a complete, healthy status response was rejected: %v", err)
	}
	if got.QueuedRuns == nil || *got.QueuedRuns != 2 ||
		got.MaxLeaseSeconds == nil || *got.MaxLeaseSeconds != serverMaxLeaseSeconds {
		t.Fatalf("status did not carry the response through: %+v", got)
	}
}

func TestStatusFailsClosedOnAnythingMissingOrWrong(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{"ok is false", func(b map[string]any) { b["ok"] = false }, "available"},
		{"ok is absent", func(b map[string]any) { delete(b, "ok") }, "available"},
		{"the job schema version is absent", func(b map[string]any) { delete(b, "jobSchemaVersion") }, "did not state"},
		{"the job schema version is empty", func(b map[string]any) { b["jobSchemaVersion"] = "" }, "did not state"},
		{"the artifact schema version is absent", func(b map[string]any) { delete(b, "artifactSchemaVersion") }, "did not state"},
		{"the artifact schema version is empty", func(b map[string]any) { b["artifactSchemaVersion"] = "" }, "did not state"},
		{"a different job schema version", func(b map[string]any) { b["jobSchemaVersion"] = "attestel.agency.job/99" }, "issues"},
		{"a different artifact schema version", func(b map[string]any) { b["artifactSchemaVersion"] = "attestel.agency.artifact/99" }, "expects"},
		{"workflows is absent", func(b map[string]any) { delete(b, "workflows") }, "no workflows"},
		{"workflows is empty", func(b map[string]any) { b["workflows"] = []string{} }, "no workflows"},
		{"a different workflow", func(b map[string]any) { b["workflows"] = []string{"something_else_v1"} }, "does not offer"},
		{"maxLeaseSeconds is absent", func(b map[string]any) { delete(b, "maxLeaseSeconds") }, "lease ceiling"},
		{"maxLeaseSeconds is zero", func(b map[string]any) { b["maxLeaseSeconds"] = 0 }, "lease ceiling"},
		{"maxLeaseSeconds is negative", func(b map[string]any) { b["maxLeaseSeconds"] = -1 }, "lease ceiling"},
		{"queuedRuns is absent", func(b map[string]any) { delete(b, "queuedRuns") }, "how many runs are queued"},
		{"queuedRuns is negative", func(b map[string]any) { b["queuedRuns"] = -1 }, "negative queue depth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := healthyStatus()
			tc.mutate(body)
			_, err := statusServer(t, body).Status(context.Background())
			if err == nil {
				t.Fatalf("%s was accepted; the preflight must fail closed", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%s reported %q, expected it to mention %q", tc.name, err, tc.wantErr)
			}
		})
	}
}

func TestStatusRejectsAResponseThatIsNotThisAPIAtAll(t *testing.T) {
	// The realistic misconfiguration: the URL reaches SOMETHING that answers 200 with JSON. An
	// empty object mentions none of the fields, which is exactly the case the old version passed.
	for _, body := range []any{
		map[string]any{},
		map[string]any{"status": "ok"},
		map[string]any{"error": "not found"},
	} {
		if _, err := statusServer(t, body).Status(context.Background()); err == nil {
			t.Fatalf("a foreign 200 response satisfied the preflight: %v", body)
		}
	}
}

func TestStatusPropagatesATransportOrAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"worker authentication required"}`))
	}))
	t.Cleanup(srv.Close)
	client := newAPIClient(Config{BaseURL: srv.URL, Token: "wrong"})
	if _, err := client.Status(context.Background()); err == nil {
		t.Fatal("a 401 satisfied the preflight")
	}
}
