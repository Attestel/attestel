package main

import (
	"embed"
	"os"
	"path/filepath"
	"strings"
)

// prompt.go — the stage prompts, and the rule that the hosted side never writes one.
//
// THE PROMPTS ARE EMBEDDED IN THIS BINARY. They ship with the bridge, they live on the owner's
// machine, and a job cannot supply, extend, override or select one. `renderPrompt` substitutes
// exactly three values into a fixed template — the ticker, the question and the cutoff — and the
// question arrives inside a clearly delimited, explicitly-untrusted block.
//
// AN OPERATOR MAY OVERRIDE THE TEMPLATES LOCALLY via ATTESTEL_BRIDGE_PROMPT_DIR. That is a local
// choice made by the machine's owner, which is a different thing entirely from a hosted deployment
// choosing one. A missing or unreadable override falls back to the embedded copy rather than
// failing: a prompt directory that vanished should degrade to the shipped behaviour.
//
// PROMPT INJECTION IS HANDLED IN FOUR PLACES AND THIS IS ONLY THE FIRST. Instructions in a fetched
// page are text; text can persuade a model. So the real defences are structural and live elsewhere:
// the artifact schema has no field for a signal (schema.go), the chain and toolsets are fixed
// (hermes.go), every stage output is decoded into a closed struct (stage.go), and the server
// validates again on receipt (journal/agency.go). What this file contributes is the one thing a
// prompt genuinely can do: tell the agent what its output must look like, and mark the untrusted
// regions as untrusted.

//go:embed prompts/*.md
var embeddedPrompts embed.FS

// renderPrompt builds one stage's query file.
//
// `facts` are the validated outputs of the earlier stages, already re-serialised by us from decoded
// structs (run.go::factsBlock) — never a stage's raw stdout. The substitution is a plain string
// replace of three named placeholders; there is no template language here, because a template
// language evaluated over untrusted text is an evaluator over untrusted text.
func renderPrompt(cfg Config, spec stageSpec, job *Job, facts []string) (string, error) {
	tpl, err := loadPromptTemplate(cfg, spec.PromptFile)
	if err != nil {
		return "", err
	}
	priorFacts := strings.TrimSpace(strings.Join(facts, "\n"))
	if priorFacts == "" {
		priorFacts = "(none — you are the first stage in this workflow)"
	}
	out := tpl
	out = strings.ReplaceAll(out, "{{TICKER}}", job.Ticker)
	out = strings.ReplaceAll(out, "{{AS_OF}}", job.AsOf)
	out = strings.ReplaceAll(out, "{{QUESTION}}", job.Question)
	out = strings.ReplaceAll(out, "{{PRIOR_FACTS}}", priorFacts)
	return out, nil
}

func loadPromptTemplate(cfg Config, name string) (string, error) {
	if cfg.PromptDir != "" {
		// filepath.Base pins the lookup to the directory: `name` comes from the hard-coded chain in
		// hermes.go and cannot contain a traversal, but the constraint is cheap and it means a
		// future edit to the chain cannot introduce one either.
		path := filepath.Join(cfg.PromptDir, filepath.Base(name))
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			return string(raw), nil
		}
	}
	raw, err := embeddedPrompts.ReadFile("prompts/" + name)
	if err != nil {
		return "", errf("the prompt template %q is missing from this build", name)
	}
	return string(raw), nil
}

// renderReviewPrompt builds one review stage's query file.
//
// FOUR SUBSTITUTIONS, ALL OF THEM LOCAL FACTS OR THE FROZEN SNAPSHOT. There is no owner-typed free
// text anywhere in a review — the job carries a snapshot id and nothing else — so the untrusted
// region here is the SNAPSHOT itself, and it is marked as data for the same reason a fetched page
// is: it is a document, and a document is never an instruction.
//
// The snapshot is genuinely lower-risk than a web page (this deployment assembled it from its own
// services), but treating it as trusted would mean a compromised paper service could write
// instructions into a `detail` string and have a stage follow them. The delimiters cost nothing.
func renderReviewPrompt(cfg Config, spec stageSpec, job *ReviewJob, snap *ExperimentSnapshot, evidence string, facts []string) (string, error) {
	tpl, err := loadPromptTemplate(cfg, spec.PromptFile)
	if err != nil {
		return "", err
	}
	priorFacts := strings.TrimSpace(strings.Join(facts, "\n"))
	if priorFacts == "" {
		priorFacts = "(none — you are the first stage in this workflow)"
	}
	out := tpl
	out = strings.ReplaceAll(out, "{{SNAPSHOT_ID}}", snap.ID)
	out = strings.ReplaceAll(out, "{{GENERATION}}", itoa64(snap.Generation))
	out = strings.ReplaceAll(out, "{{CUTOFF}}", snap.AsOf)
	out = strings.ReplaceAll(out, "{{REVISION}}", snap.Revision)
	out = strings.ReplaceAll(out, "{{PAPER_STATUS}}", snap.Derived.PaperStatus)
	out = strings.ReplaceAll(out, "{{CANDIDATE_STATUS}}", snap.Derived.CandidateStatus)
	out = strings.ReplaceAll(out, "{{OPERATIONAL_STATUS}}", snap.Derived.OperationalStatus)
	out = strings.ReplaceAll(out, "{{EVIDENCE}}", evidence)
	out = strings.ReplaceAll(out, "{{PRIOR_FACTS}}", priorFacts)
	// The run id is never substituted into a prompt: a stage has no use for it and it is one more
	// identifier that would end up in a model's context for no reason.
	_ = job
	return out, nil
}

// itoa64 renders an int64 without importing strconv into this zero-dependency module's prompt path.
func itoa64(n int64) string {
	if n >= 0 && n <= 1<<31-1 {
		return itoa(int(n))
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if i == len(buf) {
		i--
		buf[i] = '0'
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
