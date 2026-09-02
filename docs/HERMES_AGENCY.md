# Hermes research agency — `company_research_v1`

An **owner-only** research lane. The hosted application queues a bounded research job; a bridge
running on the **owner's own computer** claims it over outbound HTTPS, runs four approved Hermes
profiles in sequence, validates what they produced, and uploads a cited research artifact that
Attestel displays.

Nothing about this lane makes the hosted deployment heavier. **No model is called on the server, no
port is opened toward the owner's machine, and no provider credential ever leaves that machine.**

> **This lane produces research, not signals.** The artifact has no field for a direction, a price
> target, an expected return, a probability or a position size — not as a policy, but as the shape
> of the struct. See [What it cannot do](#what-it-cannot-do).

---

## 1. How it fits together

```
  HOSTED (public)                                LOCAL (the owner's computer)
  ────────────────────────────                   ────────────────────────────────────────────
  browser ── POST /api/agency/runs ──► gateway
                                         │  (thin proxy, forwards the session cookie)
                                         ▼
                                      journal ──── durable, owner-scoped run record
                                         ▲
                                         │  outbound HTTPS, claimed by the worker
                                         │  X-Worker-Token: <WORKER_TOKEN>
                                         │
                              ┌──────────┴──────────────────────────────────┐
                              │  attestel-hermes-bridge                     │
                              │    1. claim one queued run                  │
                              │    2. stock-scout                           │
                              │    3. stock-fundamentals                    │
                              │    4. stock-risk                            │
                              │    5. stock-chair                           │
                              │    6. validate, then upload the artifact    │
                              └─────────────────────┬───────────────────────┘
                                                    ▼
                                        the owner's own model + Hermes config
                                        (never transmitted, never read by Attestel)
```

**The direction of every connection matters.** The bridge dials out; nothing dials in. The hosted
deployment has no address for the owner's machine and never learns one. Closing the laptop is a
supported state: the run's lease lapses and it returns to the queue.

### Where each piece lives

| Piece | Path | Why there |
|---|---|---|
| Durable run store, state machine, lease, artifact validation | `journal/agency*.go` | The journal already owns per-user durable records (theses, decisions, portfolios) and verifies the session cookie itself. No new hosted process. |
| The `NO_SIGNAL` block served on every run | `journal/actionability.go` | Consumed by the run view and the UI. The full action-derivation engine is deferred — see §7. |
| Browser routes | `gateway/agency.go` | Thin proxy. The gateway stays standard-library-only. |
| The local worker | `bridge/` | Its own zero-dependency Go module. Not in `docker-compose.yml` and not in the `Dockerfile`, so it adds nothing to the hosted image. |
| The stage prompts | `bridge/prompts/*.md` | Embedded in the bridge binary. The hosted side cannot supply, select or override one. |
| Minimal UI | `web/src/components/research/AgencySection.jsx` | Research → `#research/agency`. |

---

## 2. Setup

### 2.1 Hosted side

Two variables, both empty by default, both fail closed.

```bash
# Who may create a research run. EMPTY MEANS NOBODY.
AGENCY_OWNER_UIDS=<your-user-id>

# The worker credential. Generate a NEW one; do not reuse AUTH_SECRET.
AGENCY_WORKER_TOKEN=<WORKER_TOKEN>
```

Generate the token with `openssl rand -hex 32`.

**Why not `AUTH_SECRET`?** Every other internal seam in this repository authenticates with
`X-Internal-Secret: $AUTH_SECRET`, which is defensible between containers on one private network.
It is not defensible here. `AUTH_SECRET` signs session cookies, and minting a session for an
arbitrary user id from it is roughly twenty-five lines of standard library (`paper/auth.go`'s
`systemCookie` does exactly that). This credential lives on a laptop. If it were `AUTH_SECRET`, a
compromised laptop would be a compromised account for every user of the deployment. The agency
token grants exactly the five worker routes — `status`, `claim`, `heartbeat`, `complete` and `fail`
— and can mint nothing.

Leave either variable empty and the lane is off: creating a run is a `403` naming
`AGENCY_OWNER_UIDS`, and the worker API is a `403` naming `AGENCY_WORKER_TOKEN`.

**Only the `journal` service reads either variable** (`journal/config.go`). The gateway proxies and
the journal enforces, so nothing else needs them.

**Restarting after a change — the all-in-one image.** `deploy/supervisord.conf` pins only
`PORT`, `ANALYSIS_URL` and `GATEWAY_URL` per program; everything else is *inherited from the
container environment*, which supervisord captured when the container started. So
`supervisorctl restart journal` re-execs the program with **that same captured environment** and
will not see a new or changed `AGENCY_*` value. **The container must be restarted** — redeploy or
restart the app. On a per-service deployment, set both on `journal` and redeploy `journal` alone.

### 2.2 Local side

**Provision the four profiles first.** `stock-scout`, `stock-fundamentals`, `stock-risk` and
`stock-chair` must exist as Hermes profiles, and **each must have a working model configured**.
This bridge never passes `-m`/`--provider` and never reads your Hermes configuration — which model
answers is your decision, per profile, and it is deliberately not expressible over the wire. Create
or import them with `hermes profile create` / `hermes profile install`, and confirm with
`hermes profile list`. Aliasing a profile that does not exist will not give you a working workflow.

**Then create the four wrappers, once.** `hermes chat` has no `--profile` flag; the alternatives are
a sticky global default (mutable global state — wrong for a worker) and per-profile wrapper scripts.
The bridge uses the wrapper, so the profile is part of the *executable name* and a hosted job cannot
name one:

```bash
hermes profile alias stock-scout
hermes profile alias stock-fundamentals
hermes profile alias stock-risk
hermes profile alias stock-chair
```

**The wrapper is named after the profile.** `hermes profile alias --help` says `--name NAME` is a
"Custom alias name (default: profile name)", so those commands create `stock-scout`,
`stock-fundamentals`, `stock-risk` and `stock-chair` on your PATH — **not** `hermes-stock-scout`.
The bridge resolves `<profile>` first, then `hermes-<profile>` for anyone who chose that with
`--name`, then the per-profile `ATTESTEL_HERMES_BIN_*` override.

**Build the bridge and copy the prompts beside it:**

```bash
cd bridge && go build -o attestel-hermes-bridge .
```

(The prompts are embedded in the binary. `ATTESTEL_BRIDGE_PROMPT_DIR` overrides them locally if you
want to edit them.)

**Configure it.** Copy `bridge/attestel-hermes-bridge.env.example` **outside this repository**, fill
it in, and `chmod 600` it. Every value in the template is a placeholder.

```bash
cp bridge/attestel-hermes-bridge.env.example ~/.config/attestel-hermes-bridge/env
chmod 600 ~/.config/attestel-hermes-bridge/env
```

**Point it at the right URL.** This is the setting most likely to be wrong on a first run:

| Deployment | `ATTESTEL_URL` |
|---|---|
| Behind the shipped nginx | `https://<your-host>/svc/journal` |
| Journal running directly (development) | `http://localhost:8096` |

`deploy/nginx.conf.template` serves `/api/` from the gateway and the journal from **`/svc/journal/`**
— the journal has no public port of its own. The bridge appends `/_internal/agency/...` to
`ATTESTEL_URL`, so a bare `https://<your-host>` sends every claim to a path the gateway serves and
the journal never sees. `-check` catches this before a single job is claimed.

**Verify before spending a model call:**

```bash
set -a; . ~/.config/attestel-hermes-bridge/env; set +a
attestel-hermes-bridge -check
```

`-check` verifies both halves:

- **local** — the URL and its scheme, that a credential is present (without printing it), the four
  prompt templates, and the four profile wrappers. It reports whether each wrapper resolved, never
  *where*, because that is the owner's filesystem layout.
- **hosted** — it calls the read-only `GET /_internal/agency/status`, which proves the URL reaches
  the journal through whatever reverse-proxy prefix is in front of it, that the credential is
  accepted, and that both sides speak the same schema version. It reports how many runs are queued
  and **claims nothing**: a preflight that has to consume work in order to tell you it is configured
  is not a preflight.
- **lease compatibility** — if the server would clamp your configured lease down, `-check` **fails**.
  A clamped lease silently voids the stage-budget invariant: the bridge would believe it holds 900
  seconds while actually holding whatever the server allowed.

`Status` requires every field — `ok`, both schema versions, a non-empty workflow list, a lease
ceiling and a queue depth. A response that omits them is not this API, and a preflight that passes
on silence is just a way of finding out at claim time instead. An unreadable queue answers `503`
with `ok:false` rather than `queuedRuns: 0`, because "nothing is waiting" and "nobody can read the
queue" are opposite answers that would otherwise look identical.

`-check` exits non-zero if any of that fails.

**`-check` is a preflight, not a read-only command.** It claims no job, takes no lease and invokes
no Hermes profile — but reaching it means `loadConfig` has run, which **creates the bridge state
directory (0700) and writes an opaque `worker-id` into it on first use**. Both are local and
gitignored.

It also **does** read your credential: from `ATTESTEL_WORKER_TOKEN`, or by opening the file named by
`ATTESTEL_WORKER_TOKEN_FILE` after checking it is not group- or world-readable. That read is how the
token reaches memory at all, and `-check` then **sends** it to the deployment to prove it is
accepted. What it never does is print it, log it, echo the file's path, or write it anywhere — the
report shows a character count and "value withheld".

A resolvable wrapper also only tells you the alias exists; whether the profile behind it has a
working model configured is yours to verify.

**Then prove the whole pipe with no model at all:**

```bash
ATTESTEL_BRIDGE_DRY_RUN=1 attestel-hermes-bridge
```

This runs the complete workflow with a deterministic local stub in place of Hermes: real claim, real
lease, real heartbeats, real schema validation, real upload, real UI. Debugging authentication and
debugging a research prompt at the same time is how an evening disappears. Every dry-run artifact is
labelled `degraded`, so it can never be mistaken for real research.

**Run it for real:**

```bash
attestel-hermes-bridge          # claim one job, work it, exit
attestel-hermes-bridge -drain   # keep claiming until the queue is empty (max 10), then exit
```

### 2.3 Exit status

Something will be scheduling this, so the exit code is how a failure gets noticed:

| Code | Meaning |
|---|---|
| `0` | the queue was empty, or every claimed job reached a reported conclusion — including a lease lost to a takeover, which is the protocol working |
| `1` | anything else: a bad configuration, an unreachable deployment, a rejected credential, a failed claim, a Hermes stage that would not run, an artifact that failed validation, or a failure that could not be reported back |

Wrap it in `launchd`/`cron` and let the non-zero exit surface.

### 2.4 Repetition is yours, not the software's

There is **no timer, no sleep loop and no scheduler** in the bridge, and there is none on the hosted
side either. `-drain` stops the moment the queue is empty. If you want it to run periodically, that
is your `launchd` job or your `cron` entry, outside this codebase — the same rule
`services/llm/app/enrich_worker.py` and `services/events/app/automation.py` state for their own
one-shot entrypoints, and for the same reason.

---

## 3. Using it

Research → **Research agency** (`#research/agency`). Type a question about the selected ticker and
press **Queue research run**. The API answers `202` immediately with a run id; the UI polls a
read-only status route.

Progress is the stage the **worker reported**, out of four. There is no animated percentage and no
elapsed-time estimate: a spinner that implies motion while a job sits in a queue is a lie about
where the work is. While nothing has claimed the run, the UI says exactly that.

### Reading the artifact

Every finding carries a **provenance label**, and the UI never renders two of them alike:

| Label | Means | Requires |
|---|---|---|
| `sourced` | a cited source states it | at least one citation |
| `calculated` | arithmetic over sourced values | citations **and** a visible `basis` showing the work |
| `inferred` | the model's own reading — **not a fact** | nothing; marked distinctly in the UI |
| `unknown` | explicitly not established | nothing |

`unknown` is a first-class answer and costs nothing, deliberately. A schema that only accepted
confident answers would be a schema that manufactured them. The same applies to a citation's
`publishedAt`: an undated source records the literal string **`unknown`** rather than a guessed
date, so the UI shows `s1 · sec.gov · unknown`. That is the correct rendering, not a missing value —
and it is what a dry run produces, since the stub's single citation is deliberately undated.

The strongest verdict the whole workflow can express is **`researchPriority`**:
`investigate` · `watch` · `reject` · `unknown`. These are instructions about the **question**, not
about a position. `investigate` means "worth more research". It does not mean buy.

### Run states

`queued` → `claimed` → `running` → `completed` | `failed` | `cancelled` | `expired`

A lapsed lease returns the run to `queued` while attempts remain (an outage is not a verdict on the
job) and to `expired` after three. **Cancel** is terminal and takes effect immediately: the lease is
dropped, so a worker that was mid-chain cannot land its result afterwards.

---

## 4. Offline and failure behaviour

| Situation | What happens |
|---|---|
| The bridge is not running | Runs sit `queued`. The UI says so. Nothing is lost. |
| No run is queued | `attestel-hermes-bridge` logs "no research runs are queued" and exits 0. |
| The laptop sleeps mid-run | The lease lapses; the run returns to `queued` and another attempt picks it up. The sleeping worker cannot overwrite the newer result when it wakes — `complete` requires the *current* lease token. |
| The hosted deployment is unreachable | The bridge exits non-zero and starts **no** Hermes invocation. Nothing is spent on work that cannot be delivered. |
| A stage exceeds its budget | The stage is killed at the local deadline and the run fails with a stated reason. Retryable. |
| A stage returns malformed output | The run **fails** with a reason. It is not repaired, not partially accepted, and not retried — the same input produces the same refusal. |
| A stage produces prescriptive language, a leaked path, or a credential-shaped string | Refused **locally, before upload**, and refused again by the server if it somehow arrives. |
| Two bridges run at once | The lease serialises them. Exactly one claims any given run. |
| Cancelled while running | Every remaining worker call is a `409`. The bridge discards its result rather than retrying. |

---

## 5. Security boundaries

**What the hosted side can send.** A workflow *name*, a ticker, a question, a cutoff and a lease
token. That is the entire job envelope. There is no field for a prompt, a profile, a toolset, a
model, a provider, a filesystem path, a shell command or a system prompt, and the create route
decodes with `DisallowUnknownFields` — so a request that *tried* to add one is a `400`, not a
silently ignored key. **This is not a remote-command API and it cannot be turned into one by
sending a different payload.**

**How Hermes is invoked.**

```
<profile-wrapper> chat --query-file <file> --oneshot --quiet \
    --in <bridge-owned scratch dir> --max-turns N --run-budget S -t <toolsets> --source tool
```

- **`--query-file`, never `-q`.** Hermes' own documentation for it: *"Safe for arbitrary text:
  nothing is shell-interpreted, so quotes, `$(...)`, and backticks are preserved verbatim."* The
  question is the one hosted-controlled string that reaches an agent, and this is the input path
  where it cannot become an argument. The argv is an explicit slice; there is no shell in this path.
- **Never `--yolo`.** Asserted against the module's source text, not just its behaviour.
- **Never `hermes -z/--oneshot` (the top-level flag).** Its own help says *"approvals are
  auto-bypassed"*, which makes it equivalent to `--yolo` for approval purposes on a headless run.
  `hermes chat --oneshot` is the subcommand form and carries no such clause.
- **Never `--accept-hooks`**, which auto-approves unseen shell hooks without a prompt.
- **Bounded three ways**: `--max-turns` caps tool iterations, `--run-budget` gives the agent a
  wall-clock budget, and a local context deadline kills the process if it ignores the budget — a
  bound the child honours voluntarily is not a bound.
- **Restricted toolsets** are passed explicitly on every invocation.

**Why not `--ignore-rules`?** It skips injection of `AGENTS.md`, `SOUL.md`, memory and preloaded
skills — and a Hermes **profile is defined by exactly those files**. Passing it would silently turn
`stock-scout` into a generic agent while appearing to run it. The risk it addresses (an `AGENTS.md`
in the working directory injecting instructions) is handled properly instead: `--in` points at a
bridge-created scratch directory containing one file, and the bridge **refuses to start a stage** if
anything instruction-shaped has appeared there.

**Prompt injection.** Any page an agent fetches may contain text addressed to it, and the honest
assumption is that one eventually will. The prompts say so, but prompts are not the defence. The
defences are structural:

1. the artifact schema has **no field for a signal**, so the best outcome of a successful injection
   is a wrong *narrative*;
2. the chain, the toolsets, the working directory and the prompts are fixed before the first token
   is generated — there is no branch a fetched page can take;
3. every stage's stdout is decoded into a **closed struct** with unknown fields refused;
4. each stage sees the *decoded, re-serialised* output of the earlier stages, never their raw
   stdout, so an injected instruction is presented as quoted evidence rather than as instruction;
5. the server validates everything again on receipt;
6. citations are rendered in the UI as **text, not links** — a URL an agent found is not one click
   from the owner's browser.

**Quoted subject matter vs agent-authored output.** The prescriptive-language scan runs only over
what the *agents composed* — findings and their bases, notes, contradictions, the thesis and
anti-thesis, risk findings, the chair's conclusion, the veto's reasons — at every provenance
including `inferred`. The owner's *question* and a source's *title*, *publisher* and *URL* are
exempt: "Is the sell-side price target justified?" is a legitimate question and "Analyst raises price
target on NVDA" is a real headline. An agent may **report** that a third party issued a rating; it
may not **issue** one. The leak scan has no such exemption — a credential or a home path is a
disclosure wherever it appears, including in a citation title.

**The worker credential never reaches a Hermes child, in either form.** `ATTESTEL_WORKER_TOKEN` and
`ATTESTEL_WORKER_TOKEN_FILE` are both removed from the bridge's own environment as soon as the
credential is read — on every path, including a failed read — and both are filtered again at the
subprocess boundary as defence in depth. The file form matters as much as the token form: the
variable holds a path to a 0600 file that the child, running as the same user, could simply open.
Nothing else is filtered, because Hermes needs the operator's own environment to run the profiles.

**What never leaves the machine.** `~/.hermes` in its entirety, `auth.json`, `.env` files, provider
and OAuth credentials, ChatGPT/Claude authentication, model and provider names, quantization,
temperature, token counts, cost, usage reports, session histories, memories, logs, caches, state
databases, machine identifiers, and absolute personal filesystem paths. Two mechanisms enforce it:
the artifact struct has nowhere to put any of them, and every string it *does* carry is scanned for
their shapes before upload and again on receipt. Errors and logs go through a redactor at
construction, so no call site has to remember.

**If the worker is fully compromised**, it can write a wrong narrative into a research record. It
cannot write a trading signal (no field), forge a session (the token is not `AUTH_SECRET`), reach
another user's data (routes are owner-scoped), or cause a trade (no broker exists anywhere in this
application). Every artifact carries `identity.workflowVersion` and `identity.bridgeVersion`, so a
compromised range is identifiable and can be excluded later.

