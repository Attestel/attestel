package main

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"
)

// review_assemble.go — turning three validated stage outputs into one uploadable review, and the
// last gate before it crosses the network.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// THE MANDATORY BLOCKERS ARE MERGED IN BY THIS BRIDGE, NOT LEFT TO A MODEL
// ─────────────────────────────────────────────────────────────────────────────────────────────
// The server derived a set of blockers from the snapshot and will REJECT a review that omits one.
// A prompt asking a model to please copy them all would be a prompt that occasionally fails, and
// the failure would look like a clean review with a missing finding — the worst possible shape.
//
// So `assembleReview` merges the server's mandatory blockers into `blockers[]` unconditionally,
// keyed by code, before anything a stage wrote. A stage that raised the same code contributes its
// own statement and paths as an ADDITIONAL entry; a stage that ignored the code entirely changes
// nothing. Dropping a blocker is therefore not merely refused — it is unreachable.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// THE THREE STATUSES ARE COPIED FROM THE SNAPSHOT
// ─────────────────────────────────────────────────────────────────────────────────────────────
// `paperStatus`, `candidateStatus` and `operationalStatus` are read from `snapshot.derived` and are
// never read from a stage. The chair supplies a RATIONALE for each — prose explaining what the
// value means for this deployment — and the value itself is not its to choose. See review.go.

// snapshotIDRE is the exact shape the server mints. Checked before a job is worked so a malformed
// id fails immediately rather than as a confusing 404 mid-run.
var snapshotIDRE = regexp.MustCompile(`^exs_[0-9a-f]{24}$`)

// reviewStageResult is one completed review stage, ready to be assembled.
type reviewStageResult struct {
	spec      stageSpec
	status    string
	notes     []string
	startedAt time.Time
	endedAt   time.Time
}

// assembleReview builds the uploadable artifact. It fills in every field the server requires and
// NOTHING ELSE — the struct has no other fields to fill.
func assembleReview(
	job *ReviewJob,
	snap *ExperimentSnapshot,
	stages []reviewStageResult,
	chair reviewChairOutput,
	blockers, unknowns, contradictions []ReviewClaim,
	nextChecks []ReviewCheck,
	degraded []string,
	now time.Time,
) (*ExperimentReviewArtifact, error) {
	completed := 0
	outStages := make([]ReviewStage, 0, len(stages))
	for _, s := range stages {
		if s.status == "ok" {
			completed++
		}
		outStages = append(outStages, ReviewStage{
			Profile:   s.spec.Profile,
			Status:    s.status,
			Notes:     clipAll(s.notes, maxStatementLen),
			StartedAt: s.startedAt.UTC().Format(time.RFC3339),
			EndedAt:   s.endedAt.UTC().Format(time.RFC3339),
		})
	}

	// THE MERGE. Every mandatory blocker first, in the server's own order and with the server's own
	// statement and paths, then whatever the stages raised. See the header.
	merged := make([]ReviewClaim, 0, len(snap.Derived.MandatoryBlockers)+len(blockers))
	for _, b := range snap.Derived.MandatoryBlockers {
		merged = append(merged, ReviewClaim{
			Code:          b.Code,
			Statement:     clip(strings.TrimSpace(b.Statement), maxStatementLen),
			EvidencePaths: b.EvidencePaths,
		})
	}
	merged = append(merged, blockers...)
	if len(merged) > reviewMaxItems {
		// The server's own blockers are never the ones dropped: they are first in the slice, and a
		// review that shed one would be rejected on arrival.
		merged = merged[:reviewMaxItems]
	}

	summary := clip(strings.TrimSpace(chair.Summary), reviewMaxSummary)
	if summary == "" {
		return nil, errf("the chair returned no summary")
	}

	a := &ExperimentReviewArtifact{
		SchemaVersion:   reviewArtifactSchemaVersion,
		RunID:           job.RunID,
		WorkflowVersion: job.WorkflowVersion,

		SnapshotID:            snap.ID,
		Generation:            snap.Generation,
		Cutoff:                snap.AsOf,
		SnapshotSchemaVersion: snap.SchemaVersion,

		// Echoed from the job, never regenerated — the server compares it against its own
		// server-assigned cutoff, so a worker cannot move the point in time a review answers at.
		AsOf:       job.AsOf,
		ProducedAt: now.UTC().Format(time.RFC3339),

		// COPIED FROM THE SNAPSHOT, all of it. The chair wrote only the rationales.
		//
		// `EvaluatorResult` and `EvidenceValidity` travel beside the conclusion so the UI can show a
		// real `NO EDGE` next to an `unknown` conclusion rather than leaving a reader to dig it out
		// of a blocker. No stage schema has a field for either, so a stage cannot alter them.
		EvaluatorResult:  snap.Derived.EvaluatorResult,
		EvidenceValidity: snap.Derived.EvidenceValidity,
		PaperStatus: ReviewVerdict{
			Value:         snap.Derived.PaperStatus,
			EvidencePaths: snap.Derived.PaperStatusPaths,
			Rationale:     clip(strings.TrimSpace(chair.PaperRationale), maxStatementLen),
		},
		CandidateStatus: ReviewVerdict{
			Value:         snap.Derived.CandidateStatus,
			EvidencePaths: snap.Derived.CandidateStatusPaths,
			Rationale:     clip(strings.TrimSpace(chair.CandidateRationale), maxStatementLen),
		},
		OperationalStatus: ReviewVerdict{
			Value:         snap.Derived.OperationalStatus,
			EvidencePaths: snap.Derived.OperationalStatusPaths,
			Rationale:     clip(strings.TrimSpace(chair.OperationalRationale), maxStatementLen),
		},

		Blockers:       merged,
		Unknowns:       unknowns,
		Contradictions: contradictions,
		NextChecks:     nextChecks,

		Summary:  summary,
		Stages:   outStages,
		Degraded: degraded,
		Identity: ReviewIdentity{
			WorkflowVersion:       job.WorkflowVersion,
			ArtifactSchemaVersion: reviewArtifactSchemaVersion,
			Profiles:              reviewProfileNames(),
			StagesCompleted:       completed,
			BridgeVersion:         bridgeVersion,
		},
	}

	// Every rationale is required: a status with no explanation is the one place a reader would
	// have to take the value on trust.
	for label, v := range map[string]ReviewVerdict{
		"paperStatus": a.PaperStatus, "candidateStatus": a.CandidateStatus,
		"operationalStatus": a.OperationalStatus,
	} {
		if v.Rationale == "" {
			return nil, errf("the chair gave no rationale for %s", label)
		}
		if len(v.EvidencePaths) == 0 {
			return nil, errf("the snapshot supplied no evidence paths for %s", label)
		}
	}

	a.EvidenceReferences = collectEvidenceReferences(a, snapshotPaths(snap))
	if len(a.EvidenceReferences) == 0 {
		return nil, errf("the review cites no evidence at all")
	}
	return a, nil
}

