package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// handlers_test.go — the refusal has to be VISIBLE.
//
// The engine currently trades nothing, which is correct. An empty book with no explanation is
// indistinguishable from a broken service, so the reason each config is refused must reach the JSON.

func TestStatusSurfacesTheRefusingGateAndReason(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	f.set(func(f *fakes) { f.pred["evaluation"] = nil })
	e.evalConfig(context.Background(), testCfg, now)

	api := newAPI(e.cfg, store, e.clients, e)
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	var body struct {
		Configs []struct {
			Config         string `json:"config"`
			Position       string `json:"position"`
			LastBarActedOn string `json:"lastBarActedOn"`
			LastDecision   *struct {
				Bar    string `json:"bar"`
				Target string `json:"target"`
				Action string `json:"action"`
				Gate   string `json:"gate"`
				Reason string `json:"reason"`
				Gates  []struct {
					Name string `json:"name"`
					OK   bool   `json:"ok"`
				} `json:"gates"`
			} `json:"lastDecision"`
		} `json:"configs"`
		Contract string `json:"contract"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Configs) != 1 {
		t.Fatalf("configs = %+v", body.Configs)
	}
	c := body.Configs[0]
	if c.Config != "NVDA:1D:5" || c.Position != "flat" {
		t.Errorf("config row = %+v", c)
	}
	if c.LastDecision == nil {
		t.Fatal("the status payload must carry the last decision")
	}
	if c.LastDecision.Bar != barLabel(now) || c.LastDecision.Target != "long" || c.LastDecision.Action != "none" {
		t.Errorf("decision = %+v", *c.LastDecision)
	}
	if c.LastDecision.Gate != "evaluator-verdict" ||
		!strings.Contains(c.LastDecision.Reason, "no persisted evaluator verdict") {
		t.Errorf("the refusing gate and its reason must both be served, got %+v", *c.LastDecision)
	}
	if len(c.LastDecision.Gates) != 4 {
		t.Errorf("every gate's verdict should be served, got %d", len(c.LastDecision.Gates))
	}
	if !strings.Contains(body.Contract, "PAPER_EXECUTION_CONTRACT") {
		t.Errorf("the payload should name the contract it implements, got %q", body.Contract)
	}
}

// The comparison payload must not present live and backtest numbers as like-for-like.
func TestComparisonDeclaresItsUnitsAndTheRefusal(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	f.set(func(f *fakes) { f.pred["evaluation"] = nil })
	e.evalConfig(context.Background(), testCfg, now)

	api := newAPI(e.cfg, store, e.clients, e)
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/comparison", nil))

	var body struct {
		Comparisons []struct {
			Note string `json:"note"`
			Live struct {
				SharpePerTrade *float64 `json:"sharpePerTradeIfEnough"`
			} `json:"live"`
			LastDecision *Decision `json:"lastDecision"`
		} `json:"comparisons"`
		Units map[string]string `json:"units"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Units["live.sharpePerTradeIfEnough"] == "" || body.Units["backtest.sharpe"] == "" {
		t.Fatalf("the payload must declare which unit each column is in, got %v", body.Units)
	}
	if !strings.Contains(body.Units["backtest.sharpe"], "ANNUALIZED") ||
		!strings.Contains(body.Units["live.sharpePerTradeIfEnough"], "UN-annualized") {
		t.Errorf("units = %v", body.Units)
	}
	if len(body.Comparisons) != 1 {
		t.Fatalf("comparisons = %+v", body.Comparisons)
	}
	if !strings.Contains(body.Comparisons[0].Note, "evaluator-verdict") {
		t.Errorf("the note must say why nothing is being traded, got %q", body.Comparisons[0].Note)
	}
	if body.Comparisons[0].LastDecision == nil {
		t.Error("the comparison row must carry the last decision")
	}
}

// /paper/positions must not advertise an exit that no longer exists.
func TestPositionsDoNotAdvertiseAScheduledExit(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	e.evalConfig(context.Background(), testCfg, now)

	// The journal fake does not serve GET /trades, so this exercises the payload shape via a stub.
	jm := http.NewServeMux()
	jm.HandleFunc("GET /trades", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"trades": []any{map[string]any{
			"id": "t1", "ticker": "NVDA", "side": "long", "status": "open", "mode": "paper",
			"entry":          map[string]any{"date": "2026-08-20", "price": 100.0, "size": 100.0, "timeframe": "1D"},
			"attachedSignal": map[string]any{"horizon": 5, "decidedOnBar": "2026-08-19"},
		}}})
	})
	js := httptest.NewServer(jm)
	defer js.Close()

	cfg := e.cfg
	cfg.JournalURL = js.URL
	e.cfg = cfg
	e.clients = newClients(cfg)
	api := newAPI(cfg, store, e.clients, e)
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/positions", nil))

	var body struct {
		Positions []map[string]any `json:"positions"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Positions) != 1 {
		t.Fatalf("positions = %+v", body.Positions)
	}
	p := body.Positions[0]
	for _, gone := range []string{"dueAt", "barsRemaining"} {
		if _, ok := p[gone]; ok {
			t.Errorf("%q is a countdown to an exit rule that does not exist any more", gone)
		}
	}
	if p["entryBar"] != barLabel(now) {
		t.Errorf("the position must name the bar it was decided on, got %v", p["entryBar"])
	}
	if p["modelVersion"] != "model-v1" || p["strategyVersion"] != "sv1-abcdef0123456789" {
		t.Errorf("the position must retain the immutable model lineage that opened it, got %+v", p)
	}
}

// --------------------------------------------------------------------------- attribution
//
// "One config per (ticker, timeframe)" is false the moment two horizons of one ticker are
// configured: NVDA:1D:5 and NVDA:1D:10 are two different models with two different track records.
// Grouping closed trades by the pair credited both with the union of their results.

func TestClosedTradesAreAttributedPerHorizon(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	cfgs := []PaperCfg{
		{Ticker: "NVDA", Timeframe: "1D", Horizon: 5},
		{Ticker: "NVDA", Timeframe: "1D", Horizon: 10},
	}
	store.SetConfigs(cfgs)

	closed := func(id string, horizon any, pnl float64) map[string]any {
		trade := map[string]any{
			"id": id, "ticker": "NVDA", "side": "long", "status": "closed", "mode": "paper",
			"entry":  map[string]any{"date": "2026-08-10", "price": 100.0, "size": 1.0, "timeframe": "1D"},
			"pnlPct": pnl,
		}
		if horizon != nil {
			trade["attachedSignal"] = map[string]any{"horizon": horizon}
		}
		return trade
	}
	f.set(func(f *fakes) {
		f.closedTrades = []any{
			closed("a", 5, 1.0), closed("b", 5, 2.0), // NVDA:1D:5
			closed("c", 10, -3.0), // NVDA:1D:10
			closed("d", nil, 9.0), // legacy: ambiguous across two configs -> excluded
		}
	})

	api := newAPI(e.cfg, store, e.clients, e)
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/comparison", nil))

	var body struct {
		Comparisons []struct {
			Config string `json:"config"`
			Live   struct {
				NClosed      int      `json:"nClosed"`
				AvgReturnPct *float64 `json:"avgReturnPct"`
			} `json:"live"`
		} `json:"comparisons"`
		Unattributed int `json:"unattributed"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]int{}
	avg := map[string]float64{}
	for _, c := range body.Comparisons {
		got[c.Config] = c.Live.NClosed
		if c.Live.AvgReturnPct != nil {
			avg[c.Config] = *c.Live.AvgReturnPct
		}
	}
	if got["NVDA:1D:5"] != 2 || got["NVDA:1D:10"] != 1 {
		t.Fatalf("each horizon must see only its own trades, got %v", got)
	}
	if avg["NVDA:1D:5"] != 1.5 || avg["NVDA:1D:10"] != -3.0 {
		t.Errorf("one config's results must not leak into the other's, got %v", avg)
	}
	if body.Unattributed != 1 {
		t.Errorf("an unattributable legacy trade must be counted, not distributed by guesswork; got %d",
			body.Unattributed)
	}
}