---

## 6. Limits

| Limit | Value | Where |
|---|---|---|
| Tickers per run | 1 | `agencyMaxTickersPerRun` |
| Question length | 500 characters | `agencyMaxQuestionLen` |
| Sources per artifact | 40 | `agencyMaxSources` |
| Findings per stage / support list | 40 | `agencyMaxFindingsPerBox` |
| Any single statement | 2 000 characters | `agencyMaxStatementLen` |
| Artifact size | 256 KiB | `agencyMaxArtifactBytes` |
| Attempts per run | 3 | `agencyMaxAttempts` |
| Lease requested by the bridge | 900 s (server ceiling 1800 s) | `defaultLeaseSeconds` |
| Lease safety margin over one stage | 120 s, enforced at startup | `leaseSafetyMarginSeconds` |
| Server-side lease default | 15 min (ceiling 30) | `agencyLeaseDuration` |
| Unclaimed run expiry | 6 h | `agencyMaxRunAge` |
| Turns per stage | 24 (chair 12) | `bridge/hermes.go` |
| Wall clock per stage | 600 s | `ATTESTEL_STAGE_BUDGET_SECONDS` |
| Wall clock per run | 2 700 s | `ATTESTEL_RUN_BUDGET_SECONDS` |
| Run-budget margin over four stages | 300 s, enforced at startup | `runBudgetMarginSeconds` |
| Grounded **distinct** claims for `investigate`/`watch` | ≥ 2, and ≥ 25% of distinct claims | `agencyMinGroundedFindings` |
| Jobs per `-drain` | 10 | `maxDrainJobs` |

