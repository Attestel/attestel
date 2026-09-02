package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// run.go — one job, start to finish.
//
// THE SHAPE, AND EVERY PROPERTY IT IS MEANT TO HAVE:
//
//	claim  ──►  scratch dir  ──►  stage 1 ──► stage 2 ──► stage 3 ──► stage 4  ──►  assemble
//	                                 │           │           │           │              │
//	                              heartbeat   heartbeat   heartbeat   heartbeat      validate
//	                                                                                    │
//	                                                                        complete ◄──┘  or fail
//
//   - ONE JOB AT A TIME. `runOnce` claims exactly one run, works it, and returns. There is no
//     concurrency here and no worker pool: two chains against one local model would either exhaust
//     memory or serialise anyway while destroying latency, which is the reasoning
//     services/llm/app/lease.py already records for this machine.
//   - ONE PROFILE AT A TIME. The stages are a plain sequential loop, and each one receives every
//     earlier stage's VALIDATED output as facts.
//   - HEARTBEAT BETWEEN STAGES, NOT DURING ONE. A heartbeat proves we still hold the lease, so it
//     is sent before each stage starts. If it fails the run stops immediately: continuing would
//     mean spending the owner's machine on a result that can no longer be delivered.
//   - THE SCRATCH DIRECTORY IS DELETED. Every run gets a fresh 0700 directory under the state dir
//     and it is removed on the way out, in every exit path.
//   - A FAILURE IS REPORTED, NOT SWALLOWED. Every path that does not complete calls `Fail` with a
//     redacted reason, so the owner's browser shows why rather than a run that stopped moving.
//
// ONE SHOT, THEN EXIT. There is no loop, no sleep and no scheduler in this module. Repetition is
// the operator's own launchd/cron, outside this codebase — the same rule
// services/llm/app/enrich_worker.py and services/events/app/automation.py state for their own
// one-shot entrypoints, and for the same reason: a process that wakes itself up to run a model is
// the thing this architecture forbids.

// runOnce claims and works at most one job. It reports whether a job was worked.
func runOnce(ctx context.Context, cfg Config, client hostedClient, runner hermesRunner) (bool, error) {
	claimCtx, cancel := withTimeout(ctx)
	defer cancel()
	job, ok, err := client.Claim(claimCtx, cfg)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	// From here on, EVERY exit path reports back. A run that stops without a report is a run that
	// sits `running` in the owner's browser until its lease expires, which is a worse outcome than
	// an honest failure.
	if err := workJob(ctx, cfg, client, runner, job); err != nil {
		if isStaleLease(err) {
			// Not a failure to report: the lease is no longer ours, so another attempt owns this run
			// and our result — including our failure — must not overwrite theirs.
			return true, err
		}
		reason := redact(err.Error())
		failCtx, cancelFail := withTimeout(ctx)
		defer cancelFail()
		// Retryable: a transport or provider failure may succeed next time; a schema or validation
		// failure will not. `retryableFailure` decides, and the server still applies its own attempt
		// cap on top.
		if ferr := client.Fail(failCtx, job, reason, retryableFailure(err)); ferr != nil {
			return true, errf("the run failed (%s) and the failure could not be reported: %v",
				reason, ferr)
		}
		return true, err
	}
	return true, nil
}

