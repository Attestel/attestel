package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
)

// stub.go — the deterministic stand-in for Hermes.
//
// WHY IT SHIPS IN THE BINARY RATHER THAN LIVING IN A TEST FILE. Two callers need it, and only one
// of them is a test:
//
//  1. `integration_test.go` runs the complete workflow — queue, claim, four stages, validate,
//     upload, read back — with no model, no provider credential and no network. That is what makes
//     the end-to-end test something CI and a new contributor can actually run.
//  2. An operator setting the lane up for the first time can run `ATTESTEL_BRIDGE_DRY_RUN=1
//     attestel-hermes-bridge` and prove the whole pipe works — token, URL, routes, schema,
//     validation, UI — BEFORE spending a single model call on it. Debugging authentication and
//     debugging a research prompt at the same time is how an evening disappears.
//
// EVERY ARTIFACT IT PRODUCES IS LABELLED. `run.go` pushes "hermes-dry-run: …" onto the artifact's
// `degraded` list whenever this runner is active, so a stubbed research record is identifiable
// forever and can never be mistaken for real research. That is the same discipline
// services/llm/app/versioning.py applies when it stamps `stub:offline` as a model id: "a
// stub-generated artefact must be identifiable as one, or the ablation silently averages
// deterministic text into the model's score."
//
// It is OFF unless ATTESTEL_BRIDGE_DRY_RUN is explicitly set. It cannot be reached from the hosted
// side under any circumstances — there is no field on `Job` that selects a runner.

type stubRunner struct{}

// stubSource is the one citation every stubbed stage declares. It is a real, stable, public URL so
// the artifact passes the same URL validation a real one does — the stub must exercise the
// validator, not bypass it.
const stubSourceURL = "https://www.sec.gov/edgar/searchedgar/companysearch"

func (stubRunner) Run(_ context.Context, spec stageSpec, workdir, queryPath string, _ Config) (string, error) {
	// The stub still reads the query file. If it did not, the test would pass with a bridge that
	// never wrote one, and "the question reaches the agent through a file" would be untested.
	raw, err := os.ReadFile(queryPath)
	if err != nil {
		return "", errf("stub could not read the query file for stage %s", spec.Profile)
	}
	if !strings.Contains(string(raw), "QUESTION") {
		return "", errf("stub found no question block in the query file for stage %s", spec.Profile)
	}
	// And it honours the clean-workdir rule, so the test covers that too.
	if err := assertCleanWorkdir(workdir); err != nil {
		return "", err
	}

	source := map[string]any{
		"title":       "EDGAR full-text company search",
		"url":         stubSourceURL,
		"publishedAt": "unknown",
		"publisher":   "U.S. Securities and Exchange Commission",
	}
	base := map[string]any{
		"sources": []any{source},
		"findings": []any{
			map[string]any{
				"statement":  "Stage " + spec.Profile + " ran as a dry-run placeholder; nothing was researched.",
				"provenance": provenanceUnknown,
				"sourceUrls": []string{},
				"basis":      "",
			},
			map[string]any{
				"statement":  "A primary-source search endpoint exists for this issuer's filings.",
				"provenance": provenanceSourced,
				"sourceUrls": []string{stubSourceURL},
				"basis":      "",
			},
		},
		"notes":               []string{"dry run: deterministic stub output"},
		"unresolvedQuestions": []string{"Everything: no research was performed in dry-run mode."},
	}

	switch spec.Profile {
	case "stock-risk":
		base["contradictions"] = []any{}
		base["veto"] = map[string]any{
			"raised":  true,
			"reasons": []string{"dry run: no evidence was gathered, so new exposure is not supported"},
		}
	case "stock-chair":
		finding := map[string]any{
			"statement":  "No evidence was gathered in dry-run mode.",
			"provenance": provenanceUnknown,
			"sourceUrls": []string{},
			"basis":      "",
		}
		base["thesis"] = map[string]any{
			"statement": "No thesis can be supported from a dry run.",
			"support":   []any{finding},
		}
		base["antiThesis"] = map[string]any{
			"statement": "No anti-thesis can be supported from a dry run either.",
			"support":   []any{finding},
		}
		// Deliberately NOT phrased as "this run was produced by …": that is a statement about how
		// the answer was generated, and the leak scan refuses those on purpose (redact.go). The
		// stub has to satisfy the same validator a real stage does, or it would be testing a
		// weaker pipeline than the one that ships.
		base["conclusion"] = "Dry-run placeholder output from the local stub. It establishes that " +
			"the pipeline works end to end and establishes nothing whatsoever about the company."
		base["keyRisks"] = []string{"Treating a dry-run artifact as research."}
		base["whatWouldChangeIt"] = []string{"Running the workflow against the real profiles."}
		base["researchPriority"] = "unknown"
	}

	out, err := json.Marshal(base)
	if err != nil {
		return "", errf("stub could not encode stage %s", spec.Profile)
	}
	return string(out), nil
}
