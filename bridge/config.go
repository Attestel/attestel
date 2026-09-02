package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// config.go — everything this bridge is allowed to know, and where it is allowed to learn it from.
//
// CREDENTIALS COME FROM THE ENVIRONMENT OR FROM A FILE THE OPERATOR OWNS. NEVER FROM THIS
// REPOSITORY. `ATTESTEL_WORKER_TOKEN` is read from the process environment; if it is absent, a
// path in `ATTESTEL_WORKER_TOKEN_FILE` is read instead, and that file must not be
// group/world-readable. There is no default, no committed value and no fallback to any other
// secret. The shipped `attestel-hermes-bridge.env.example` is a template of placeholders and is the
// only configuration artefact in the repository.
//
// THE TOKEN IS REMOVED FROM THE ENVIRONMENT ONCE IT IS READ. `loadConfig` unsets it, so the Hermes
// child processes this bridge spawns do not inherit it. A prompt injection that persuades an agent
// to print its own environment must not be able to print the credential that lets it talk to the
// hosted deployment.

const (
	maxQuestionLen = 500

	// defaultLease is what the worker asks for; the server clamps it to its own ceiling. It is
	// shorter than the total run budget on purpose: the heartbeat between stages is what keeps it
	// alive, so a bridge that dies mid-stage releases the run within one lease rather than at the
	// end of the whole workflow.
	//
	// IT MUST OUTLAST ONE STAGE, WITH ROOM TO SPARE. The bridge heartbeats BETWEEN stages, not
	// during one, so the lease has to cover the longest a single stage can take plus the time to
	// get the next heartbeat out. A lease equal to the stage budget — which is what this was, 600
	// against 600 — expires at the exact moment the stage it is covering ends: a stage that runs to
	// its budget loses the run to a takeover, the completion is refused with a 409, and the work is
	// discarded. `validateBudgets` refuses that configuration rather than letting it be discovered
	// on the one run that happens to be slow.
	defaultLeaseSeconds = 900

	// leaseSafetyMarginSeconds is the room the lease must have beyond one stage budget: enough for
	// the stage's own teardown, the JSON decode, the schema validation and the next heartbeat's
	// round trip, on a machine that may be busy running a model.
	leaseSafetyMarginSeconds = 120

	// runBudgetMarginSeconds is the room the RUN budget must have beyond the four stage budgets it
	// has to contain.
	//
	// A run budget of exactly `4 × stage` — which is what 2400 against 600 was — leaves zero time
	// for everything between and after the stages: four heartbeat round trips, four JSON decodes,
	// four schema validations, the source-table merge, the artifact assembly, the final content
	// scans and the upload. All of that is bounded work, but it is not free, and a run whose four
	// stages each used their full budget would hit the deadline with a finished artifact in hand
	// and no time left to send it. The margin makes that arithmetic explicit rather than lucky.
	runBudgetMarginSeconds = 300

	// defaultStageBudget bounds ONE Hermes stage in wall-clock seconds, and defaultRunBudget bounds
	// the whole four-stage workflow. Both are hard: the stage budget is passed to Hermes as
	// `--run-budget` AND enforced locally with a context deadline, because a budget the child
	// honours voluntarily is not a bound.
	defaultStageBudgetSeconds = 600
	defaultRunBudgetSeconds   = 2700

	// defaultMaxTurns bounds tool-calling iterations inside one stage. Hermes' own default is 500,
	// which is an interactive default, not a headless one.
	defaultMaxTurns = 24

	// httpTimeout bounds one call to the hosted API. Short: these are small JSON round-trips.
	httpTimeout = 30 * time.Second
)