// A legacy trade with no horizon IS attributed when only one config could have produced it —
// excluding it there would throw away real history for no reason.
func TestALegacyTradeIsAttributedWhenUnambiguous(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir()) // one config: NVDA:1D:5
	f.set(func(f *fakes) {
		f.closedTrades = []any{map[string]any{
			"id": "legacy", "ticker": "NVDA", "side": "long", "status": "closed", "mode": "paper",
			"entry":  map[string]any{"date": "2026-08-10", "price": 100.0, "size": 1.0, "timeframe": "1D"},
			"pnlPct": 4.0,
		}}
	})

	api := newAPI(e.cfg, store, e.clients, e)
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/comparison", nil))

	var body struct {
		Comparisons []struct {
			Config string `json:"config"`
			Live   struct {
				NClosed int `json:"nClosed"`
			} `json:"live"`
		} `json:"comparisons"`
		Unattributed int `json:"unattributed"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Comparisons) != 1 || body.Comparisons[0].Live.NClosed != 1 {
		t.Fatalf("comparisons = %+v", body.Comparisons)
	}
	if body.Unattributed != 0 {
		t.Errorf("unattributed = %d, want 0", body.Unattributed)
	}
}

// --------------------------------------------------------------------------- route authentication
//
// `POST /paper/reset` deletes EVERY paper trade in the journal and `POST /paper/config` rewrites
// what the engine trades. Both used to need only `confirm=true`, which is a typo guard, not
// authentication. GET routes stay public — the frontend reads them and they disclose nothing.

func apiWithSecret(t *testing.T, secret string) *API {
	t.Helper()
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	cfg := e.cfg
	cfg.AuthSecret = secret
	cfg.CookieName = "nvda_session"
	cfg.SystemUID = "paper-engine"
	e.cfg = cfg
	return newAPI(cfg, store, newClients(cfg), e)
}

func sessionCookie(secret, uid string, exp time.Time) *http.Cookie {
	raw, _ := json.Marshal(sessionPayload{UID: uid, IAT: time.Now().Unix(), Exp: exp.Unix()})
	body := b64.EncodeToString(raw)
	return &http.Cookie{Name: "nvda_session", Value: body + "." + signPayload(secret, body)}
}

func TestMutatingRoutesRequireASession(t *testing.T) {
	api := apiWithSecret(t, "shared-secret")
	valid := sessionCookie("shared-secret", "someone", time.Now().Add(time.Hour))

	for _, route := range []string{"/paper/reset?confirm=true", "/paper/config"} {
		t.Run(route, func(t *testing.T) {
			// No cookie at all.
			rec := httptest.NewRecorder()
			api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, route,
				strings.NewReader(`{"confirm":true,"configs":"NVDA:1D:5"}`)))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("no session should be 401, got %d: %s", rec.Code, rec.Body.String())
			}

			// A cookie signed with the wrong secret.
			bad := httptest.NewRequest(http.MethodPost, route,
				strings.NewReader(`{"confirm":true,"configs":"NVDA:1D:5"}`))
			bad.AddCookie(sessionCookie("some-other-secret", "someone", time.Now().Add(time.Hour)))
			rec = httptest.NewRecorder()
			api.routes().ServeHTTP(rec, bad)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("a forged cookie should be 401, got %d", rec.Code)
			}

			// An expired cookie.
			stale := httptest.NewRequest(http.MethodPost, route,
				strings.NewReader(`{"confirm":true,"configs":"NVDA:1D:5"}`))
			stale.AddCookie(sessionCookie("shared-secret", "someone", time.Now().Add(-time.Hour)))
			rec = httptest.NewRecorder()
			api.routes().ServeHTTP(rec, stale)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("an expired cookie should be 401, got %d", rec.Code)
			}

			// A valid session gets through to the handler.
			ok := httptest.NewRequest(http.MethodPost, route,
				strings.NewReader(`{"confirm":true,"configs":"NVDA:1D:5"}`))
			ok.AddCookie(valid)
			rec = httptest.NewRecorder()
			api.routes().ServeHTTP(rec, ok)
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Fatalf("a valid session must reach the handler, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// With no AUTH_SECRET there is nothing to verify against, so the route is CLOSED and says which
// variable is missing — not left open, and not silently pretending to be authenticated.
func TestMutatingRoutesAreClosedWithNoAuthSecret(t *testing.T) {
	api := apiWithSecret(t, "")
	for _, route := range []string{"/paper/reset?confirm=true", "/paper/config"} {
		rec := httptest.NewRecorder()
		api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, route,
			strings.NewReader(`{"confirm":true,"configs":"NVDA:1D:5"}`)))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s with no AUTH_SECRET = %d, want 403", route, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "AUTH_SECRET") {
			t.Errorf("the refusal must name the missing configuration, got %s", rec.Body.String())
		}
	}
}

func TestReadRoutesStayPublic(t *testing.T) {
	api := apiWithSecret(t, "shared-secret")
	for _, route := range []string{
		"/health", "/paper/status", "/paper/config", "/paper/comparison", "/paper/dashboard", "/paper/experiments",
	} {
		rec := httptest.NewRecorder()
		api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("GET %s must stay public, got %d", route, rec.Code)
		}
	}
}
