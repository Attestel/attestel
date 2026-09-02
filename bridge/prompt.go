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
