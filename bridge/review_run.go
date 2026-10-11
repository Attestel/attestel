package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// review_run.go — one experiment-review job, start to finish.
//
//	claim ──► fetch snapshot ──► scratch dir ──► auditor ──► skeptic ──► chair ──► assemble
//	                                               │           │          │            │
//	                                           heartbeat   heartbeat  heartbeat     validate
//	                                                                                   │
//	                                                                       complete ◄──┘  or fail
//
// It is `run.go`'s shape, with one addition at the front and three differences in the middle:
//
//   - THE EVIDENCE IS FETCHED ONCE, BEFORE ANY STAGE RUNS. Every stage then reads the SAME frozen
//     document. Re-fetching between stages would reintroduce exactly the multi-moment problem the
//     snapshot exists to remove, and would let the deployment change its mind halfway through a
//     review of it.
//   - NO STAGE READS THE WEB. The chain's toolsets are `todo` and nothing else (hermes.go), and
//     `execRunner` refuses to invoke a review stage that names anything wider.
//   - THE THREE STATUSES ARE NEVER TAKEN FROM A STAGE. They are copied from the snapshot's
//     server-derived block by `assembleReview`, and no stage schema has a field for them.
//
// ONE SHOT, THEN EXIT — no loop, no sleep, no scheduler, exactly as run.go states.

// workReview is the review workflow.
func workReview(ctx context.Context, cfg Config, client hostedClient, runner hermesRunner, job *ReviewJob) error {
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.RunBudgetSeconds)*time.Second)
	defer cancel()

	// THE EVIDENCE, FETCHED ONCE. `Snapshot` validates the schema version and that the server
	// served the snapshot this job actually names, so a mismatch fails here rather than producing a
	// review of the wrong experiment.
	snapCtx, cancelSnap := withTimeout(runCtx)
	snap, err := client.Snapshot(snapCtx, job)
	cancelSnap()
	if err != nil {
		return err
	}
	index := snapshotPaths(snap)

	// The snapshot as the stages will see it: re-serialised from the decoded struct, indented, so
	// the paths a stage cites are the paths this bridge and the server both compute. It is NOT the
	// raw response body — that would include the transport envelope.
	evidence, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return errf("the evidence snapshot could not be rendered for the review stages")
	}

	workdir, err := os.MkdirTemp(cfg.StateDir, "review-")
	if err != nil {
		return errf("cannot create a scratch directory for this review")
	}
	// Removed on every path. It holds the prompts, which quote the owner's own experiment state,
	// and the stage outputs, which are unvalidated model text.
	defer os.RemoveAll(workdir)
	if err := os.Chmod(workdir, 0o700); err != nil {
		return errf("cannot secure the scratch directory")
	}

	var (
		results  []reviewStageResult
		facts    []string
		degraded []string

		blockers       []ReviewClaim
		unknowns       []ReviewClaim
		contradictions []ReviewClaim
		nextChecks     []ReviewCheck
		chair          reviewChairOutput
	)
	if cfg.DryRunHermes {
		degraded = append(degraded,
			"hermes-dry-run: stages were produced by a local stub, not by a model")
	}

	for _, spec := range experimentReviewChain {
		if err := runCtx.Err(); err != nil {
			return errf("the review exceeded its %ds budget before stage %s",
				cfg.RunBudgetSeconds, spec.Profile)
		}

		// Prove we still hold the lease BEFORE spending a stage on it.
		hbCtx, cancelHB := withTimeout(runCtx)
		hbErr := client.Heartbeat(hbCtx, job.ref(), spec.Profile, cfg)
		cancelHB()
		if hbErr != nil {
			return hbErr
		}

		startedAt := time.Now()
		queryPath := filepath.Join(workdir, "query-"+spec.Profile+".txt")
		prompt, err := renderReviewPrompt(cfg, spec, job, snap, string(evidence), facts)
		if err != nil {
			return err
		}
		// 0600, and written before the process starts. A FILE rather than an argument, for the
		// reason hermes.go's header gives: nothing in it can be shell-interpreted.
		if err := os.WriteFile(queryPath, []byte(prompt), 0o600); err != nil {
			return errf("cannot write the query file for stage %s", spec.Profile)
		}

		stdout, err := runner.Run(runCtx, spec, workdir, queryPath, cfg)
		if err != nil {
			return err
		}
		endedAt := time.Now()

		// Decode into the CLOSED schema for this stage. Everything past this point is validated,
		// and every cited path is checked against the snapshot's real index.
		var notes []string
		switch spec.Profile {
		case "experiment-auditor":
			var out auditorOutput
			if err := decodeStage(stdout, &out); err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			b, err := convertReviewClaims(out.Blockers, index, "blockers")
			if err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			u, err := convertReviewClaims(out.Unknowns, index, "unknowns")
			if err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			blockers = append(blockers, b...)
			unknowns = append(unknowns, u...)
			notes = out.Notes
			facts = append(facts, auditorFacts(b, u))

		case "evidence-skeptic":
			var out skepticOutput
			if err := decodeStage(stdout, &out); err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			// THE THREE LISTS ARE KEPT APART ALL THE WAY THROUGH.
			//
			// `missingEvidence` and `insufficientSample` are UNKNOWNS — nothing was established.
			// `negativeEvidence` is a BLOCKER — something WAS established and the answer was no.
			// Merging them at this seam is exactly how "the paper experiment has not run" and "the
			// strategy failed" become the same sentence in a report.
			missing, err := convertReviewClaims(out.MissingEvidence, index, "missingEvidence")
			if err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			insufficient, err := convertReviewClaims(out.InsufficientSample, index, "insufficientSample")
			if err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			negative, err := convertReviewClaims(out.NegativeEvidence, index, "negativeEvidence")
			if err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			contra, err := convertReviewClaims(out.Contradictions, index, "contradictions")
			if err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			unknowns = append(unknowns, missing...)
			unknowns = append(unknowns, insufficient...)
			blockers = append(blockers, negative...)
			contradictions = append(contradictions, contra...)
			notes = out.Notes
			facts = append(facts, skepticFacts(missing, insufficient, negative, contra))

		case "experiment-chair":
			var out reviewChairOutput
			if err := decodeStage(stdout, &out); err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			b, err := convertReviewClaims(out.Blockers, index, "blockers")
			if err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			u, err := convertReviewClaims(out.Unknowns, index, "unknowns")
			if err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			c, err := convertReviewClaims(out.Contradictions, index, "contradictions")
			if err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			checks, err := convertReviewChecks(out.NextChecks, index)
			if err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			blockers = append(blockers, b...)
			unknowns = append(unknowns, u...)
			contradictions = append(contradictions, c...)
			nextChecks = checks
			chair = out
			notes = out.Notes

		default:
			return errf("stage %q is not part of %s", spec.Profile, workflowExperimentReview)
		}

		results = append(results, reviewStageResult{
			spec: spec, status: "ok", notes: clipAll(notes, maxStatementLen),
			startedAt: startedAt, endedAt: endedAt,
		})
	}

	// De-duplicate before assembly: the chair legitimately restates what the auditor found, and a
	// review that listed the same blocker three times would be a worse document for it.
	blockers = dedupeClaims(blockers)
	unknowns = dedupeClaims(unknowns)
	contradictions = dedupeClaims(contradictions)

	review, err := assembleReview(job, snap, results, chair, blockers, unknowns, contradictions,
		nextChecks, degraded, time.Now())
	if err != nil {
		return err
	}
	if err := finalReviewCheck(review); err != nil {
		return err
	}

	completeCtx, cancelComplete := withTimeout(runCtx)
	defer cancelComplete()
	return client.CompleteReview(completeCtx, job, review)
}