---

## 7. What it cannot do

**Hermes cannot generate an actionable signal, and this is enforced in four independent places —
the first of which is structural and is the authoritative one.**

0. **The lane cannot reach the prediction service at all.** The journal's agency code makes *zero*
   outbound HTTP calls; the gateway proxy resolves to exactly three `/agency/...` paths through
   `proxyJournal`; the bridge can construct exactly five `/_internal/agency/*` paths. There is no
   client, no URL and no configuration by which a research run could read a model, a backtest or a
   verdict — so it cannot consume a signal, and it cannot perturb one. This is checkable without
   running anything:

   ```bash
   grep -rn "PredictionURL\|:8003\|/predict\|/backtest\|/models/" \
     journal/agency*.go journal/actionability.go gateway/agency.go bridge/*.go --include='*.go' \
     | grep -v '_test.go'
   ```

   Expect only the four **comment** lines in `journal/actionability.go` citing `services/prediction`
   as documentation. Any call site is a defect. Prefer this over comparing `/api/predict` responses
   before and after a run: those carry `asOf` and cache metadata that move on their own, so a diff
   there proves nothing either way.

1. **The artifact schema has no field for one.** No `direction`, `signal`, `target`, `priceTarget`,
   `expectedReturn`, `probability`, `confidence`, `positionSize`, `weight`, `entry`, `stop` or
   `recommendation`. A stage that concluded "sell" has nowhere to write it, and the server's strict
   decoder refuses any field it does not declare.
