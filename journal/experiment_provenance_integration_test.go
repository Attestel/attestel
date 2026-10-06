package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// experiment_provenance_integration_test.go — ONE UNSTATED FIELD, FOLLOWED ACROSS THREE SERVICES.
//
// The claim being tested is a chain, and every link of it was independently wrong at some point:
//
//	a model record with no `trainedOnSynthetic`
//	  -> /predict serves        trainedOnSynthetic: null        (services/prediction/app/main.py)
//	  -> paper decodes          predictResp.TrainedOnSynthetic == nil   (paper/clients.go)
//	  -> gate 1 REFUSES         "does not state whether it was trained on synthetic data"
//	  -> /paper/provenance      trainedOnSynthetic: null        (paper/provenance.go)
//	  -> the snapshot raises    synthetic-trained-model         (experiment_snapshot.go)
//	  -> candidateStatus        unknown, never no_edge
//
// UNIT TESTS ON EACH LINK WOULD NOT HAVE CAUGHT THE ORIGINAL BUG. Each service's own tests supplied
// a fixture that stated the field, so each service was individually correct about a case that never
// occurred in them. The defect lived in the DEFAULT that appeared when the field was absent, which
// is precisely what no fixture exercised.
//
// So this test runs the REAL paper binary — compiled from source, as a process — against a fake
// prediction service that omits the field, and then assembles a REAL evidence snapshot from it.
// Only `/predict` and `/candles` are faked, and they are faked to be as close to the real payloads
// as the assertion allows. `bridge/integration_test.go` uses the same shape for the same reason.
//
// It needs no PostgreSQL, no Docker and no network: the paper service runs on its documented file
// backend on a loopback port with a temp data directory.

// fakePrediction serves ONE model record. `stateSynthetic` decides whether the record mentions its
// training provenance at all — which is the entire variable under test.
type fakePrediction struct {
	stateSynthetic bool
	syntheticValue bool
}

func (f fakePrediction) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /predict/{ticker}", func(w http.ResponseWriter, r *http.Request) {
		record := map[string]any{
			"ticker": strings.ToUpper(r.PathValue("ticker")), "timeframe": "1D", "horizon": 5,
			"asOf":         time.Now().UTC().Format(time.RFC3339),
			"modelVersion": "model-int-1", "strategyVersion": "sv-int-1",
			"dataThrough": time.Now().UTC().Format("2006-01-02"),
			"signal": map[string]any{
				"direction": "Buy", "probUp": 0.61, "confidence": 0.3,
			},
			"backtest": map[string]any{
				"passed": true, "costBps": 6.0, "allowShort": true, "hitRate": 0.55,
			},
			"currentData": map[string]any{"source": "tiingo", "synthetic": false},
			"evaluation": map[string]any{
				"verdict": "NO EDGE", "evaluatedAt": "2026-08-20T00:00:00Z",
				"strategyVersion": "sv-int-1", "current": true, "evidenceCurrent": true,
				"method": "portfolio-v3", "evidenceIssues": []string{},
			},
		}
		// THE VARIABLE. When the record does not state it, the key is absent entirely — which is
		// exactly what a legacy or hand-written record looks like on disk.
		if f.stateSynthetic {
			record["trainedOnSynthetic"] = f.syntheticValue
		}
		_ = json.NewEncoder(w).Encode(record)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeAnalysis serves the bar and quote the paper engine needs to reach gate 1 at all.
