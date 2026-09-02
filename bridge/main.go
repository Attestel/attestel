// Command attestel-hermes-bridge claims ONE bounded research job from a hosted Attestel
// deployment, runs the approved Hermes profiles for it on this machine, validates what they
// produced, and uploads the artifact.
//
// It runs on the owner's own computer. It listens on nothing, exposes nothing, and every network
// connection it makes is outbound to the one hosted URL it was configured with. Provider
// credentials, model configuration and Hermes state stay here and are never transmitted.
//
//	attestel-hermes-bridge          # claim one job, work it, exit
//	attestel-hermes-bridge -drain   # keep claiming until the queue is empty, then exit
//	attestel-hermes-bridge -check   # verify configuration, wrappers and hosted connectivity;
//	                                # claims no job, but see the note on -check below
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// EXIT STATUS IS A CONTRACT, BECAUSE SOMETHING WILL BE SCHEDULING THIS.
//
// Repetition lives in the operator's own launchd/cron (there is no timer in this program), which
// means the exit code is how a failure gets noticed at all. So:
//
//	0  the queue was empty, or every job claimed was carried to a reported conclusion
//	1  anything else — a bad configuration, an unreachable deployment, a rejected credential, a
//	   failed claim, a Hermes stage that would not run, an artifact that failed validation, or a
//	   failure we could not even report back
//
// The one deliberate non-failure is a LOST LEASE. A 409 means another attempt owns the run and our
// result is correctly discarded; that is the protocol working, not this bridge failing, so it is
// logged and exits 0.
//
// An earlier version of this file logged every error and returned nil. A cron job wrapped around
// that would have reported success forever while nothing worked.
//
// ONE SHOT, THEN EXIT — no timer, no sleep loop, no scheduler. Even `-drain` stops the moment the
// queue is empty. That is the same rule services/llm/app/enrich_worker.py and
// services/events/app/automation.py state for their own one-shot entrypoints.
//
// Configuration is documented in docs/HERMES_AGENCY.md and templated in
// attestel-hermes-bridge.env.example. The worker credential comes from the environment or from a
// 0600 file; it is never read from this repository and never written to one.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

// maxDrainJobs bounds `-drain` so a runaway server cannot turn one invocation into an unbounded
// amount of local compute. It is a ceiling, not a target: the loop stops as soon as the queue is
// empty, which is the ordinary exit.
const maxDrainJobs = 10

// options are the parsed flags, in one struct so `run` is callable from a test.
type options struct {
	drain bool
	check bool
}

// deps are the three things `run` needs from the outside world. Production supplies the real ones;
// a test supplies fakes so the failure paths that must exit non-zero — a rejected credential, an
// unreachable host, a stage that will not run — are reachable without a server or a Hermes install.
type deps struct {
	loadConfig func() (Config, error)
	newClient  func(Config) hostedClient
	newRunner  func(Config) hermesRunner
}

func productionDeps() deps {
	return deps{
		loadConfig: loadConfig,
		newClient:  func(cfg Config) hostedClient { return newAPIClient(cfg) },
		newRunner: func(cfg Config) hermesRunner {
			if cfg.DryRunHermes {
				return stubRunner{}
			}
			return execRunner{}
		},
	}
}

func main() {
	drain := flag.Bool("drain", false, "keep claiming until the queue is empty (max 10 jobs), then exit")
	check := flag.Bool("check", false, "verify configuration, wrappers and hosted connectivity, then exit "+
		"without claiming a job (creates the bridge state directory and worker id on first use)")
	flag.Parse()

	// Logs go to stderr and every line has already been through the redactor, because every error
	// in this program is built by errf. Nothing here prints the token, a path under the home
	// directory, a model name or a provider.
	log.SetFlags(log.Ltime)
	log.SetPrefix("attestel-hermes-bridge: ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, options{drain: *drain, check: *check}, productionDeps()); err != nil {
		log.Printf("%v", err)
		os.Exit(1)
	}
}

