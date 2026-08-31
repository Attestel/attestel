package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func internalResynthFixture(t *testing.T) (*Server, *http.ServeMux, Thesis) {
	t.Helper()
	store, err := openThesisStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	created, apiErr := store.Create("u1", Thesis{
		Ticker: "NVDA", Claim: "Revenue growth depends on accelerator demand.",
		Text: "Revenue growth depends on accelerator demand.", Status: statusActive,
	}, 1_780_000_000)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	srv := &Server{cfg: Config{Secret: "test-secret", CookieName: "nvda_session"}, theses: store}
	mux := http.NewServeMux()
	srv.registerInternalThesesAPI(mux, store, nil)
	return srv, mux, created
}

func TestInternalResynthReadRequiresSecretAndExactOwner(t *testing.T) {
	srv, mux, thesis := internalResynthFixture(t)
	path := "/_internal/thesis-resynth/" + thesis.ID + "?userId=u1"

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("without secret status=%d", w.Code)
	}

	browser := httptest.NewRequest(http.MethodGet, path, nil)
	browser.Header.Set("X-Internal-Secret", srv.cfg.Secret)
	browser.AddCookie(&http.Cookie{Name: srv.cfg.CookieName, Value: "session"})
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, browser)
	if w.Code != http.StatusNotFound {
		t.Fatalf("browser-shaped request status=%d", w.Code)
	}

	wrongOwner := httptest.NewRequest(http.MethodGet,
		"/_internal/thesis-resynth/"+thesis.ID+"?userId=u2", nil)
	wrongOwner.Header.Set("X-Internal-Secret", srv.cfg.Secret)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, wrongOwner)
	if w.Code != http.StatusNotFound {
		t.Fatalf("wrong owner status=%d", w.Code)
	}

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Internal-Secret", srv.cfg.Secret)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	got := body["thesis"].(map[string]any)
	if got["claim"] != thesis.Claim {
		t.Fatalf("claim=%v", got["claim"])
	}
}

func TestInternalResynthPatchCanOnlyWriteLastCheck(t *testing.T) {
	srv, mux, thesis := internalResynthFixture(t)
	body := `{"lastCheck":{"at":1800000000,"verdict":"challenged","summary":"New facts challenge the claim.",` +
		`"confidence":72,"evidence":[],"watchFor":["Next reported quarter"],"modelUsed":"qwen3-14b"}}`
	req := httptest.NewRequest(http.MethodPatch,
		"/_internal/thesis-resynth/"+thesis.ID+"?userId=u1", bytes.NewBufferString(body))
	req.Header.Set("X-Internal-Secret", srv.cfg.Secret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", w.Code, w.Body.String())
	}

	got, ok := srv.theses.Get("u1", thesis.ID)
	if !ok || got.LastCheck == nil || got.LastCheck.Verdict != "challenged" {
		t.Fatalf("lastCheck=%+v", got.LastCheck)
	}
	if got.Claim != thesis.Claim || got.UpdatedAt != thesis.UpdatedAt || len(got.Versions) != 0 {
		t.Fatalf("system check mutated user-owned/versioned fields: %+v", got)
	}
}

func TestInternalResynthPatchUsesNormalCheckValidation(t *testing.T) {
	srv, mux, thesis := internalResynthFixture(t)
	req := httptest.NewRequest(http.MethodPatch,
		"/_internal/thesis-resynth/"+thesis.ID+"?userId=u1",
		bytes.NewBufferString(`{"lastCheck":{"verdict":"bullish","summary":"invalid"}}`))
	req.Header.Set("X-Internal-Secret", srv.cfg.Secret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid verdict status=%d body=%s", w.Code, w.Body.String())
	}
}
