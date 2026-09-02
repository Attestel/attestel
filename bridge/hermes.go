package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// hermes.go — the fixed mapping from `company_research_v1` to what actually runs, and the only
// place in this repository that starts a process.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// THE INVOCATION, AND WHY IT IS THIS ONE
// ─────────────────────────────────────────────────────────────────────────────────────────────
//
//	<profile-wrapper> chat --query-file <file> --oneshot -Q \
//	    --in <workdir> --max-turns N --run-budget S -t <toolsets> --source tool
//
// `--query-file`, not `-q`. Hermes' own help for it: "Read the single query from a file instead of
// the command line ('-' reads stdin). Safe for arbitrary text: nothing is shell-interpreted, so
// quotes, $(...), and backticks are preserved verbatim." The research question is the one piece of
// hosted-controlled text that reaches the agent, and this is the input path where that text cannot
// become an argument, a quote, or a subshell. It is also why `exec.Command` is given an explicit
// argv slice and never a shell string: there is no shell in this path at all.
//
// NOT `hermes -z/--oneshot`, WHICH IS THE OBVIOUS CHOICE AND THE WRONG ONE. The top-level `-z`
// flag's own documentation says "approvals are auto-bypassed". That makes it equivalent to --yolo
// for approval purposes on a headless run, and this bridge must never use an invocation that
// automatically bypasses dangerous-command approvals. `hermes chat --oneshot` is the subcommand
// form and carries no such clause: it selects single-query behaviour and nothing else.
//
// `--yolo` APPEARS NOWHERE IN THIS FILE OR THIS MODULE, and `hermes_test.go` asserts that against
// the source text — the same source-level assertion services/llm/tests/test_enrich_worker.py makes
// about its own forbidden spellings. `--accept-hooks` is refused on the same grounds: it
// auto-approves unseen shell hooks without a prompt.
//
// WHY NOT `--ignore-rules`, WHICH LOOKS LIKE THE SAFE CHOICE. It skips injection of AGENTS.md,
// SOUL.md, memory and preloaded skills — and a Hermes PROFILE is defined by exactly those files.
// Passing it would silently neuter `stock-scout` into a generic agent while appearing to run it,
// which is worse than the risk it addresses. The risk it addresses — an AGENTS.md in the working
// directory injecting instructions — is handled properly instead: `--in` points at a
// bridge-created, bridge-owned scratch directory that contains one file, the query, and
// `assertCleanWorkdir` refuses to start if any instruction-shaped file has appeared in it.
//
// PROFILE SELECTION IS LOCAL, AND IT IS NOT A FLAG. `hermes chat` has no `--profile`; the
// mechanisms are a sticky global default (`hermes profile use`, which is mutable global state and
// therefore wrong for a worker) and per-profile wrapper scripts (`hermes profile alias`). This
// bridge takes the wrapper: the profile is part of the EXECUTABLE NAME, the four names are a
// hard-coded map below, and a job cannot name one. The operator creates them once:
//
//	hermes profile alias stock-scout        # and the other three
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// EVERYTHING THE HOSTED SIDE CANNOT INFLUENCE
// ─────────────────────────────────────────────────────────────────────────────────────────────
// The profile, the executable, the toolsets, the turn cap, the time budget, the working directory,
// the prompt template, the model, the provider and the reasoning level. None of them is a field on
// `Job` (see schema.go), and none of them is derived from one. The only hosted values that reach a
// Hermes process are the ticker and the question, and they reach it inside a file, after being
// re-validated locally.

// stageSpec is one step of the chain. The whole workflow is these four values, in this order.
type stageSpec struct {
	// Profile is the Hermes profile name AND the default wrapper executable name.
	Profile string
	// Toolsets is passed verbatim to `-t`. Narrow on purpose: the three research stages need to
	// read the web, and the chair synthesises what they already found.
	Toolsets string
	// MaxTurns bounds tool-calling iterations for this stage. Hermes' default is 500, which is an
	// interactive default; a research stage that has not finished in this many turns is looping.
	MaxTurns int
	// PromptFile is the template in the bridge's prompt directory.
	PromptFile string
}

// companyResearchChain is THE mapping. Changing it changes what `company_research_v1` means, which
// is a versioned contract — bump `workflowCompanyResearch` if the shape of what runs changes, not
// just the wording of a prompt.
//
// The order is load-bearing: each stage receives the validated JSON of every stage before it as
// FACTS, so the chair sees what the scout found and the risk stage sees both. That is the same
// sequential, one-model-at-a-time shape services/llm/app/committee.py already uses, and for the
// same reason — a single local model cannot serve two generations at once.
var companyResearchChain = []stageSpec{
	{Profile: "stock-scout", Toolsets: "web", MaxTurns: 24, PromptFile: "stock-scout.md"},
	{Profile: "stock-fundamentals", Toolsets: "web", MaxTurns: 24, PromptFile: "stock-fundamentals.md"},
	{Profile: "stock-risk", Toolsets: "web", MaxTurns: 24, PromptFile: "stock-risk.md"},
	{Profile: "stock-chair", Toolsets: "web", MaxTurns: 12, PromptFile: "stock-chair.md"},
}