// collectEvidenceReferences is the de-duplicated union of every path the review actually leaned on.
//
// `presence` is computed against the snapshot's real index rather than asserted: a path that is in
// the index is `present`, and one that somehow is not is `absent`. In practice every path has
// already been validated, so this records `present` — but computing it rather than hard-coding it
// means the field states a fact instead of a hope.
func collectEvidenceReferences(a *ExperimentReviewArtifact, index map[string]bool) []EvidenceReference {
	seen := map[string]bool{}
	add := func(paths []string) {
		for _, p := range paths {
			seen[p] = true
		}
	}
	add(a.PaperStatus.EvidencePaths)
	add(a.CandidateStatus.EvidencePaths)
	add(a.OperationalStatus.EvidencePaths)
	for _, group := range [][]ReviewClaim{a.Blockers, a.Unknowns, a.Contradictions} {
		for _, c := range group {
			add(c.EvidencePaths)
		}
	}
	for _, c := range a.NextChecks {
		add(c.EvidencePaths)
	}

	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := make([]EvidenceReference, 0, len(paths))
	for _, p := range paths {
		presence := "absent"
		if index[p] {
			presence = "present"
		}
		out = append(out, EvidenceReference{Path: p, Presence: presence})
	}
	return out
}

// reviewProfileNames is the review chain's profile list, in order, for the artifact's identity
// block. The server checks it against its own copy (journal/agency_workflows.go).
func reviewProfileNames() []string {
	out := make([]string, 0, len(experimentReviewChain))
	for _, s := range experimentReviewChain {
		out = append(out, s.Profile)
	}
	return out
}

// finalReviewCheck is the last gate before upload: the content scans, then the size cap.
//
// IT USES THE SAME LEAK AND BANNED-LANGUAGE TABLES the research lane uses. A leak is a leak and a
// recommendation is a recommendation whichever workflow produced it, and two divergent copies of
// one rule is how the weaker copy becomes the one that matters.
func finalReviewCheck(a *ExperimentReviewArtifact) error {
	texts := reviewStrings(a)
	for _, text := range texts {
		for _, p := range leakPatterns {
			if p.re.MatchString(text) {
				return errf("the review contains %s and will not be uploaded", p.reason)
			}
		}
	}
	for _, text := range texts {
		normalised := strings.Join(strings.Fields(text), " ")
		for _, re := range bannedPhrases {
			if m := re.FindString(normalised); m != "" {
				return errf("the agents produced prescriptive language (%q); this workflow explains "+
					"an experiment and never produces a recommendation, a target or a position", m)
			}
		}
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		return errf("the review could not be encoded")
	}
	if len(encoded) > maxArtifactBytes {
		return errf("the review is %d bytes; the limit is %d", len(encoded), maxArtifactBytes)
	}
	return nil
}

// reviewStrings flattens EVERY free-text string the review carries, so neither scan can be evaded
// by putting the text in a field nobody remembered to check.
//
// There is no quoted/authored split here and there should not be: a review of this deployment's own
// snapshot quotes no third party, so every sentence in it was composed by a stage and every
// sentence is scanned. Mirrors journal/experiment_review.go's `reviewArtifactStrings`.
func reviewStrings(a *ExperimentReviewArtifact) []string {
	if a == nil {
		return nil
	}
	var out []string
	add := func(vs ...string) { out = append(out, vs...) }
	addClaims := func(items []ReviewClaim) {
		for _, c := range items {
			add(c.Statement)
		}
	}
	add(a.PaperStatus.Rationale, a.CandidateStatus.Rationale, a.OperationalStatus.Rationale)
	addClaims(a.Blockers)
	addClaims(a.Unknowns)
	addClaims(a.Contradictions)
	for _, c := range a.NextChecks {
		add(c.Statement)
	}
	add(a.Summary)
	for _, st := range a.Stages {
		add(st.Notes...)
	}
	add(a.Degraded...)
	return out
}
