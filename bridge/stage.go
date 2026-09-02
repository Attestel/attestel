package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
)

// stage.go — the strict schema every Hermes stage's output must satisfy, and the assembly of four
// validated stage outputs into one artifact.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// EVERYTHING A STAGE PRODUCES IS UNTRUSTED INPUT
// ─────────────────────────────────────────────────────────────────────────────────────────────
// A research stage reads the open web. Any page it reads may contain text addressed to the agent,
// and the honest assumption is that one eventually will. So a stage's stdout is treated exactly
// like a response from a hostile server:
//
//   - it is decoded into a CLOSED struct with DisallowUnknownFields, so a field the schema does not
//     declare is a refusal rather than a silently carried value;
//   - every enum is checked against a fixed set;
//   - every citation must resolve to a source the run actually declared, and every URL must be an
//     absolute http(s) URL with no embedded credentials;
//   - a `sourced` claim with no citation is rejected outright. "Missing citations" is one of the
//     fail-closed conditions, and this is where it closes;
//   - the assembled result is scanned for prescriptive language and for leaks before it is uploaded.
//
// A stage that fails any of these fails the RUN. It is not repaired, not partially accepted and not
// summarised into something acceptable: a research record the owner will rely on must be what the
// agents actually produced or nothing at all.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// UNKNOWN IS A FIRST-CLASS ANSWER
// ─────────────────────────────────────────────────────────────────────────────────────────────
// `provenance: "unknown"` exists so a stage can say "this was not established" instead of
// inventing something that satisfies a schema. The prompts instruct the agents to use it and the
// validator makes it costless: an `unknown` finding needs no citation and no basis. A schema that
// only accepted confident answers would be a schema that manufactured them.

// rawSource is a citation as a stage reports it. Stages cite by URL; the bridge assigns the stable
// s1..sN ids across the whole run, so no stage has to manage an id namespace it cannot see.
type rawSource struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	PublishedAt string `json:"publishedAt"`
	Publisher   string `json:"publisher"`
}

type rawFinding struct {
	Statement  string   `json:"statement"`
	Provenance string   `json:"provenance"`
	SourceURLs []string `json:"sourceUrls"`
	Basis      string   `json:"basis"`
}

type rawContradiction struct {
	Statement  string   `json:"statement"`
	SourceURLs []string `json:"sourceUrls"`
}

type rawPosition struct {
	Statement string       `json:"statement"`
	Support   []rawFinding `json:"support"`
}

type rawVeto struct {
	Raised  bool     `json:"raised"`
	Reasons []string `json:"reasons"`
}

// researchOutput is what the scout and fundamentals stages return.
type researchOutput struct {
	Sources             []rawSource  `json:"sources"`
	Findings            []rawFinding `json:"findings"`
	Notes               []string     `json:"notes"`
	UnresolvedQuestions []string     `json:"unresolvedQuestions"`
}

// riskOutput adds the two things only the risk stage may produce: contradictions between sources,
// and the veto. Note there is still no field for a direction — a risk stage that concluded "sell"
// has nowhere to write it.
type riskOutput struct {
	researchOutput
	Contradictions []rawContradiction `json:"contradictions"`
	Veto           rawVeto            `json:"veto"`
}

// chairOutput adds the synthesis. `researchPriority` is the strongest verdict this whole workflow
// can express, and its vocabulary is investigate / watch / reject / unknown — four RESEARCH
// instructions. There is deliberately no fifth value and no numeric score.
type chairOutput struct {
	researchOutput
	Thesis            rawPosition `json:"thesis"`
	AntiThesis        rawPosition `json:"antiThesis"`
	Conclusion        string      `json:"conclusion"`
	KeyRisks          []string    `json:"keyRisks"`
	WhatWouldChangeIt []string    `json:"whatWouldChangeIt"`
	ResearchPriority  string      `json:"researchPriority"`
}

// ─────────────────────────────────────────────────────────────────────────────── decoding stdout