// dedupeClaims merges claims whose normalised statement matches, keeping the first occurrence's
// code and the union of its cited paths. Conservative on purpose — it merges restatements, not
// paraphrases, exactly as `claimKey` does for research findings.
func dedupeClaims(in []ReviewClaim) []ReviewClaim {
	seen := map[string]int{}
	out := make([]ReviewClaim, 0, len(in))
	for _, c := range in {
		key := claimKey(c.Statement)
		if key == "" {
			continue
		}
		if idx, ok := seen[key]; ok {
			for _, p := range c.EvidencePaths {
				if !containsString(out[idx].EvidencePaths, p) &&
					len(out[idx].EvidencePaths) < reviewMaxPaths {
					out[idx].EvidencePaths = append(out[idx].EvidencePaths, p)
				}
			}
			if out[idx].Code == "" {
				out[idx].Code = c.Code
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, c)
	}
	return out
}

// auditorFacts and skepticFacts render a completed stage as the facts the next stage reads.
//
// Built from the VALIDATED structures, never from raw stdout — so an instruction that somehow
// survived into a statement is presented as quoted evidence under a heading rather than as part of
// the next prompt's instruction section. Same rule and same reason as run.go's `factsBlock`.
func auditorFacts(blockers, unknowns []ReviewClaim) string {
	var b strings.Builder
	b.WriteString("### experiment-auditor\n")
	writeClaimList(&b, "Blockers", blockers)
	writeClaimList(&b, "Could not establish", unknowns)
	return b.String()
}

func skepticFacts(missing, insufficient, negative, contradictions []ReviewClaim) string {
	var b strings.Builder
	b.WriteString("### evidence-skeptic\n")
	writeClaimList(&b, "Missing evidence (never measured)", missing)
	writeClaimList(&b, "Insufficient sample (measured, cannot decide)", insufficient)
	writeClaimList(&b, "Negative evidence (measured, the answer was no)", negative)
	writeClaimList(&b, "Contradictions", contradictions)
	return b.String()
}

func writeClaimList(b *strings.Builder, heading string, items []ReviewClaim) {
	if len(items) == 0 {
		b.WriteString(heading + ": none reported\n")
		return
	}
	b.WriteString(heading + ":\n")
	for _, c := range items {
		b.WriteString("- ")
		if c.Code != "" {
			b.WriteString("[" + c.Code + "] ")
		}
		b.WriteString(c.Statement)
		if len(c.EvidencePaths) > 0 {
			b.WriteString(" (" + strings.Join(c.EvidencePaths, ", ") + ")")
		}
		b.WriteString("\n")
	}
}