// run is the whole program, minus flag parsing and the exit call. It returns an error for every
// condition that must exit non-zero; see the exit-status contract in the file header.
func run(ctx context.Context, opts options, d deps) error {
	cfg, err := d.loadConfig()
	if err != nil {
		return err
	}
	client := d.newClient(cfg)
	runner := d.newRunner(cfg)

	if cfg.DryRunHermes {
		log.Printf("DRY RUN: Hermes will not be invoked; stages are produced by a local stub and " +
			"every artifact is labelled degraded")
	}
	if opts.check {
		return runCheck(ctx, cfg, client, runner)
	}

	worked, err := drainQueue(ctx, cfg, client, runner, opts.drain)
	if err != nil {
		// Reported with the count so an operator can tell "nothing worked" from "two worked and the
		// third failed", which are different problems.
		if worked > 0 {
			return errf("worked %d run(s) before failing: %v", worked, err)
		}
		return err
	}
	if worked == 0 {
		// THE ONLY SUCCESSFUL ZERO. An empty queue is the ordinary answer, not a failure.
		log.Printf("no research runs are queued")
		return nil
	}
	log.Printf("worked %d run(s)", worked)
	return nil
}

// drainQueue claims and works jobs until the queue is empty, the drain cap is reached, or something
// fails. It returns how many jobs it carried to a conclusion and the first error that stopped it.
//
// A LOST LEASE IS NOT AN ERROR HERE. `runOnce` returns a stale-lease error when the run was taken
// over or cancelled while we held it; the correct response is to discard our result and carry on,
// so it is logged, counted as worked, and does not stop a drain.
func drainQueue(ctx context.Context, cfg Config, client hostedClient, runner hermesRunner, drain bool) (int, error) {
	worked := 0
	for {
		did, err := runOnce(ctx, cfg, client, runner)
		if did {
			worked++
		}
		switch {
		case err != nil && isStaleLease(err):
			log.Printf("a run's lease was taken over while we held it; discarding our result")
		case err != nil:
			return worked, err
		}
		if !did {
			return worked, nil
		}
		if !drain || worked >= maxDrainJobs {
			return worked, nil
		}
		// A cancelled context (Ctrl-C, SIGTERM) stops the drain between jobs rather than starting
		// another one we would only abandon.
		if err := ctx.Err(); err != nil {
			return worked, nil
		}
	}
}