// extractJSONObject pulls the first balanced top-level JSON object out of a stage's stdout.
//
// Hermes with `--quiet` prints only the final response, but a model may still wrap it in a fenced
// code block or add a sentence before it. Scanning for a balanced object is tolerant of that
// without being tolerant of anything else: the object that comes back still has to satisfy the
// closed schema. Quoted strings and escapes are tracked so a `}` inside a citation title does not
// terminate the scan early.
func extractJSONObject(out string) (string, error) {
	start := strings.IndexByte(out, '{')
	if start < 0 {
		return "", errf("the stage produced no JSON object")
	}
	depth, inString, escaped := 0, false, false
	for i := start; i < len(out); i++ {
		c := out[i]
		if escaped {
			escaped = false
			continue
		}
		switch {
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// nothing else is structural inside a string
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return out[start : i+1], nil
			}
		}
	}
	return "", errf("the stage's JSON object is unterminated")
}

// decodeStage decodes one stage's stdout into `target` with unknown fields refused.
func decodeStage(stdout string, target any) error {
	raw, err := extractJSONObject(stdout)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return errf("the stage's output does not match its schema: %v", err)
	}
	return nil
}

// ────────────────────────────────────────────────────────────────────── the run-wide source table

// sourceTable assigns stable s1..sN ids by normalised URL, so the same page cited by two stages is
// one source in the artifact rather than two.
type sourceTable struct {
	byURL   map[string]string
	sources []Source
}

func newSourceTable() *sourceTable {
	return &sourceTable{byURL: map[string]string{}}
}

// normaliseURL lowercases the scheme and host and drops a trailing slash, which is enough to merge
// the ordinary duplicates without pretending two different query strings are one page.
func normaliseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errf("the url is not parseable")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errf("the url must be http or https")
	}
	if u.Host == "" {
		return "", errf("the url has no host")
	}
	if u.User != nil {
		return "", errf("the url carries credentials")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	s := u.String()
	return strings.TrimRight(s, "/"), nil
}

// add registers a source and returns its id. A URL already known keeps its first id and its first
// metadata — the first stage to cite something is the one whose description is kept, so a later
// stage cannot rewrite an earlier citation.
func (t *sourceTable) add(src rawSource) (string, error) {
	key, err := normaliseURL(src.URL)
	if err != nil {
		return "", err
	}
	if id, ok := t.byURL[key]; ok {
		return id, nil
	}
	if len(t.sources) >= maxSources {
		return "", errf("the run cited more than %d sources", maxSources)
	}
	title := strings.TrimSpace(src.Title)
	if title == "" {
		return "", errf("a source has no title")
	}
	published := strings.TrimSpace(src.PublishedAt)
	if err := validateSourceDate(published); err != nil {
		return "", err
	}
	id := "s" + itoa(len(t.sources)+1)
	t.byURL[key] = id
	t.sources = append(t.sources, Source{
		ID:          id,
		Title:       clip(title, maxStatementLen),
		URL:         key,
		PublishedAt: published,
		Publisher:   clip(strings.TrimSpace(src.Publisher), 200),
	})
	return id, nil
}

// lookup resolves a cited URL to an id, refusing one that was never declared. A citation that
// points at nothing is not a citation, and accepting it would make the provenance labels
// decorative.
func (t *sourceTable) lookup(raw string) (string, error) {
	key, err := normaliseURL(raw)
	if err != nil {
		return "", err
	}
	id, ok := t.byURL[key]
	if !ok {
		return "", errf("a finding cites a source the run never declared")
	}
	return id, nil
}

// validateSourceDate mirrors the server's rule: YYYY-MM-DD, RFC3339, or the literal "unknown".
func validateSourceDate(v string) error {
	if v == provenanceUnknown {
		return nil
	}
	if _, ok := parseSourceDate(v); ok {
		return nil
	}
	return errf(`a source's publishedAt must be YYYY-MM-DD, RFC3339, or the literal "unknown"`)
}

const (
	maxSources        = 40
	maxFindingsPerBox = 40
	maxStatementLen   = 2000
)