func fakeAnalysis(t *testing.T) *httptest.Server {
	t.Helper()
	now := time.Now().UTC()
	bar := now.AddDate(0, 0, -1).Format("2006-01-02")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /candles/{ticker}", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ticker": "NVDA", "timeframe": "1D", "source": "tiingo", "sourceIsSynthetic": false,
			"bars": []any{map[string]any{"time": bar, "close": 100.0}},
		})
	})
	mux.HandleFunc("GET /quote/{ticker}", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"symbol": "NVDA", "price": 100.0, "source": "tiingo",
			"asOf": now.Format(time.RFC3339),
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// startPaper compiles and runs the REAL paper service against the supplied upstreams.
func startPaper(t *testing.T, predictionURL, analysisURL string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "paper-under-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "../paper"
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cannot build the paper service: %v\n%s", err, out)
	}

	port := freeLoopbackPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, bin)
	// A DELIBERATELY MINIMAL ENVIRONMENT, for the reason bridge/integration_test.go gives: so this
	// test cannot pick up a real DATABASE_URL or AUTH_SECRET from the developer's shell and write
	// into something that matters. No AUTH_SECRET at all, so the engine records nothing in a
	// journal — this test reads gates and provenance, it does not book trades.
	cmd.Env = []string{
		"PORT=" + port,
		"PREDICTION_URL=" + predictionURL,
		"ANALYSIS_URL=" + analysisURL,
		"JOURNAL_URL=http://127.0.0.1:1", // unreachable on purpose; nothing here should record
		"CONFIGS=NVDA:1D:5",
		"DATA_DIR=" + t.TempDir(),
		"ATTESTEL_REVISION=integration-rev",
		"EVAL_INTERVAL=1h", // long, so the background loop does not race the assertions
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("cannot start the paper service: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("paper service log:\n%s", logs.String())
		}
	})

	base := "http://127.0.0.1:" + port
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the paper service did not become healthy:\n%s", logs.String())
	return ""
}

func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

// getJSON reads one JSON document from the running paper service.
func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("GET %s decode: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %v", url, resp.StatusCode, out)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────── the chain