// runCheck verifies everything an operator can get wrong, in the order they will hit it, WITHOUT
// claiming a job.
//
// IT IS NOT READ-ONLY, AND CALLING IT "read-only" WOULD BE WRONG. Reaching this function means
// `loadConfig` has already run, and that creates the bridge state directory (0700) and writes an
// opaque `worker-id` into it on first use. Both are local, both are gitignored, and neither leaves
// the machine — but they are writes, and an operator told "this changes nothing" would be misled.
//
// WHAT IT DOES WITH THE CREDENTIAL, PRECISELY. `loadConfig` has already READ it — from
// `ATTESTEL_WORKER_TOKEN`, or by opening and reading the file named by `ATTESTEL_WORKER_TOKEN_FILE`
// after checking that file is not group- or world-readable. That read is how the token gets into
// memory at all, and `-check` then SENDS it to the hosted deployment to prove it is accepted. What
// it never does is PRINT it, log it, echo the file's path, or write it anywhere: the report shows a
// character count and the words "value withheld", and nothing else.
//
// What it does NOT do: claim a job, take a lease, invoke a Hermes profile, or touch anything under
// the operator's Hermes installation.
//
// It checks the LOCAL half (URL shape, credential presence, prompts, profile wrappers) and then the
// HOSTED half (`GET /_internal/agency/status`): that the URL reaches the service through whatever
// reverse-proxy prefix is in front of it, that the credential is accepted, and that both sides
// speak the same schema version. Checking only the local half would let `-check` pass on a bridge
// that cannot talk to anything, which is the failure it most needs to catch.
//
// Its output goes to the operator's terminal and nowhere near the network. It never prints the
// token, and it never prints the PATH a wrapper resolved to — that is their filesystem layout.
func runCheck(ctx context.Context, cfg Config, client hostedClient, runner hermesRunner) error {
	fmt.Println("configuration")
	fmt.Printf("  state directory   : created/verified 0700 (worker id written on first use)\n")
	fmt.Printf("  hosted URL        : %s\n", cfg.BaseURL)
	fmt.Printf("  worker id         : %s\n", cfg.WorkerID)
	// The length only. Enough to catch an empty or truncated credential; never the value, and
	// never the path it came from.
	fmt.Printf("  worker credential : read, %d characters, value withheld\n", len(cfg.Token))
	fmt.Printf("  lease / stage / run budget : %ds / %ds / %ds\n",
		cfg.LeaseSeconds, cfg.StageBudgetSeconds, cfg.RunBudgetSeconds)
	fmt.Printf("  workflow          : %s\n", workflowCompanyResearch)

	var problems []string

	fmt.Println("prompt templates")
	for _, spec := range companyResearchChain {
		if _, err := loadPromptTemplate(cfg, spec.PromptFile); err != nil {
			fmt.Printf("  %-24s MISSING (%v)\n", spec.PromptFile, err)
			problems = append(problems, "prompt template "+spec.PromptFile)
		} else {
			fmt.Printf("  %-24s ok\n", spec.PromptFile)
		}
	}

	fmt.Println("Hermes profile wrappers")
	if _, isStub := runner.(stubRunner); isStub {
		fmt.Println("  (dry run: wrappers are not resolved)")
	} else {
		for _, spec := range companyResearchChain {
			if _, err := resolveProfileBinary(spec.Profile); err != nil {
				fmt.Printf("  %-24s NOT FOUND — %v\n", spec.Profile, err)
				problems = append(problems, "profile wrapper "+spec.Profile)
			} else {
				// The NAME that resolved, never the path — the path is the owner's filesystem
				// layout. A resolvable wrapper means the alias exists; whether the profile behind
				// it has a working model configured is the owner's to verify, and this bridge
				// deliberately never reads their Hermes configuration to find out.
				fmt.Printf("  %-24s ok (wrapper resolved)\n", spec.Profile)
			}
		}
	}

	fmt.Println("hosted deployment")
	statusCtx, cancel := withTimeout(ctx)
	defer cancel()
	status, err := client.Status(statusCtx)
	if err != nil {
		fmt.Printf("  reachable / authorised   FAILED — %v\n", err)
		fmt.Println("  hint: ATTESTEL_URL must include the reverse-proxy prefix the deployment")
		fmt.Println("        serves the journal under (for example https://<host>/svc/journal),")
		fmt.Println("        and ATTESTEL_WORKER_TOKEN must equal the server's AGENCY_WORKER_TOKEN.")
		problems = append(problems, "hosted connectivity")
	} else {
		fmt.Printf("  reachable / authorised   ok\n")
		fmt.Printf("  job / artifact schema    %s · %s\n",
			status.JobSchemaVersion, status.ArtifactSchemaVersion)
		fmt.Printf("  runs queued for me       %d\n", *status.QueuedRuns)
		// A LEASE THE SERVER WILL CLAMP IS A FAILURE, NOT A NOTE.
		//
		// `validateBudgets` refuses a lease shorter than one stage plus its margin, because a stage
		// that outlasts its lease loses the run to a takeover. If the server then silently clamps
		// the lease back DOWN, that guarantee is void — the bridge believes it holds a 900-second
		// lease and actually holds whatever the server allowed. Printing a note and finishing with
		// "all checks passed" told the operator their configuration was fine when it was not.
		if cfg.LeaseSeconds > *status.MaxLeaseSeconds {
			fmt.Printf("  lease compatibility     FAILED — this bridge asks for %ds and the server "+
				"caps a lease at %ds; the clamped lease would silently void the stage-budget "+
				"invariant\n", cfg.LeaseSeconds, *status.MaxLeaseSeconds)
			fmt.Printf("  fix: lower ATTESTEL_STAGE_BUDGET_SECONDS so that stage + %ds fits under "+
				"%ds, then lower ATTESTEL_LEASE_SECONDS to match\n",
				leaseSafetyMarginSeconds, *status.MaxLeaseSeconds)
			problems = append(problems, "lease compatibility")
		} else {
			fmt.Printf("  lease compatibility     ok (%ds requested, server cap %ds)\n",
				cfg.LeaseSeconds, *status.MaxLeaseSeconds)
		}
	}

	if len(problems) > 0 {
		return errf("-check found %d problem(s): %s", len(problems), joinComma(problems))
	}
	fmt.Println("\nall checks passed")
	return nil
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