2. **A banned-language scan** runs over every string, on both sides of the wire.
3. **The actionability block is a constant.** Every run, in every state including a completed one,
   reports `evidenceState: NO_SIGNAL`, no target, and `action: NO_ACTION`, with the six required
   quantitative gates listed as **not evaluated**.

### The vocabulary this introduces

`journal/actionability.go` names the state that the codebase currently has no word for:

| Axis | Values | Status in this patch |
|---|---|---|
| Evidence state | `VALIDATED` · `NO_SIGNAL` | **shipped** — served on every run |
| Action | `NO_ACTION` | **shipped** — the only outcome this lane can produce |
| Model target | `LONG` · `FLAT` · `SHORT` | **deferred** |
| Position-relative action | `OPEN_LONG` · `OPEN_SHORT` · `EXIT` · `NO_CHANGE` | **deferred** |

`NO_SIGNAL` is any missing or failed validation — no model, a failed backtest, a stale data policy,
no current pooled `EDGE` verdict, a strategy-version mismatch, an unreachable upstream, or a failed
research run. **It is never `HOLD`**, and there is no code path here that can make it one.

The defect it is aimed at is real and still present elsewhere in the tree:
`services/prediction/app/model.py::derive_direction` collapses "no directional view" and "bearish
but shorting was never backtested" into one word, `Hold`; and `Hold` means target *flat*, which
against an open long is an **exit**, while the English word means *keep*.