// convertFindings validates and converts a stage's findings.
func convertFindings(in []rawFinding, table *sourceTable) ([]Finding, error) {
	if len(in) > maxFindingsPerBox {
		return nil, errf("a stage returned %d findings; the limit is %d", len(in), maxFindingsPerBox)
	}
	out := make([]Finding, 0, len(in))
	for i, f := range in {
		statement := strings.TrimSpace(f.Statement)
		if statement == "" {
			return nil, errf("finding %d has no statement", i)
		}
		if len(statement) > maxStatementLen {
			return nil, errf("finding %d is %d characters; the limit is %d",
				i, len(statement), maxStatementLen)
		}
		basis := strings.TrimSpace(f.Basis)
		switch f.Provenance {
		case provenanceSourced:
			if len(f.SourceURLs) == 0 {
				return nil, errf("finding %d is marked sourced but cites nothing", i)
			}
			if basis != "" {
				return nil, errf("finding %d is sourced but carries a calculation basis", i)
			}
		case provenanceCalculated:
			if basis == "" {
				return nil, errf("finding %d is marked calculated but shows no basis", i)
			}
			if len(f.SourceURLs) == 0 {
				return nil, errf("finding %d is calculated but cites no inputs", i)
			}
		case provenanceInferred, provenanceUnknown:
			if basis != "" {
				return nil, errf("finding %d is %s but carries a calculation basis", i, f.Provenance)
			}
		default:
			return nil, errf("finding %d has provenance %q; expected sourced, calculated, "+
				"inferred or unknown", i, f.Provenance)
		}

		ids := make([]string, 0, len(f.SourceURLs))
		for _, u := range f.SourceURLs {
			id, err := table.lookup(u)
			if err != nil {
				return nil, errf("finding %d: %v", i, err)
			}
			if !containsString(ids, id) {
				ids = append(ids, id)
			}
		}
		out = append(out, Finding{
			Statement:  statement,
			Provenance: f.Provenance,
			SourceIDs:  ids,
			Basis:      clip(basis, maxStatementLen),
		})
	}
	return out, nil
}

// registerSources adds a stage's declared sources to the run-wide table.
func registerSources(in []rawSource, table *sourceTable) error {
	for i, s := range in {
		if _, err := table.add(s); err != nil {
			return errf("source %d: %v", i, err)
		}
	}
	return nil
}

// ────────────────────────────────────────────────────────────────────────────── artifact assembly

// stageResult is one completed stage, ready to be assembled.
type stageResult struct {
	spec       stageSpec
	status     string
	findings   []Finding
	notes      []string
	unresolved []string
	startedAt  time.Time
	endedAt    time.Time
}