// workJob is the workflow itself.
func workJob(ctx context.Context, cfg Config, client hostedClient, runner hermesRunner, job *Job) error {
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.RunBudgetSeconds)*time.Second)
	defer cancel()

	workdir, err := os.MkdirTemp(cfg.StateDir, "run-")
	if err != nil {
		return errf("cannot create a scratch directory for this run")
	}
	// Removed on every path, including a panic-free early return. Nothing in it is uploaded and
	// nothing in it should outlive the run: it holds the prompts, which quote the question, and the
	// stage outputs, which are the unvalidated model text.
	defer os.RemoveAll(workdir)
	if err := os.Chmod(workdir, 0o700); err != nil {
		return errf("cannot secure the scratch directory")
	}

	table := newSourceTable()
	var (
		results        []stageResult
		facts          []string
		contradictions []Contradiction
		veto           Veto
		chair          chairOutput
		thesis         Position
		antiThesis     Position
		riskFindings   []Finding
		degraded       []string
	)
	if cfg.DryRunHermes {
		// Recorded in the artifact so a stubbed run is identifiable forever. An unlabelled stub is
		// indistinguishable from real research, which is the specific dishonesty
		// services/llm/app/versioning.py refuses when it stamps `stub:offline` as a model id.
		degraded = append(degraded, "hermes-dry-run: stages were produced by a local stub, not by a model")
	}

	for _, spec := range companyResearchChain {
		if err := runCtx.Err(); err != nil {
			return errf("the run exceeded its %ds budget before stage %s",
				cfg.RunBudgetSeconds, spec.Profile)
		}

		// Prove we still hold the lease BEFORE spending a stage on it.
		hbCtx, cancelHB := withTimeout(runCtx)
		hbErr := client.Heartbeat(hbCtx, job, spec.Profile, cfg)
		cancelHB()
		if hbErr != nil {
			return hbErr
		}

		startedAt := time.Now()
		queryPath := filepath.Join(workdir, "query-"+spec.Profile+".txt")
		prompt, err := renderPrompt(cfg, spec, job, facts)
		if err != nil {
			return err
		}
		// 0600, and written before the process starts. This file is the ONLY channel by which the
		// hosted question reaches an agent, and it is a file rather than an argument precisely so
		// that nothing in it can be shell-interpreted (see hermes.go).
		if err := os.WriteFile(queryPath, []byte(prompt), 0o600); err != nil {
			return errf("cannot write the query file for stage %s", spec.Profile)
		}

		stdout, err := runner.Run(runCtx, spec, workdir, queryPath, cfg)
		if err != nil {
			return err
		}
		endedAt := time.Now()

		// Decode into the CLOSED schema for this stage. Everything past this point is validated.
		var base researchOutput
		switch spec.Profile {
		case "stock-risk":
			var out riskOutput
			if err := decodeStage(stdout, &out); err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			base = out.researchOutput
			if err := registerSources(base.Sources, table); err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			contradictions, err = convertContradictions(out.Contradictions, table)
			if err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			veto = Veto{Raised: out.Veto.Raised, Scope: vetoScope, Reasons: out.Veto.Reasons}

		case "stock-chair":
			var out chairOutput
			if err := decodeStage(stdout, &out); err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			base = out.researchOutput
			if err := registerSources(base.Sources, table); err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			chair = out
			thesis, err = convertPosition(out.Thesis, table)
			if err != nil {
				return errf("stage %s thesis: %v", spec.Profile, err)
			}
			antiThesis, err = convertPosition(out.AntiThesis, table)
			if err != nil {
				return errf("stage %s antiThesis: %v", spec.Profile, err)
			}

		default:
			var out researchOutput
			if err := decodeStage(stdout, &out); err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
			base = out
			if err := registerSources(base.Sources, table); err != nil {
				return errf("stage %s: %v", spec.Profile, err)
			}
		}

		findings, err := convertFindings(base.Findings, table)
		if err != nil {
			return errf("stage %s: %v", spec.Profile, err)
		}
		if spec.Profile == "stock-risk" {
			riskFindings = findings
		}
		results = append(results, stageResult{
			spec:       spec,
			status:     "ok",
			findings:   findings,
			notes:      clipAll(base.Notes, maxStatementLen),
			unresolved: base.UnresolvedQuestions,
			startedAt:  startedAt,
			endedAt:    endedAt,
		})

		// The NEXT stage sees this stage's VALIDATED output, re-serialised by us — never the raw
		// stdout. A stage cannot pass instructions to the next one through a field the schema does
		// not have, because what is forwarded is the decoded struct and nothing else.
		facts = append(facts, factsBlock(spec.Profile, base, findings))
	}

	artifact, err := assembleArtifact(job, table, results, contradictions, veto, chair,
		thesis, antiThesis, riskFindings, degraded, time.Now())
	if err != nil {
		return err
	}
	if err := finalCheck(artifact); err != nil {
		return err
	}

	completeCtx, cancelComplete := withTimeout(runCtx)
	defer cancelComplete()
	return client.Complete(completeCtx, job, artifact)
}