**Why the rest is deferred.** An earlier draft of this patch shipped a full `deriveAgencyAction`
mapping a target and the user's current position onto an action, tested by enumeration. Nothing
consumed it — the research lane cannot produce a target, so the function had no caller and no route.
It has been removed rather than carried as unwired code, and belongs with the change that actually
reads the quantitative gates. The rules it must satisfy are recorded in `actionability.go`'s comments
so the follow-up starts from the same semantics:

1. without `VALIDATED` evidence, no input produces a position change;
2. a veto may only ever **remove** an exposure-increasing action, never create, flip or strengthen
   one;
3. a veto may **never** block an exit — the same asymmetry `paper/gates.go` states for the
   quantitative gates;
4. holding the target you already hold is `NO_CHANGE`, and it stays distinguishable from
   `NO_SIGNAL`.

**Nothing is wired to the prediction service yet, deliberately.** The research lane does not read
quantitative evidence and cannot set a target. Before any actionable result is served, it must pass
`real-data`, `freshness`, `backtest-passed`, `pooled-edge`, `strategy-version` and
`portfolio-policy` — the gates that already exist in `paper/gates.go`,
`services/prediction/app/verdicts.py` and `journal/portfolio_intelligence.go`.

**There is no broker integration and no real-money execution anywhere in this application**, and
this lane adds none.