var tickerRE = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.\-]{0,15}$`)

// Config is the resolved runtime configuration. Note what is NOT here: no model, no provider, no
// API key, no prompt. Those live in the operator's own Hermes configuration, which this bridge
// never reads, never copies and never transmits.
type Config struct {
	// BaseURL is the hosted Attestel deployment. HTTPS is required unless AllowInsecureURL is
	// explicitly set, which exists for local development and for this repository's own tests.
	BaseURL          string
	AllowInsecureURL bool

	Token string

	// WorkerID is opaque and randomly generated on first run, then cached in StateDir. It is
	// deliberately NOT a hostname, a username or a MAC address: the hosted side needs to tell two
	// workers apart and needs to learn nothing else about either.
	WorkerID string

	// StateDir holds the worker id and the per-run scratch directories. It defaults under the
	// user's state directory and is created 0700. Nothing in it is ever uploaded.
	StateDir string

	// PromptDir holds the four stage prompt templates. Defaults to `prompts/` next to the
	// executable, falling back to the repository copy during development.
	PromptDir string

	LeaseSeconds       int
	StageBudgetSeconds int
	RunBudgetSeconds   int
	MaxTurns           int

	// DryRunHermes replaces the Hermes invocation with a deterministic local stub. It exists for
	// the integration test, it is OFF unless explicitly set, and it is reported in the artifact's
	// `degraded` list so a stubbed run can never be mistaken for a real one.
	DryRunHermes bool
}

func loadConfig() (Config, error) {
	cfg := Config{
		BaseURL:            strings.TrimRight(env("ATTESTEL_URL", ""), "/"),
		AllowInsecureURL:   envBool("ATTESTEL_ALLOW_INSECURE_URL"),
		LeaseSeconds:       envInt("ATTESTEL_LEASE_SECONDS", defaultLeaseSeconds),
		StageBudgetSeconds: envInt("ATTESTEL_STAGE_BUDGET_SECONDS", defaultStageBudgetSeconds),
		RunBudgetSeconds:   envInt("ATTESTEL_RUN_BUDGET_SECONDS", defaultRunBudgetSeconds),
		MaxTurns:           envInt("ATTESTEL_MAX_TURNS", defaultMaxTurns),
		DryRunHermes:       envBool("ATTESTEL_BRIDGE_DRY_RUN"),
	}
	if cfg.BaseURL == "" {
		return cfg, errf("ATTESTEL_URL is not set: this bridge does not guess where to connect")
	}
	if err := validateBaseURL(cfg.BaseURL, cfg.AllowInsecureURL); err != nil {
		return cfg, err
	}
	// Refused at startup, not discovered mid-run. See validateBudgets.
	if err := validateBudgets(cfg); err != nil {
		return cfg, err
	}

	token, err := readToken()
	if err != nil {
		return cfg, err
	}
	cfg.Token = token

	stateDir := env("ATTESTEL_BRIDGE_STATE_DIR", "")
	if stateDir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return cfg, errf("cannot resolve a state directory: %v", redact(err.Error()))
		}
		stateDir = filepath.Join(base, "attestel-hermes-bridge")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return cfg, errf("cannot create the bridge state directory: %v", redact(err.Error()))
	}
	cfg.StateDir = stateDir

	cfg.WorkerID, err = resolveWorkerID(stateDir)
	if err != nil {
		return cfg, err
	}

	cfg.PromptDir = env("ATTESTEL_BRIDGE_PROMPT_DIR", "")
	if cfg.PromptDir == "" {
		if exe, err := os.Executable(); err == nil {
			cfg.PromptDir = filepath.Join(filepath.Dir(exe), "prompts")
		}
	}
	return cfg, nil
}

// serverMaxLeaseSeconds mirrors journal/agency.go's `agencyMaxLeaseDuration`. The server clamps a
// longer request down to this, so a bridge configured past it would be running with a shorter lease
// than it believes — which is the same bug as an under-long lease, arriving quietly.
const serverMaxLeaseSeconds = 1800

// validateBudgets enforces the one invariant that ties the three time bounds together:
//
//	LeaseSeconds >= StageBudgetSeconds + leaseSafetyMarginSeconds
//
// The bridge heartbeats between stages, so the lease must cover a whole stage plus the work of
// getting the next heartbeat out. If it does not, a stage that runs close to its budget loses the
// run to a takeover at the exact moment it finishes, and everything it produced is discarded by a
// 409. Refusing the configuration at startup turns an intermittent, load-dependent data-loss bug
// into a message the operator reads once.
//
// The run budget is checked too: a run budget that cannot fit the four stages it is required to
// execute is a configuration that can only ever time out.
func validateBudgets(cfg Config) error {
	needed := cfg.StageBudgetSeconds + leaseSafetyMarginSeconds
	if cfg.LeaseSeconds < needed {
		return errf("ATTESTEL_LEASE_SECONDS is %ds but one stage may take %ds: the lease must be "+
			"at least %ds (stage budget + %ds margin), or a stage that runs to its budget loses "+
			"the run to a takeover the moment it finishes",
			cfg.LeaseSeconds, cfg.StageBudgetSeconds, needed, leaseSafetyMarginSeconds)
	}
	if cfg.LeaseSeconds > serverMaxLeaseSeconds {
		return errf("ATTESTEL_LEASE_SECONDS is %ds but the server caps a lease at %ds; lower the "+
			"stage budget so the lease fits under the cap",
			cfg.LeaseSeconds, serverMaxLeaseSeconds)
	}
	stages := cfg.StageBudgetSeconds * len(companyResearchChain)
	if minRun := stages + runBudgetMarginSeconds; cfg.RunBudgetSeconds < minRun {
		return errf("ATTESTEL_RUN_BUDGET_SECONDS is %ds but %d stages of up to %ds each need %ds, "+
			"plus %ds for the heartbeats, validation, assembly and upload between and after them: "+
			"at least %ds is required, or a run whose stages use their budgets would finish the "+
			"artifact with no time left to send it",
			cfg.RunBudgetSeconds, len(companyResearchChain), cfg.StageBudgetSeconds, stages,
			runBudgetMarginSeconds, minRun)
	}
	return nil
}

// validateBaseURL enforces the outbound transport rule.
//
// HTTPS IS REQUIRED. This connection carries a bearer credential; over plain HTTP the credential is
// readable by anything on the path, and the whole point of the pull model is that the laptop
// initiates a connection it can trust. The escape hatch is explicit, named, and refuses anything
// that is not a loopback host — so "allow insecure" can never quietly become "allow insecure to the
// internet".
func validateBaseURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errf("ATTESTEL_URL is not a valid URL")
	}
	if u.Host == "" {
		return errf("ATTESTEL_URL has no host")
	}
	if u.User != nil {
		return errf("ATTESTEL_URL must not embed credentials")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if !allowInsecure {
			return errf("ATTESTEL_URL must use https; set ATTESTEL_ALLOW_INSECURE_URL=1 only for " +
				"a loopback development server")
		}
		if host := u.Hostname(); host != "localhost" && host != "127.0.0.1" && host != "::1" {
			return errf("ATTESTEL_ALLOW_INSECURE_URL permits http only for localhost, not %q", host)
		}
		return nil
	default:
		return errf("ATTESTEL_URL scheme %q is not supported", u.Scheme)
	}
}

// readToken loads the worker credential and then removes it from this process's environment.
//
// The unset is the point. Every Hermes stage this bridge spawns inherits the environment; a
// credential left in it is one `env` tool call away from a model's context, and from there one
// prompt injection away from a transcript. It is held in memory instead, in exactly one struct.
func readToken() (string, error) {
	// BOTH variables are removed, on EVERY path, before this function can return.
	//
	// The earlier version unset only `ATTESTEL_WORKER_TOKEN`, which left the FILE form completely
	// exposed: `ATTESTEL_WORKER_TOKEN_FILE` stayed in the environment, every Hermes stage inherited
	// it, and an agent that read its own environment learned the exact path of a 0600 file holding
	// the credential — and could simply open it, since it runs as the same user. The path is not
	// the secret, but it is a map to the secret, and the file form was being recommended as the
	// SAFER of the two.
	//
	// Deferred rather than written at each return, so a future early return cannot skip it.
	defer func() {
		_ = os.Unsetenv("ATTESTEL_WORKER_TOKEN")
		_ = os.Unsetenv("ATTESTEL_WORKER_TOKEN_FILE")
	}()

	token := strings.TrimSpace(os.Getenv("ATTESTEL_WORKER_TOKEN"))
	if token != "" {
		return token, nil
	}
	path := strings.TrimSpace(os.Getenv("ATTESTEL_WORKER_TOKEN_FILE"))
	if path == "" {
		return "", errf("no worker credential: set ATTESTEL_WORKER_TOKEN or " +
			"ATTESTEL_WORKER_TOKEN_FILE. It is never read from the repository")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", errf("cannot read the worker credential file (path withheld)")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errf("the worker credential file is group- or world-readable; chmod 600 it")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", errf("cannot read the worker credential file (path withheld)")
	}
	token = strings.TrimSpace(string(raw))
	if token == "" {
		return "", errf("the worker credential file is empty")
	}
	return token, nil
}

// resolveWorkerID returns a stable, opaque id, generating one on first run.
func resolveWorkerID(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "worker-id")
	if raw, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id, nil
		}
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", errf("cannot generate a worker id")
	}
	id := "wkr_" + hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		// Not fatal: an ephemeral id still works, it just changes between runs.
		return id, nil
	}
	return id, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// errf builds an error whose text has already been through the redactor, so no call site can
// accidentally construct an error that carries a secret or an absolute path.
func errf(format string, args ...any) error {
	return fmt.Errorf("%s", redact(fmt.Sprintf(format, args...)))
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