// assembleArtifact builds the uploadable artifact from four validated stage results plus the risk
// and chair extras. It fills in every field the server requires and NOTHING ELSE — the struct has
// no other fields to fill.
func assembleArtifact(
	job *Job,
	table *sourceTable,
	stages []stageResult,
	contradictions []Contradiction,
	veto Veto,
	chair chairOutput,
	thesis, antiThesis Position,
	riskFindings []Finding,
	degraded []string,
	now time.Time,
) (*Artifact, error) {
	completed := 0
	outStages := make([]Stage, 0, len(stages))
	unresolved := []string{}
	seenUnresolved := map[string]bool{}
	for _, s := range stages {
		if s.status == "ok" {
			completed++
		}
		outStages = append(outStages, Stage{
			Profile:   s.spec.Profile,
			Status:    s.status,
			Findings:  s.findings,
			Notes:     s.notes,
			StartedAt: s.startedAt.UTC().Format(time.RFC3339),
			EndedAt:   s.endedAt.UTC().Format(time.RFC3339),
		})
		for _, q := range s.unresolved {
			q = strings.TrimSpace(q)
			if q == "" || seenUnresolved[q] {
				continue
			}
			seenUnresolved[q] = true
			unresolved = append(unresolved, clip(q, maxStatementLen))
		}
	}

	priority := strings.TrimSpace(chair.ResearchPriority)
	if !containsString(researchPriorities, priority) {
		return nil, errf("the chair returned researchPriority %q; expected one of %s",
			priority, strings.Join(researchPriorities, ", "))
	}
	if veto.Raised && len(veto.Reasons) == 0 {
		return nil, errf("the risk stage raised a veto without stating a reason")
	}

	sources := append([]Source(nil), table.sources...)
	sort.SliceStable(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })

	a := &Artifact{
		SchemaVersion:   artifactSchemaVersion,
		RunID:           job.RunID,
		WorkflowVersion: job.WorkflowVersion,
		Ticker:          job.Ticker,
		Question:        job.Question,
		// Echoed from the job, never regenerated. The server compares it against its own
		// server-assigned cutoff, so a worker cannot move the point in time the research answers.
		AsOf:       job.AsOf,
		ProducedAt: now.UTC().Format(time.RFC3339),

		Sources:             sources,
		Stages:              outStages,
		UnresolvedQuestions: unresolved,
		Contradictions:      contradictions,

		Thesis:       thesis,
		AntiThesis:   antiThesis,
		RiskFindings: riskFindings,
		Chair: Chair{
			Conclusion:        clip(strings.TrimSpace(chair.Conclusion), maxStatementLen),
			KeyRisks:          clipAll(chair.KeyRisks, maxStatementLen),
			WhatWouldChangeIt: clipAll(chair.WhatWouldChangeIt, maxStatementLen),
		},
		ResearchPriority: priority,
		Veto: Veto{
			Raised:  veto.Raised,
			Scope:   vetoScope,
			Reasons: clipAll(veto.Reasons, maxStatementLen),
		},
		Identity: Identity{
			WorkflowVersion:       job.WorkflowVersion,
			ArtifactSchemaVersion: artifactSchemaVersion,
			Profiles:              profileNames(),
			StagesCompleted:       completed,
			BridgeVersion:         bridgeVersion,
		},
		Degraded: degraded,
	}
	if strings.TrimSpace(a.Chair.Conclusion) == "" {
		return nil, errf("the chair returned no conclusion")
	}
	if strings.TrimSpace(a.Thesis.Statement) == "" || strings.TrimSpace(a.AntiThesis.Statement) == "" {
		return nil, errf("the chair returned no thesis or no anti-thesis")
	}
	return a, nil
}

const (
	// Citation-coverage floors. KEEP IN STEP WITH journal/agency.go's `agencyMinGroundedFindings`
	// and `agencyMinGroundedFraction` — the server applies the same rule, and a worker that used a
	// weaker one would simply produce artifacts the server rejects.
	minGroundedFindings = 2
	minGroundedFraction = 0.25
)

// groundedCoverage counts DISTINCT CLAIMS, not appearances. Mirrors journal/agency.go's
// `agencyGroundedCoverage`, including which lists it walks and how it deduplicates.
//
// The chair's thesis support properly repeats findings the scout already reported, and the risk
// stage restates what it is attacking. Counting those as separate evidence would let one sourced
// sentence, echoed across four stages, clear a floor meant to say "this rests on more than one
// thing". A claim counts as grounded if ANY of its appearances is sourced or calculated.
func groundedCoverage(a *Artifact) (grounded, total int) {
	seen := map[string]bool{}
	order := make([]string, 0, 16)
	count := func(fs []Finding) {
		for _, f := range fs {
			key := claimKey(f.Statement)
			if key == "" {
				continue
			}
			isGrounded := f.Provenance == provenanceSourced || f.Provenance == provenanceCalculated
			if _, ok := seen[key]; !ok {
				order = append(order, key)
				seen[key] = isGrounded
				continue
			}
			seen[key] = seen[key] || isGrounded
		}
	}
	for _, st := range a.Stages {
		count(st.Findings)
	}
	count(a.Thesis.Support)
	count(a.AntiThesis.Support)
	count(a.RiskFindings)

	for _, key := range order {
		total++
		if seen[key] {
			grounded++
		}
	}
	return grounded, total
}

