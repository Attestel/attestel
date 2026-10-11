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

// provenance_test.go — `GET /paper/provenance` must answer the two questions no existing payload
// answers as data: WHICH MODEL is serving, and WHAT THE EVALUATOR SAID about it.
//
// The load-bearing property under test is the one an evidence snapshot depends on: an upstream that
// could not be reached must produce `unavailable` WITH A REASON, never a row of zeros and never a
// row that reads as clean.

type provenanceBody struct {
	Generation       int64  `json:"generation"`
	Revision         string `json:"revision"`
	AsOf             string `json:"asOf"`
	TradeableVerdict string `json:"tradeableVerdict"`
	Configs          []struct {
		Config             string `json:"config"`
		Availability       string `json:"availability"`
		UnavailableReason  string `json:"unavailableReason"`
		ModelVersion       string `json:"modelVersion"`
		StrategyVersion    string `json:"strategyVersion"`
		TrainedOnSynthetic *bool  `json:"trainedOnSynthetic"`
		DataThrough        string `json:"dataThrough"`
		HasSignal          *bool  `json:"hasSignal"`
		CurrentData        *struct {
			Source    string `json:"source"`
			Synthetic bool   `json:"synthetic"`
		} `json:"currentData"`
		Evaluation *struct {
			Verdict         string   `json:"verdict"`
			StrategyVersion string   `json:"strategyVersion"`
			Current         *bool    `json:"current"`
			EvidenceCurrent *bool    `json:"evidenceCurrent"`
			EvidenceIssues  []string `json:"evidenceIssues"`
			Method          string   `json:"method"`
		} `json:"evaluation"`
	} `json:"configs"`
}

