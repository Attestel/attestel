package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type portfolioTestEnv struct {
	t       *testing.T
	dir     string
	srv     *Server
	handler http.Handler
}

func newPortfolioTestEnv(t *testing.T) *portfolioTestEnv {
	t.Helper()
	dir := t.TempDir()
	portfolios, err := openPortfolioStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	theses, err := openThesisStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := openPortfolioSnapshotStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reviews, err := openPortfolioReviewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		cfg:                Config{TradesDir: dir, Secret: "portfolio-secret", CookieName: "nvda_session"},
		portfolios:         portfolios,
		portfolioSnapshots: snapshots,
		portfolioReviews:   reviews,
		theses:             theses,
	}
	return &portfolioTestEnv{t: t, dir: dir, srv: srv, handler: srv.routes()}
}

func (e *portfolioTestEnv) cookie(uid string) *http.Cookie {
	e.t.Helper()
	raw, _ := json.Marshal(sessionPayload{UID: uid, IAT: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix()})
	body := base64.RawURLEncoding.EncodeToString(raw)
	return &http.Cookie{Name: e.srv.cfg.CookieName, Value: body + "." + signPayload(e.srv.cfg.Secret, body)}
}

func (e *portfolioTestEnv) request(method, path, uid string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if uid != "" {
		req.AddCookie(e.cookie(uid))
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func decodePortfolio(t *testing.T, rec *httptest.ResponseRecorder) Portfolio {
	t.Helper()
	var body struct {
		Portfolio Portfolio `json:"portfolio"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v\n%s", err, rec.Body.String())
	}
	return body.Portfolio
}

func validPortfolioBody() map[string]any {
	return map[string]any{
		"name": "Core portfolio", "baseCurrency": "usd",
		"positions": []map[string]any{{
			"ticker": "nvda", "quantity": 4.5, "averageCost": 100.0,
			"sector": "Technology", "industry": "Semiconductors",
		}},
		"cash": []map[string]any{{"currency": "usd", "amount": 2500.0}},
		"targets": []map[string]any{{
			"kind": "ticker", "key": "nvda", "targetWeight": 0.25, "minWeight": 0.20, "maxWeight": 0.30,
		}},
		"profile": map[string]any{
			"objective": "growth", "horizon": "3_5_years", "lossTolerance": "medium",
			"constraints": map[string]any{
				"noLeverage": true, "minimumCashWeight": 0.05, "maximumPositionWeight": 0.35,
			},
		},
	}
}

func TestPortfolioCRUDIsAuthenticatedScopedAndNormalized(t *testing.T) {
	e := newPortfolioTestEnv(t)

	guest := e.request(http.MethodPost, "/portfolios", "", validPortfolioBody())
	if guest.Code != http.StatusUnauthorized {
		t.Fatalf("guest create status=%d want 401", guest.Code)
	}

	createdRec := e.request(http.MethodPost, "/portfolios", "alice", validPortfolioBody())
	if createdRec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createdRec.Code, createdRec.Body.String())
	}
	created := decodePortfolio(t, createdRec)
	if !strings.HasPrefix(created.ID, "pf_") || created.SchemaVersion != portfolioSchemaVersion || created.Version != 1 {
		t.Fatalf("server fields not assigned: %+v", created)
	}
	if created.BaseCurrency != "USD" || created.Positions[0].Ticker != "NVDA" || created.Cash[0].Currency != "USD" {
		t.Fatalf("normalization failed: %+v", created)
	}
	guestRead := e.request(http.MethodGet, "/portfolios/"+created.ID, "", nil)
	if guestRead.Code != http.StatusUnauthorized {
		t.Fatalf("guest read status=%d want 401", guestRead.Code)
	}

	foreign := e.request(http.MethodGet, "/portfolios/"+created.ID, "bob", nil)
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign read status=%d want 404", foreign.Code)
	}
	list := e.request(http.MethodGet, "/portfolios", "alice", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), created.ID) {
		t.Fatalf("owner list status=%d body=%s", list.Code, list.Body.String())
	}

	updatedRec := e.request(http.MethodPatch, "/portfolios/"+created.ID, "alice", map[string]any{
		"name": "Long-term research", "cash": []map[string]any{{"currency": "USD", "amount": 3000.0}},
	})
	if updatedRec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updatedRec.Code, updatedRec.Body.String())
	}
	updated := decodePortfolio(t, updatedRec)
	if updated.Name != "Long-term research" || updated.Version != 2 || updated.Cash[0].Amount != 3000 {
		t.Fatalf("unexpected update: %+v", updated)
	}
	if len(updated.Positions) != 1 || updated.Positions[0].Ticker != "NVDA" {
		t.Fatalf("patch discarded absent positions: %+v", updated.Positions)
	}

	deleted := e.request(http.MethodDelete, "/portfolios/"+created.ID, "alice", nil)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	missing := e.request(http.MethodGet, "/portfolios/"+created.ID, "alice", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("deleted read status=%d want 404", missing.Code)
	}
}

func TestPortfolioValidationRejectsAmbiguousOrUnsafeMath(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(map[string]any)
		field string
	}{
		{"duplicate ticker", func(b map[string]any) {
			b["positions"] = []map[string]any{{"ticker": "NVDA", "quantity": 1}, {"ticker": "nvda", "quantity": 2}}
		}, "positions[1].ticker"},
		{"negative cash", func(b map[string]any) {
			b["cash"] = []map[string]any{{"currency": "USD", "amount": -1}}
		}, "cash[0].amount"},
		{"invalid ticker characters", func(b map[string]any) {
			b["positions"] = []map[string]any{{"ticker": "NVDA/USD", "quantity": 1}}
		}, "positions[0].ticker"},
		{"invalid cash target key", func(b map[string]any) {
			b["targets"] = []map[string]any{{"kind": "cash", "key": "US", "targetWeight": .1}}
		}, "targets[0].key"},
		{"nan is invalid JSON", func(b map[string]any) {}, ""},
		{"reversed range", func(b map[string]any) {
			b["targets"] = []map[string]any{{"kind": "ticker", "key": "NVDA", "minWeight": .5, "maxWeight": .2}}
		}, "targets[0]"},
		{"weight over one", func(b map[string]any) {
			b["profile"] = map[string]any{"constraints": map[string]any{"maximumPositionWeight": 1.1}}
		}, "profile.constraints.maximumPositionWeight"},
	}
	for _, tc := range tests {
		if tc.name == "nan is invalid JSON" {
			continue // encoding/json cannot produce NaN; finite() is covered directly below.
		}
		t.Run(tc.name, func(t *testing.T) {
			e := newPortfolioTestEnv(t)
			body := validPortfolioBody()
			tc.edit(body)
			rec := e.request(http.MethodPost, "/portfolios", "alice", body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.field) {
				t.Fatalf("status=%d body=%s want field %q", rec.Code, rec.Body.String(), tc.field)
			}
		})
	}
	bad := newPortfolioForTest("bad")
	bad.Positions = []PortfolioPosition{{Ticker: "NVDA", Quantity: 1, ManualValue: func() *float64 { v := math.NaN(); return &v }()}}
	if err := validatePortfolio(&bad); err == nil || err.Field != "positions[0].manualValue" {
		t.Fatalf("non-finite manual value err=%v", err)
	}
}

func TestPortfolioStorePersistsAndRefusesUnreadableCollection(t *testing.T) {
	dir := t.TempDir()
	store, err := openPortfolioStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := newPortfolioForTest("Persisted")
	p.ID = ""
	created, err := store.Add("alice", p)
	if err != nil {
		t.Fatal(err)
	}
	reopened, _ := openPortfolioStore(dir)
	got, ok, err := reopened.Get("alice", created.ID)
	if err != nil || !ok || got.Name != "Persisted" {
		t.Fatalf("reopen got=%+v ok=%v err=%v", got, ok, err)
	}

	brokenDir := t.TempDir()
	userDir := filepath.Join(brokenDir, "alice")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(userDir, "portfolios.json")
	if err := os.WriteFile(path, []byte(`[{"id":`), 0o644); err != nil {
		t.Fatal(err)
	}
	broken, _ := openPortfolioStore(brokenDir)
	if _, err := broken.List("alice"); err == nil {
		t.Fatal("unreadable collection was presented as an empty list")
	}
	before, _ := os.ReadFile(path)
	if _, err := broken.Add("alice", newPortfolioForTest("must not overwrite")); err == nil {
		t.Fatal("write against unreadable collection succeeded")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("unreadable collection was overwritten")
	}
}