---

## 8. Troubleshooting

**`403` with `missingConfiguration: AGENCY_OWNER_UIDS`** — your user id is not on the allowlist, or
the allowlist is empty. Empty means nobody, on purpose.

**`403` with `missingConfiguration: AGENCY_WORKER_TOKEN`** — the hosted side has no worker
credential configured, so the worker API is off.

**`401 worker authentication required`** — the bridge's `ATTESTEL_WORKER_TOKEN` does not match the
server's `AGENCY_WORKER_TOKEN`.

**`404` on a worker route** — the request carried a session cookie. Internal routes refuse any
request from a browser, whatever its session says, and `404` keeps their existence undisclosed. If
you are testing with `curl`, drop the cookie jar.

**`ATTESTEL_URL must use https`** — expected. Plain HTTP is permitted only to a loopback host, and
only with `ATTESTEL_ALLOW_INSECURE_URL=1`.

**`no wrapper for profile "stock-scout" is on PATH`** — run `hermes profile alias stock-scout` (and
the other three), which creates a wrapper named after the profile. If you used `--name`, the bridge
also accepts `hermes-<profile>`; otherwise set the matching `ATTESTEL_HERMES_BIN_*` override to an
executable file. `attestel-hermes-bridge -check` lists all four.

**Every claim returns `401` although the tokens look identical** — check for a trailing newline. The
journal trims its configured `AGENCY_WORKER_TOKEN` and the bridge trims its own copy, so this should
no longer bite; if it does, regenerate with `openssl rand -hex 32 | tr -d '\n'`.

