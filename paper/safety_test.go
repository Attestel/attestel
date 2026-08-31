package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenStoreRefusesCorruptDurableState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(dir, []PaperCfg{testCfg}); err == nil || !strings.Contains(err.Error(), "decode paper state") {
		t.Fatalf("corrupt state must stop initialization, got %v", err)
	}
}

func TestFailedStoreMutationRollsBackMemoryAndReturnsError(t *testing.T) {
	dir := t.TempDir()
	store, err := openStore(dir, []PaperCfg{testCfg})
	if err != nil {
		t.Fatal(err)
	}
	// A regular file cannot contain state.json beneath it, producing a deterministic ENOTDIR.
	store.dir = filepath.Join(dir, "state.json")
	next := []PaperCfg{{Ticker: "GOOGL", Timeframe: "1D", Horizon: 5}}
	if err := store.SetConfigs(next); err == nil {
		t.Fatal("a non-durable config update must return an error")
	}
	if got := store.Configs(); len(got) != 1 || got[0].Key() != testCfg.Key() {
		t.Fatalf("the failed write must roll memory back, got %+v", got)
	}
}

func TestResetNeverReportsSuccessWhenLedgerResetFails(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	f := newFakes(t, now)
	e, store := harness(t, f, dir)
	e.evalConfig(context.Background(), testCfg, now)

	cfg := e.cfg
	cfg.AuthSecret, cfg.CookieName, cfg.SystemUID = "s", "nvda_session", "paper-engine"
	api := newAPI(cfg, store, e.clients, e)
	api.now = func() time.Time { return now }
	// Point the ledger under a regular file. Reset's preflight now fails deterministically while its
	// in-memory lot remains intact.
	e.ledger.dir = store.path()
	req := httptest.NewRequest(http.MethodPost, "/paper/reset?confirm=true", nil)
	req.AddCookie(sessionCookie("s", cfg.SystemUID, time.Now().Add(time.Hour)))
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("ledger reset failure must be 500, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != false || body["engineStateReset"] != false || body["ledgerReset"] != false {
		t.Fatalf("a partial reset must never claim an official start: %+v", body)
	}
	if store.StateFor(testCfg).Side != "long" || !e.ledger.HasLot(testCfg.Key()) {
		t.Fatal("engine and ledger memory must remain intact when ledger reset cannot commit")
	}
}

func TestConfigMutationWaitsForEngineOperation(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	api := newAPI(e.cfg, store, e.clients, e)

	e.opMu.Lock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		api.handleSetConfig(rec, httptest.NewRequest(http.MethodPost, "/paper/config",
			strings.NewReader(`{"configs":"NVDA:1D:5,GOOGL:1D:5"}`)))
		done <- rec
	}()
	select {
	case <-done:
		e.opMu.Unlock()
		t.Fatal("config mutation interleaved with an active engine operation")
	case <-time.After(40 * time.Millisecond):
	}
	e.opMu.Unlock()
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("config update after the operation lock was released: %d %s", rec.Code, rec.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("config mutation did not resume after the engine operation completed")
	}
}

func TestOfficialResetIsBlockedByNoEdgeAndMutatesNothing(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	f.set(func(f *fakes) {
		f.pred["evaluation"].(map[string]any)["verdict"] = "NO EDGE"
	})
	e, store := harness(t, f, t.TempDir())
	cfg := e.cfg
	cfg.AuthSecret, cfg.CookieName, cfg.SystemUID = "s", "nvda_session", "paper-engine"
	api := newAPI(cfg, store, e.clients, e)
	api.now = func() time.Time { return now }

	readyRec := httptest.NewRecorder()
	api.routes().ServeHTTP(readyRec, httptest.NewRequest(http.MethodGet, "/paper/readiness", nil))
	if readyRec.Code != http.StatusOK {
		t.Fatalf("readiness = %d: %s", readyRec.Code, readyRec.Body.String())
	}
	var ready experimentReadiness
	if err := json.Unmarshal(readyRec.Body.Bytes(), &ready); err != nil {
		t.Fatal(err)
	}
	if ready.Ready || len(ready.Blockers) == 0 || !strings.Contains(strings.Join(ready.Blockers, " "), "NO EDGE") {
		t.Fatalf("NO EDGE must be a named launch blocker, got %+v", ready)
	}

	req := httptest.NewRequest(http.MethodPost, "/paper/reset?confirm=true", nil)
	req.AddCookie(sessionCookie("s", cfg.SystemUID, time.Now().Add(time.Hour)))
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reset under NO EDGE = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if e.ledger.st.OfficialStartedAt != "" || e.ledger.generation != 0 {
		t.Fatalf("a refused start must not move the official clock: %+v", e.ledger.st)
	}
	if calls := f.journalCalls(); len(calls) != 0 {
		t.Fatalf("readiness refusal must happen before journal mutation, calls=%v", calls)
	}
}

func TestSuccessfulResetPersistsOfficialDayZero(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	f := newFakes(t, now)
	e, store := harness(t, f, dir)
	cfg := e.cfg
	cfg.AuthSecret, cfg.CookieName, cfg.SystemUID = "s", "nvda_session", "paper-engine"
	api := newAPI(cfg, store, e.clients, e)
	api.now = func() time.Time { return now }

	req := httptest.NewRequest(http.MethodPost, "/paper/reset?confirm=true", nil)
	req.AddCookie(sessionCookie("s", cfg.SystemUID, time.Now().Add(time.Hour)))
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ready reset = %d: %s", rec.Code, rec.Body.String())
	}
	want := now.Format(time.RFC3339)
	if e.ledger.st.OfficialStartedAt != want || len(e.ledger.st.OfficialConfigs) != 1 ||
		e.ledger.st.OfficialConfigs[0] != testCfg.Key() {
		t.Fatalf("official clock = %q %+v, want %q [%s]",
			e.ledger.st.OfficialStartedAt, e.ledger.st.OfficialConfigs, want, testCfg.Key())
	}

	reopened, err := openLedger(dir, defaultStartingCash)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.st.OfficialStartedAt != want {
		t.Fatalf("day 0 did not survive reopen: %q", reopened.st.OfficialStartedAt)
	}
}

func TestChangingScopeInvalidatesOfficialClock(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	dir := t.TempDir()
	e, store := harness(t, f, dir)
	if err := e.ledger.StartOfficial(now, store.Configs()); err != nil {
		t.Fatal(err)
	}
	api := newAPI(e.cfg, store, e.clients, e)
	rec := httptest.NewRecorder()
	api.handleSetConfig(rec, httptest.NewRequest(http.MethodPost, "/paper/config",
		strings.NewReader(`{"configs":"NVDA:1D:5,GOOGL:1D:5"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("config update = %d: %s", rec.Code, rec.Body.String())
	}
	if e.ledger.st.OfficialStartedAt != "" || e.ledger.st.OfficialConfigs != nil {
		t.Fatalf("changed scope retained an old official clock: %+v", e.ledger.st)
	}
	if !strings.Contains(rec.Body.String(), `"officialClockInvalidated":true`) {
		t.Fatalf("response did not disclose clock invalidation: %s", rec.Body.String())
	}
	reopened, err := openLedger(dir, defaultStartingCash)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.st.OfficialStartedAt != "" || reopened.st.OfficialConfigs != nil {
		t.Fatalf("clock invalidation did not survive reopen: %+v", reopened.st)
	}
}