// profileNames is the chain's profile list, in order, for the artifact's identity block. The server
// checks it against its own copy (journal/agency.go::agencyProfileChain).
func profileNames() []string {
	out := make([]string, 0, len(companyResearchChain))
	for _, s := range companyResearchChain {
		out = append(out, s.Profile)
	}
	return out
}

// instructionFileNames are the files Hermes auto-injects from the working directory. The scratch
// directory must contain none of them: a research run must not be steerable by anything that
// happens to be on disk where it runs.
var instructionFileNames = []string{
	"AGENTS.md", "SOUL.md", "HERMES.md", "CLAUDE.md", ".cursorrules", ".env",
}

// forbiddenFlags may never appear in an argv this bridge builds. Checked at construction time in
// addition to simply not being written, so a future edit that adds one fails a test rather than
// shipping.
var forbiddenFlags = []string{"--yolo", "--accept-hooks", "-z"}

// hermesRunner is the seam the integration test replaces. Production uses execRunner; the test uses
// a deterministic stub so a full queue-to-artifact run needs no model, no network and no provider
// credential. The stub is reported in the artifact's `degraded` list, so a stubbed run can never be
// mistaken for a real one.
type hermesRunner interface {
	Run(ctx context.Context, spec stageSpec, workdir, queryPath string, cfg Config) (string, error)
}

type execRunner struct{}

// Run executes one stage and returns its stdout.
//
// FOUR BOUNDS, AND THEY ARE INDEPENDENT:
//  1. `--max-turns` caps tool-calling iterations inside the agent;
//  2. `--run-budget` gives the agent a wall-clock budget it winds down against;
//  3. the context deadline kills the process if it ignores (2) — a budget the child honours
//     voluntarily is not a bound;
//  4. stdout is capped, so a runaway stage cannot exhaust memory on the way to being killed.
func (execRunner) Run(ctx context.Context, spec stageSpec, workdir, queryPath string, cfg Config) (string, error) {
	bin, err := resolveProfileBinary(spec.Profile)
	if err != nil {
		return "", err
	}
	if err := assertCleanWorkdir(workdir); err != nil {
		return "", err
	}

	args := buildHermesArgs(spec, workdir, queryPath, cfg)
	if bad := containsForbiddenFlag(args); bad != "" {
		// Defence in depth against a future edit. A bridge that can be made to bypass approvals by
		// a one-line change should fail loudly at the moment of the change.
		return "", errf("refusing to invoke Hermes with %s", bad)
	}

	stageCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StageBudgetSeconds)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(stageCtx, bin, args...)
	cmd.Dir = workdir
	// The environment is inherited MINUS the worker credential, in BOTH its forms.
	//
	// `config.go` already unsets both from this process, so this is defence in depth — and it is
	// worth having, because the two mechanisms fail differently. The unset protects against an
	// agent reading its own environment; this filter protects against a code path that never went
	// through `loadConfig` (a test, a future entry point, a caller that constructs Config itself)
	// and would otherwise hand a live credential to a model.
	//
	// NOTHING ELSE IS SCRUBBED, deliberately. Hermes needs the operator's own environment — their
	// provider configuration lives there and in ~/.hermes — and a bridge that sanitised it would
	// break the profiles it exists to run.
	cmd.Env = environWithout(os.Environ(), credentialEnvVars...)

	var stdout, stderr boundedBuffer
	stdout.limit = maxStageOutputBytes
	stderr.limit = 16 << 10
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// No stdin. A stage that blocks waiting for a human is a stage that hangs, and there is no human
	// here — which is also why an approval prompt in this path DENIES rather than being bypassed.
	cmd.Stdin = nil

	if err := cmd.Run(); err != nil {
		if stageCtx.Err() == context.DeadlineExceeded {
			return "", errf("stage %s exceeded its %ds wall-clock budget", spec.Profile,
				cfg.StageBudgetSeconds)
		}
		// stderr is redacted before it is quoted: it is the most likely place for a path or a
		// provider message to appear.
		return "", errf("stage %s failed: %v — %s", spec.Profile, err,
			truncateForError(stderr.String()))
	}
	return stdout.String(), nil
}