// claimKey normalises a statement for deduplication. Mirrors journal/agency.go's `agencyClaimKey`:
// whitespace collapsed, case folded, trailing punctuation dropped. Conservative on purpose — it
// merges restatements, not paraphrases.
func claimKey(statement string) string {
	key := strings.ToLower(strings.Join(strings.Fields(statement), " "))
	return strings.TrimRight(key, ".!? ")
}

// checkCoverage refuses a research-positive outcome that rests on nothing.
//
// `investigate` and `watch` claim there is something here. If the chair reaches one of them with no
// sources, or with almost everything marked `inferred`, the honest outcome is `unknown` — which
// costs nothing and is accepted everywhere. Caught locally so the run fails with a reason the
// operator can act on rather than as a remote 400.
func checkCoverage(a *Artifact) error {
	if a.ResearchPriority != "investigate" && a.ResearchPriority != "watch" {
		return nil
	}
	if len(a.Sources) == 0 {
		return errf("the chair returned researchPriority %q with no sources at all; an artifact "+
			"that cites nothing must report \"unknown\"", a.ResearchPriority)
	}
	grounded, total := groundedCoverage(a)
	if grounded < minGroundedFindings {
		return errf("the chair returned researchPriority %q resting on %d DISTINCT sourced or "+
			"calculated finding(s); at least %d are required, or the run must report \"unknown\". "+
			"Repeating one claim across stages does not make it two",
			a.ResearchPriority, grounded, minGroundedFindings)
	}
	if total > 0 && float64(grounded)/float64(total) < minGroundedFraction {
		return errf("the chair returned researchPriority %q resting on %d grounded claim(s) out of "+
			"%d distinct; at least %.0f%% must rest on a source, or the run must report \"unknown\"",
			a.ResearchPriority, grounded, total, minGroundedFraction*100)
	}
	return nil
}

// checkCitationDates refuses a source dated after the run's own cutoff.
//
// A citation that postdates the point in time the research answers at is either a hallucinated date
// or research that reached past its cutoff. Either way the artifact stops being a point-in-time
// record, which is the property that would let these snapshots ever become model features without
// look-ahead. Day-granularity dates compare at the start of their day, so a source dated the same
// day as the cutoff is accepted.
func checkCitationDates(a *Artifact) error {
	cutoff, err := time.Parse(time.RFC3339, a.AsOf)
	if err != nil {
		return errf("the artifact's asOf is not an RFC3339 timestamp")
	}
	for _, s := range a.Sources {
		published, ok := parseSourceDate(s.PublishedAt)
		if !ok {
			continue // "unknown" is a real answer
		}
		if published.After(cutoff) {
			return errf("source %s is dated %s, after this run's cutoff of %s — a citation cannot "+
				"postdate the point in time the research answers at", s.ID, s.PublishedAt, a.AsOf)
		}
	}
	return nil
}

// parseSourceDate returns the instant a date names, and whether it named one. Mirrors
// journal/agency.go's `parseAgencySourceDate`.
func parseSourceDate(raw string) (time.Time, bool) {
	v := strings.TrimSpace(raw)
	if v == "" || v == provenanceUnknown {
		return time.Time{}, false
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// finalCheck is the last gate before upload: research quality, point-in-time integrity, the two
// content scans, then the size cap.
func finalCheck(a *Artifact) error {
	if err := checkCoverage(a); err != nil {
		return err
	}
	if err := checkCitationDates(a); err != nil {
		return err
	}
	if fragment := scanArtifactForBannedLanguage(a); fragment != "" {
		return errf("the agents produced prescriptive language (%q); this workflow returns "+
			"research, never a recommendation, a target or a position", fragment)
	}
	if reason := scanArtifactForLeaks(a); reason != "" {
		return errf("the artifact contains %s and will not be uploaded", reason)
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		return errf("the artifact could not be encoded")
	}
	if len(encoded) > maxArtifactBytes {
		return errf("the artifact is %d bytes; the limit is %d", len(encoded), maxArtifactBytes)
	}
	return nil
}

const maxArtifactBytes = 256 << 10

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func clipAll(in []string, n int) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, clip(s, n))
		}
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}