**`the worker credential file is group- or world-readable`** — `chmod 600` it.

**`-check` fails with `lease compatibility`** — the server's lease ceiling is below your configured
`ATTESTEL_LEASE_SECONDS`. Lower `ATTESTEL_STAGE_BUDGET_SECONDS` so `stage + 120s` fits under the
server's cap, then lower the lease to match.

**`the research queue could not be read`** (`503`) — an owner's stored document is unreadable, or no
owner is configured. The lane is not usable until that is resolved; it deliberately does not report
an empty queue instead.

**`ATTESTEL_LEASE_SECONDS is Ns but one stage may take Ms`** — the bridge refuses to start with a
lease that cannot outlast a single stage. It heartbeats between stages, so a lease shorter than
`stage budget + 120s` expires exactly when a slow stage finishes and the run is lost to a takeover.
Raise the lease or lower the stage budget.

**`ATTESTEL_RUN_BUDGET_SECONDS is Ns but 4 stages of up to Ms each need …`** — the run budget must
exceed the four stage budgets it contains by 300 s, so there is bounded time for the heartbeats,
validation, assembly and upload between and after them.

**A run failed with `researchPriority "investigate" … with no sources at all`** — the chair reported
a research-positive outcome while citing nothing. `unknown` is the honest outcome for a run that
established nothing, and it is always accepted. Working as intended.