func TestUnstatedSyntheticProvenanceStaysUnknownAcrossServices(t *testing.T) {
	if testing.Short() {
		t.Skip("this test compiles and runs the real paper binary")
	}
	prediction := fakePrediction{stateSynthetic: false} // the record does not mention the field
	paperURL := startPaper(t, prediction.server(t).URL, fakeAnalysis(t).URL)

	// ── LINK 1+2: the paper service decoded /predict and served the provenance. ──────────────
	prov := getJSON(t, paperURL+"/paper/provenance")
	rows, _ := prov["configs"].([]any)
	if len(rows) != 1 {
		t.Fatalf("configs = %v", prov["configs"])
	}
	row := rows[0].(map[string]any)
	if row["availability"] != "live" {
		t.Fatalf("availability = %v; the fake prediction service answered", row["availability"])
	}
	// THE ASSERTION THE WHOLE FILE EXISTS FOR. The key must be present and null — not false, and
	// not omitted. `false` is a denial nobody made; omitted is indistinguishable from a field the
	// serialiser forgot.
	raw, ok := row["trainedOnSynthetic"]
	if !ok {
		t.Fatal("/paper/provenance omitted trainedOnSynthetic; it must be present and null")
	}
	if raw != nil {
		t.Fatalf("/paper/provenance served trainedOnSynthetic=%v for a record that never stated "+
			"it; silence must not become a denial", raw)
	}
	// The model identity still came through, which is what distinguishes this from `unavailable`.
	if row["modelVersion"] != "model-int-1" {
		t.Errorf("modelVersion = %v", row["modelVersion"])
	}

	// ── LINK 3: gate 1 refuses. ──────────────────────────────────────────────────────────────
	readiness := getJSON(t, paperURL+"/paper/readiness")
	if readiness["ready"] == true {
		t.Error("the launch checklist passed with a model of unstated training provenance")
	}
	if !readinessRefusedFor(readiness, "no-synthetic-data", "does not state") {
		t.Errorf("gate 1 did not refuse an unstated training provenance; checklist: %v",
			readiness["blockers"])
	}

	// ── LINK 4+5: the snapshot records it as a blocker, and the candidate is unknown. ────────
	srv, _ := agencyFixture(t)
	srv.cfg.PaperURL = paperURL
	store, err := openExperimentSnapshotStore(srv.cfg.TradesDir, srv.cfg.AgencyOwnerUIDs, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.snapshots = store

	snap, err := srv.assembleExperimentSnapshot(context.Background(), agencyOwner, time.Now().UTC())
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if !hasBlocker(snap, blockerSyntheticModel) {
		t.Fatalf("the snapshot raised no %q blocker for a model of unstated training provenance; "+
			"blockers: %v", blockerSyntheticModel, blockerCodes(snap))
	}
	if stmt := blockerStatement(snap, blockerSyntheticModel); !strings.Contains(stmt, "does not state") {
		t.Errorf("the blocker claims synthetic training rather than unknown provenance: %q", stmt)
	}
	// NOT `no_edge`, even though the evaluator returned exactly that. A verdict about a model whose
	// provenance nobody recorded establishes nothing about the real market.
	if snap.Derived.CandidateStatus != candidateUnknown {
		t.Errorf("candidateStatus = %q, want %q", snap.Derived.CandidateStatus, candidateUnknown)
	}
	// And the raw evaluator result is still visible, rather than being erased by the demotion.
	if snap.Derived.EvaluatorResult != evaluatorNoEdge {
		t.Errorf("evaluatorResult = %q, want %q — the verdict WAS read and must not be hidden",
			snap.Derived.EvaluatorResult, evaluatorNoEdge)
	}
	if snap.Derived.EvidenceValidity != evidenceInvalid {
		t.Errorf("evidenceValidity = %q, want %q", snap.Derived.EvidenceValidity, evidenceInvalid)
	}
}

// The control case: an EXPLICIT `false` is the only value that means verified-real, and it must
// still pass every link. Without this, the test above would also pass on a build that rejected
// everything.
func TestAnExplicitFalseProvenanceIsAcceptedAcrossServices(t *testing.T) {
	if testing.Short() {
		t.Skip("this test compiles and runs the real paper binary")
	}
	prediction := fakePrediction{stateSynthetic: true, syntheticValue: false}
	paperURL := startPaper(t, prediction.server(t).URL, fakeAnalysis(t).URL)

	row := getJSON(t, paperURL+"/paper/provenance")["configs"].([]any)[0].(map[string]any)
	if row["trainedOnSynthetic"] != false {
		t.Fatalf("trainedOnSynthetic = %v, want a stated false", row["trainedOnSynthetic"])
	}
	readiness := getJSON(t, paperURL+"/paper/readiness")
	if readinessRefusedFor(readiness, "no-synthetic-data", "") {
		t.Errorf("gate 1 refused a model that explicitly states real training data: %v",
			readiness["blockers"])
	}

	srv, _ := agencyFixture(t)
	srv.cfg.PaperURL = paperURL
	store, err := openExperimentSnapshotStore(srv.cfg.TradesDir, srv.cfg.AgencyOwnerUIDs, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.snapshots = store
	snap, err := srv.assembleExperimentSnapshot(context.Background(), agencyOwner, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if hasBlocker(snap, blockerSyntheticModel) {
		t.Error("a model that explicitly states real training data raised a synthetic blocker")
	}
	// With clean provenance the NO EDGE verdict IS spendable as a finding about the real market.
	if snap.Derived.CandidateStatus != candidateNoEdge {
		t.Errorf("candidateStatus = %q, want %q — with clean provenance a NO EDGE verdict is real "+
			"negative evidence", snap.Derived.CandidateStatus, candidateNoEdge)
	}
	if snap.Derived.EvidenceValidity != evidenceValid {
		t.Errorf("evidenceValidity = %q, want %q", snap.Derived.EvidenceValidity, evidenceValid)
	}
}

// readinessRefusedFor reports whether a named check failed, optionally requiring its detail to
// contain `contains`.
func readinessRefusedFor(readiness map[string]any, name, contains string) bool {
	configs, _ := readiness["configs"].([]any)
	for _, c := range configs {
		row, _ := c.(map[string]any)
		checks, _ := row["checks"].([]any)
		for _, ch := range checks {
			check, _ := ch.(map[string]any)
			if check["name"] != name || check["ok"] == true {
				continue
			}
			detail, _ := check["detail"].(string)
			if contains == "" || strings.Contains(detail, contains) {
				return true
			}
		}
	}
	return false
}

func blockerCodes(snap *ExperimentSnapshot) []string {
	out := make([]string, 0, len(snap.Derived.MandatoryBlockers))
	for _, b := range snap.Derived.MandatoryBlockers {
		out = append(out, b.Code)
	}
	return out
}