func convertContradictions(in []rawContradiction, table *sourceTable) ([]Contradiction, error) {
	out := make([]Contradiction, 0, len(in))
	for i, c := range in {
		statement := strings.TrimSpace(c.Statement)
		if statement == "" {
			return nil, errf("contradiction %d has no statement", i)
		}
		ids := make([]string, 0, len(c.SourceURLs))
		for _, u := range c.SourceURLs {
			id, err := table.lookup(u)
			if err != nil {
				return nil, errf("contradiction %d: %v", i, err)
			}
			ids = append(ids, id)
		}
		out = append(out, Contradiction{Statement: clip(statement, maxStatementLen), SourceIDs: ids})
	}
	return out, nil
}

func convertPosition(in rawPosition, table *sourceTable) (Position, error) {
	statement := strings.TrimSpace(in.Statement)
	if statement == "" {
		return Position{}, errf("the position has no statement")
	}
	support, err := convertFindings(in.Support, table)
	if err != nil {
		return Position{}, err
	}
	return Position{Statement: clip(statement, maxStatementLen), Support: support}, nil
}

// factsBlock renders one completed stage as the facts the next stage reads. It is built from the
// VALIDATED structures, so an injected instruction that survived into a statement is presented as
// quoted evidence under a heading, not as part of the instruction section of the next prompt.
func factsBlock(profile string, base researchOutput, findings []Finding) string {
	var b strings.Builder
	b.WriteString("### Findings from " + profile + "\n")
	for _, f := range findings {
		b.WriteString("- [" + f.Provenance + "] " + f.Statement)
		if len(f.SourceIDs) > 0 {
			b.WriteString(" (" + strings.Join(f.SourceIDs, ", ") + ")")
		}
		if f.Basis != "" {
			b.WriteString(" [basis: " + f.Basis + "]")
		}
		b.WriteString("\n")
	}
	if len(base.UnresolvedQuestions) > 0 {
		b.WriteString("Unresolved after " + profile + ":\n")
		for _, q := range base.UnresolvedQuestions {
			b.WriteString("- " + strings.TrimSpace(q) + "\n")
		}
	}
	return b.String()
}

// retryableFailure decides whether another attempt could plausibly succeed.
//
// A transport or budget failure is retryable: the network, the provider or the machine was busy. A
// schema, validation, banned-language or leak failure is NOT — the same inputs will produce the
// same refusal, and burning the remaining attempts on it only delays the honest answer.
func retryableFailure(err error) bool {
	msg := err.Error()
	for _, permanent := range []string{
		"does not match its schema",
		"produced no JSON object",
		"unterminated",
		"prescriptive language",
		"will not be uploaded",
		"cites a source the run never declared",
		"provenance",
		"researchPriority",
		// Both of resolveProfileBinary's failure modes. A wrapper that is not on PATH and an
		// override that does not point at an executable are equally unfixable by waiting, so
		// retrying either only burns the attempt cap.
		//
		// The first matches the resolver's message, which names the PROFILE; the second anchors on
		// the ENV VAR NAME rather than the prose, because that name is stable while the wording is
		// not. hermes_test.go asserts both against the resolver's REAL error, so this classifier and
		// those messages cannot drift apart again.
		"no wrapper for profile",
		"ATTESTEL_HERMES_BIN_",
		"refusing to invoke Hermes",
		"carries instructions",
	} {
		if strings.Contains(msg, permanent) {
			return false
		}
	}
	return true
}