**A run failed with `… rests on 1 DISTINCT sourced or calculated finding(s)`** — coverage counts
*distinct claims*, not appearances. One sourced sentence repeated across four stages is one piece of
evidence, not four.

**A run failed with `source sN is dated … after this run's cutoff`** — a citation postdates the
point in time the research answers at, which means either a hallucinated date or research that
reached past its cutoff.

**A run failed with `the stage's output does not match its schema`** — a profile returned something
other than the JSON object its prompt asks for. Not retryable: the same prompt will produce the same
shape. Check the profile's own instructions, or run one stage by hand to see what it returns.

**A run failed with `the agents produced prescriptive language`** — a stage wrote a recommendation, a
target or a rating. Working as intended. The offending fragment is quoted in the reason.

**A run failed with `the artifact contains …`** — a stage put an absolute path, a credential-shaped
string or a model/provider name into free text. Refused before upload.

**Runs sit `queued` forever** — nothing is claiming them. Run `attestel-hermes-bridge -check`, then
`attestel-hermes-bridge` and read the log line it prints.

**A run went back to `queued` on its own** — a lease lapsed. Expected after a sleep, a crash or a
kill. It expires terminally after three attempts.

---

## 9. Tests

Running these **writes to your machine**: the Go build and test cache (`go env GOCACHE`), the
compiled worker binary if you build one, `web/dist/` and `web/node_modules/.vite` from the bundle,
and per-test temp directories that `t.TempDir()` removes. All of it is local and gitignored. What
they do *not* touch: any tracked file, `ops/`, your Hermes installation, or any model — the
`bridge` suite stubs every Hermes invocation and briefly runs the real journal binary on a loopback
port against a temp data directory.

```bash
# Each in a subshell, so the block can be pasted as one: a bare `cd journal && …` would leave
# every later command running from the wrong directory.
(cd journal && go test ./...)   # state machine, leases, auth, idempotency, user isolation,
                                # redaction, artifact validation, research-quality floors,
                                # point-in-time citation checks, cancellation, the worker preflight
(cd gateway && go test ./...)   # proxy behaviour; the worker API is not reachable from a browser
(cd bridge  && go test ./...)   # exit-status contract, invocation rules (source-level and
                                # behavioural), stage schemas, credential handling, the privacy
                                # accept/reject table, the lease invariant, and the END-TO-END runs
(cd web     && npm run build && node --test tests/)   # the whole tests/ directory
```

`bridge/integration_test.go` compiles and starts the **real journal binary**, creates a run through
the **real owner route** with a **real session cookie**, has the **real bridge** claim, heartbeat,
assemble, validate and upload, then reads the artifact back. Only the four Hermes invocations are
stubbed. It needs no PostgreSQL, no Docker, no network, no provider credential and no Hermes
installation — the journal runs on its documented file backend.

Three of its cases are worth knowing about:

- **`TestACompleteRunThroughTheProductionProxyPath`** runs the whole thing through a reverse proxy
  that strips `/svc/journal/` exactly as `deploy/nginx.conf.template` does, so the URL shape this
  document tells operators to configure is the one that is actually tested.
- **`TestAStageThatOutlastsItsLeaseLosesTheRun`** reproduces the lease-versus-stage-budget bug
  directly, and `TestTheBudgetInvariantRefusesTheConfigurationThatCausesThat` proves every
  configuration that could cause it is refused at startup.
- **The privacy table** in `bridge/redact_test.go` and `journal/agency_test.go` is the *same* list
  of accept and reject cases, run against each module's own copy of the pattern set — so a change to
  one side that is not mirrored fails on the side that was not updated.