// buildHermesArgs constructs the argv. EVERY ELEMENT IS A CONSTANT OR A LOCALLY-DERIVED PATH. No
// element is, or is derived from, a value the hosted side supplied — the hosted question travels in
// the file at `queryPath` and nowhere else.
func buildHermesArgs(spec stageSpec, workdir, queryPath string, cfg Config) []string {
	turns := spec.MaxTurns
	if cfg.MaxTurns > 0 && cfg.MaxTurns < turns {
		turns = cfg.MaxTurns
	}
	return []string{
		"chat",
		"--query-file", queryPath,
		"--oneshot",
		"--quiet",
		"--in", workdir,
		"--max-turns", itoa(turns),
		"--run-budget", itoa(cfg.StageBudgetSeconds),
		"-t", spec.Toolsets,
		// Marks these sessions as tool-driven so they do not clutter the operator's own session
		// list. Cosmetic, and a courtesy to the machine's owner.
		"--source", "tool",
	}
}

func containsForbiddenFlag(args []string) string {
	for _, a := range args {
		for _, bad := range forbiddenFlags {
			if a == bad {
				return bad
			}
		}
	}
	return ""
}

// resolveProfileBinary maps a profile to its wrapper executable.
//
// THE WRAPPER NAME IS THE PROFILE NAME. `hermes profile alias --help` states it: `--name NAME` is a
// "Custom alias name (default: profile name)". So `hermes profile alias stock-scout` produces a
// wrapper called `stock-scout`, NOT `hermes-stock-scout`.
//
// An earlier version of this function looked only for `hermes-<profile>`, which meant every default
// installation failed to resolve — an operator who followed the documented `hermes profile alias`
// command would have been told, at `-check` time, that a wrapper they had just created was missing.
//
// The lookup order is:
//
//  1. `ATTESTEL_HERMES_BIN_<PROFILE>` — an explicit local override, for a wrapper kept outside PATH;
//  2. `<profile>` — what `hermes profile alias <profile>` actually creates;
//  3. `hermes-<profile>` — for an operator who chose that with `--name`, and for the convention this
//     bridge previously documented.
//
// All three are LOCAL configuration. No job can influence any of them, and a profile name outside
// the four in the chain is never passed here because nothing calls this with anything else.
func resolveProfileBinary(profile string) (string, error) {
	envKey := "ATTESTEL_HERMES_BIN_" + strings.ToUpper(strings.ReplaceAll(profile, "-", "_"))
	if override := strings.TrimSpace(os.Getenv(envKey)); override != "" {
		info, err := os.Stat(override)
		if err != nil {
			return "", errf("%s points at a file this bridge cannot read", envKey)
		}
		// LookPath checks the executable bit for a PATH lookup; an explicit override skips that, so
		// it is checked here rather than surfacing later as a confusing exec failure.
		if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return "", errf("%s points at something that is not an executable file", envKey)
		}
		return override, nil
	}
	for _, name := range profileWrapperNames(profile) {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errf("no wrapper for profile %q is on PATH. `hermes profile alias %s` creates one "+
		"named %q (its default is the profile name); this bridge also accepts %q, or an explicit "+
		"path in %s", profile, profile, profile, "hermes-"+profile, envKey)
}

// profileWrapperNames are the executable names this bridge will accept for a profile, in order of
// preference. The first is what `hermes profile alias` creates by default.
func profileWrapperNames(profile string) []string {
	return []string{profile, "hermes-" + profile}
}

// assertCleanWorkdir refuses to start a stage in a directory that carries instructions.
//
// This is the check that makes skipping `--ignore-rules` safe (see the header). The scratch
// directory is created by this bridge and contains only the query file and the stage outputs; if
// anything instruction-shaped is in it, either something else is writing there or a previous stage
// created it, and neither is a state in which to run an agent.
func assertCleanWorkdir(workdir string) error {
	for _, name := range instructionFileNames {
		if _, err := os.Stat(filepath.Join(workdir, name)); err == nil {
			return errf("the scratch directory contains %s; refusing to run an agent in a "+
				"directory that carries instructions", name)
		}
	}
	return nil
}

// maxStageOutputBytes caps one stage's stdout. The stage schema is small; anything larger is a
// runaway, and reading it into memory to discover that is the failure this prevents.
const maxStageOutputBytes = 512 << 10

// boundedBuffer is an io.Writer that stops accepting past `limit`. It never errors — a stage that
// overruns should be judged on the output it did produce, and the schema decode will reject a
// truncated document anyway.
type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }

func truncateForError(s string) string {
	s = redact(strings.Join(strings.Fields(s), " "))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// credentialEnvVars are the variables that must never reach a Hermes child. Both forms of the
// worker credential: the token itself, and the path to the file holding it.
var credentialEnvVars = []string{
	"ATTESTEL_WORKER_TOKEN",
	"ATTESTEL_WORKER_TOKEN_FILE",
}

// environWithout returns `env` with every `NAME=...` entry for the named variables removed.
//
// Matching is on the name only, up to the first `=`, so a variable whose VALUE happens to contain
// one of these names is untouched — the filter removes credentials, not anything that mentions one.
func environWithout(env []string, names ...string) []string {
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, found := strings.Cut(entry, "=")
		if found && drop[name] {
			continue
		}
		out = append(out, entry)
	}
	return out
}
