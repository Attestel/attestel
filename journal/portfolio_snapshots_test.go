package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func decodeSnapshot(t *testing.T, body []byte) (PortfolioSnapshot, bool) {
	t.Helper()
	var envelope struct {
		Snapshot PortfolioSnapshot `json:"snapshot"`
		Reused   bool              `json:"reused"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, body)
	}
	return envelope.Snapshot, envelope.Reused
}

func TestPortfolioSnapshotsEstablishBaselineReuseContextAndDiffMaterialChanges(t *testing.T) {
	e := newPortfolioTestEnv(t)
	analysis := newAnalysisFixture(t)
	e.srv.cfg.AnalysisURL = analysis.server.URL
	e.srv.http = analysis.server.Client()

	p := Portfolio{
		Name: "Snapshots", BaseCurrency: "USD",
		Positions: []PortfolioPosition{{Ticker: "NVDA", Quantity: 4, Sector: "Technology"}},
		Cash:      []PortfolioCash{{Currency: "USD", Amount: 300}},
		Targets:   []PortfolioTarget{{Kind: "ticker", Key: "NVDA", MinWeight: ptr(.4), MaxWeight: ptr(.6)}},
	}
	if apiErr := validatePortfolio(&p); apiErr != nil {
		t.Fatal(apiErr)
	}
	created, err := e.srv.portfolios.Add("alice", p)
	if err != nil {
		t.Fatal(err)
	}

	firstRec := e.request(http.MethodPost, "/portfolios/"+created.ID+"/snapshots", "alice", nil)
	if firstRec.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", firstRec.Code, firstRec.Body.String())
	}
	first, reused := decodeSnapshot(t, firstRec.Body.Bytes())
	if reused || len(first.Changes) != 0 || first.MaterialChangeCount != 0 || first.ChangePolicyVersion != portfolioChangeVersion {
		t.Fatalf("unexpected baseline=%+v reused=%v", first, reused)
	}

	reuseRec := e.request(http.MethodPost, "/portfolios/"+created.ID+"/snapshots", "alice", nil)
	if reuseRec.Code != http.StatusOK {
		t.Fatalf("reuse status=%d body=%s", reuseRec.Code, reuseRec.Body.String())
	}
	reusedSnapshot, reused := decodeSnapshot(t, reuseRec.Body.Bytes())
	if !reused || reusedSnapshot.ID != first.ID {
		t.Fatalf("context was not reused: first=%s second=%s reused=%v", first.ID, reusedSnapshot.ID, reused)
	}

	analysis.prices["NVDA"] = 200
	changedRec := e.request(http.MethodPost, "/portfolios/"+created.ID+"/snapshots", "alice", nil)
	if changedRec.Code != http.StatusCreated {
		t.Fatalf("changed status=%d body=%s", changedRec.Code, changedRec.Body.String())
	}
	changed, reused := decodeSnapshot(t, changedRec.Body.Bytes())
	if reused || changed.ContextVersion == first.ContextVersion || changed.MaterialChangeCount == 0 {
		t.Fatalf("changed snapshot=%+v reused=%v", changed, reused)
	}
	types := map[string]bool{}
	for _, change := range changed.Changes {
		types[change.Type] = true
	}
	for _, want := range []string{"cash_weight", "concentration", "position_weight", "target_range"} {
		if !types[want] {
			t.Fatalf("missing %s in changes=%+v", want, changed.Changes)
		}
	}

	listRec := e.request(http.MethodGet, "/portfolios/"+created.ID+"/snapshots?limit=10", "alice", nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var list struct {
		Snapshots []PortfolioSnapshot `json:"snapshots"`
	}
	_ = json.Unmarshal(listRec.Body.Bytes(), &list)
	if len(list.Snapshots) != 2 || list.Snapshots[0].ID != changed.ID {
		t.Fatalf("snapshot list=%+v", list.Snapshots)
	}
	foreign := e.request(http.MethodGet, "/portfolios/"+created.ID+"/snapshots", "bob", nil)
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign status=%d want 404", foreign.Code)
	}
}

func TestPortfolioDiffConnectsThesisCheckChangeToHoldingWeight(t *testing.T) {
	oldConfidence, newConfidence := 40, 70
	oldAt, newAt := int64(10), int64(20)
	weight := .28
	before := PortfolioIntelligence{Positions: []PortfolioPositionIntelligence{{
		Ticker: "NVDA", Weight: &weight,
		Thesis: &PortfolioThesisContext{
			ID: "thesis", UpdatedAt: 1, LatestCheckAt: &oldAt,
			LatestCheckVerdict: "neutral", LatestCheckConfidence: &oldConfidence,
		},
	}}}
	after := PortfolioIntelligence{Positions: []PortfolioPositionIntelligence{{
		Ticker: "NVDA", Weight: &weight,
		Thesis: &PortfolioThesisContext{
			ID: "thesis", UpdatedAt: 1, LatestCheckAt: &newAt,
			LatestCheckVerdict: "challenged", LatestCheckConfidence: &newConfidence,
		},
	}}}
	changes := diffPortfolioIntelligence(before, after)
	if len(changes) != 1 {
		t.Fatalf("changes=%+v", changes)
	}
	change := changes[0]
	if change.Type != "thesis_check" || !change.Material || change.ImpactWeight == nil || *change.ImpactWeight != weight {
		t.Fatalf("thesis change=%+v", change)
	}
	if !strings.Contains(change.Summary, "thesis check") {
		t.Fatalf("summary=%q", change.Summary)
	}
}

func TestSnapshotLimitValidation(t *testing.T) {
	e := newPortfolioTestEnv(t)
	p := Portfolio{Name: "Limit", BaseCurrency: "USD"}
	if apiErr := validatePortfolio(&p); apiErr != nil {
		t.Fatal(apiErr)
	}
	created, _ := e.srv.portfolios.Add("alice", p)
	rec := e.request(http.MethodGet, "/portfolios/"+created.ID+"/snapshots?limit=0", "alice", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