func readProvenance(t *testing.T, api *API) provenanceBody {
	t.Helper()
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/provenance", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("provenance status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body provenanceBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func TestProvenanceServesModelIdentityAndEvaluatorVerdict(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	e.cfg.Revision = "rev-abc123"

	api := newAPI(e.cfg, store, e.clients, e)
	body := readProvenance(t, api)

	if len(body.Configs) != 1 {
		t.Fatalf("configs = %+v", body.Configs)
	}
	row := body.Configs[0]
	if row.Availability != provenanceLive {
		t.Fatalf("availability = %q, want %q (reason %q)", row.Availability, provenanceLive,
			row.UnavailableReason)
	}
	// The two identity fields `/paper/status` only carries inside an OPEN position's block — which
	// is why this endpoint exists at all.
	if row.ModelVersion != "model-v1" || row.StrategyVersion != "sv1-abcdef0123456789" {
		t.Errorf("model identity = %q / %q", row.ModelVersion, row.StrategyVersion)
	}
	if row.TrainedOnSynthetic == nil || *row.TrainedOnSynthetic {
		t.Errorf("trainedOnSynthetic = %v, want a stated false", row.TrainedOnSynthetic)
	}
	if row.HasSignal == nil || !*row.HasSignal {
		t.Errorf("hasSignal = %v, want a stated true", row.HasSignal)
	}
	if row.CurrentData == nil || row.CurrentData.Synthetic {
		t.Errorf("currentData = %+v, want a non-synthetic frame", row.CurrentData)
	}
	if row.Evaluation == nil {
		t.Fatal("no evaluation block; the evaluator verdict is the whole point of this route")
	}
	if row.Evaluation.Verdict != "EDGE" || row.Evaluation.Method != "portfolio-v3" {
		t.Errorf("evaluation = %+v", row.Evaluation)
	}
	if row.Evaluation.Current == nil || row.Evaluation.EvidenceCurrent == nil {
		t.Errorf("current / evidenceCurrent must be STATED, not omitted: %+v", row.Evaluation)
	}
	if body.Revision != "rev-abc123" {
		t.Errorf("revision = %q", body.Revision)
	}
	if body.TradeableVerdict != verdictEdge {
		t.Errorf("tradeableVerdict = %q, want %q", body.TradeableVerdict, verdictEdge)
	}
}

// THE CENTRAL TEST. An unreachable prediction service must NOT produce a row that looks clean.
//
// The failure this prevents is precise: a `trainedOnSynthetic` of `false` and an absent evaluation
// block, on a row with no availability marker, is indistinguishable from a healthy model with no
// verdict yet. One of those is a fact and the other is silence.
func TestProvenanceReportsAnUnreachableUpstreamAsUnavailableNotClean(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	f.set(func(f *fakes) { f.predErr = true })

	api := newAPI(e.cfg, store, e.clients, e)
	body := readProvenance(t, api)

	if len(body.Configs) != 1 {
		t.Fatalf("configs = %+v", body.Configs)
	}
	row := body.Configs[0]
	if row.Availability != provenanceUnavailable {
		t.Fatalf("availability = %q, want %q", row.Availability, provenanceUnavailable)
	}
	if strings.TrimSpace(row.UnavailableReason) == "" {
		t.Error("an unavailable row must say why; a bare `unavailable` is not actionable")
	}
	// NOT `false`. A reader must never be able to take an unreached upstream for a verified
	// not-synthetic model.
	if row.TrainedOnSynthetic != nil {
		t.Errorf("trainedOnSynthetic = %v on an unavailable row; it must be absent, not false",
			*row.TrainedOnSynthetic)
	}
	if row.HasSignal != nil {
		t.Errorf("hasSignal = %v on an unavailable row; it must be absent", *row.HasSignal)
	}
	if row.ModelVersion != "" || row.Evaluation != nil || row.CurrentData != nil {
		t.Errorf("an unavailable row carried measured-looking fields: %+v", row)
	}
}

// A MODEL RECORD THAT NEVER MENTIONS ITS TRAINING PROVENANCE MUST STAY UNKNOWN.
//
// THE BUG THIS PINS. `predictResp.TrainedOnSynthetic` was a plain `bool`, so a /predict response
// with no `trainedOnSynthetic` field decoded as `false` — and every downstream reader, including
// gate 1 and this endpoint, treated the silence as an explicit denial. A model of unknown training
// provenance read as a verified-clean one, which is the exact inversion the gate exists to prevent.
func TestAbsentSyntheticProvenanceStaysUnknownRatherThanBecomingFalse(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	f.set(func(f *fakes) { delete(f.pred, "trainedOnSynthetic") })

	api := newAPI(e.cfg, store, e.clients, e)
	row := readProvenance(t, api).Configs[0]

	if row.Availability != provenanceLive {
		t.Fatalf("availability = %q; the service answered", row.Availability)
	}
	if row.TrainedOnSynthetic != nil {
		t.Fatalf("trainedOnSynthetic = %v; an unstated field must stay null, never become false",
			*row.TrainedOnSynthetic)
	}
}

// And the GATE must refuse it, for the same reason: unknown provenance is not clean provenance.
func TestGateOneRefusesAModelRecordThatDoesNotStateItsTrainingProvenance(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	f.set(func(f *fakes) { delete(f.pred, "trainedOnSynthetic") })
	e.evalConfig(context.Background(), testCfg, now)

	decision := lastDecision(t, store)
	if decision == nil {
		t.Fatal("no decision was recorded")
	}
	if decision.Gate != "no-synthetic-data" {
		t.Fatalf("gate = %q, want no-synthetic-data; an unstated training provenance must refuse",
			decision.Gate)
	}
	if !strings.Contains(decision.Reason, "does not state") {
		t.Errorf("the refusal does not explain itself: %q", decision.Reason)
	}
}

// An absent evaluation is its own state, distinct from an unavailable upstream: the service ANSWERED
// and said there is no verdict. Both must be visible, and they must not look alike.
func TestProvenanceDistinguishesNoVerdictFromNoAnswer(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	f.set(func(f *fakes) { f.pred["evaluation"] = nil })

	api := newAPI(e.cfg, store, e.clients, e)
	row := readProvenance(t, api).Configs[0]

	if row.Availability != provenanceLive {
		t.Fatalf("availability = %q; the service answered, so the row is live", row.Availability)
	}
	if row.Evaluation != nil {
		t.Errorf("evaluation = %+v, want nil — no verdict exists for this config", row.Evaluation)
	}
	// The identity is still known, which is exactly what makes this different from `unavailable`.
	if row.ModelVersion == "" {
		t.Error("a live row with no verdict must still carry the model identity")
	}
}

// An unset ATTESTEL_REVISION must serialise as the literal "unavailable", never as "". The snapshot
// that consumes this cannot tell an empty string from a field somebody forgot to fill in.
func TestUnsetRevisionIsTheLiteralUnavailable(t *testing.T) {
	t.Setenv("ATTESTEL_REVISION", "")
	t.Setenv("GIT_SHA", "")
	if got := loadConfig().Revision; got != revisionUnknown {
		t.Fatalf("Revision = %q, want %q", got, revisionUnknown)
	}
}

// The generation and revision must reach EVERY payload an evidence snapshot composes. Without that,
// a snapshot cannot prove its four sources came from one generation.
func TestSnapshotSourcesAllCarryGenerationAndRevision(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	e.cfg.Revision = "rev-xyz789"
	api := newAPI(e.cfg, store, e.clients, e)
	handler := api.routes()

	for _, path := range []string{
		"/paper/status", "/paper/dashboard", "/paper/experiments", "/paper/readiness",
		"/paper/provenance",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", path, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("%s decode: %v", path, err)
		}
		if _, ok := body["generation"]; !ok {
			t.Errorf("%s carries no top-level `generation`", path)
		}
		if body["revision"] != "rev-xyz789" {
			t.Errorf("%s revision = %v, want rev-xyz789", path, body["revision"])
		}
	}
}
