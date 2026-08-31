# Automation operations

How to enable, disable, pace and observe the production clock and the explicit operator lanes.

## The rule that shapes everything here

`CLAUDE.md` invariant #4 and `docs/research-os/EVENT_CONTRACTS.md` §9.22 forbid a model call caused
by a timer, interval, cron tick or scheduler tick **anywhere in the codebase**, without
qualification. Two of the eight lanes call the model.

The production image therefore ships one deliberately narrow clock: `app.production_scheduler`
can dispatch only the events-owned, model-free `ingest`, `reactions`, `scout-intake`, `scout`, and
`opportunity-radar` lanes. Its imports and tests exclude the worker-owned lanes.
`make automate-once` still evaluates all eight lanes once and exits,
but it is an **explicit operator command and must not be placed in cron**, because it includes the
two model lanes.

There is one pre-existing exception, and it is not new: `alerts` has an optional in-process timer
for the **deterministic, model-free** thesis sweep (`THESIS_MONITOR_ENABLED` +
`THESIS_MONITOR_INTERVAL`). It has never been able to reach a model, and it still cannot. If you
run the automation lane `thesis-monitor` instead, leave that timer off and let the dispatcher drive
the same `Tick` — you get a durable run record and a lane lease rather than only a log line.

## The eight lanes

| Lane | What one pass does | Model? | Production clock? | Default interval |
|---|---|---|---|---|
| `ingest` | One bounded provider ingestion pass | no | **yes** | 1 h |
| `reactions` | One bounded post-event reaction capture pass | no | **yes** | 6 h |
| `scout-intake` | Rotate five companies through keyless, budgeted discovery intake | no | **yes** | 4 h |
| `scout` | Materialize a deterministic company research queue from stored evidence | no | **yes** | 4 h |
| `opportunity-radar` | Scan completed daily bars for emerging, confirmed, extended, or invalidated research setups | no | **yes** | 4 h |
| `enrich` | One bounded enrichment pass over the raw-event queue | **yes**, under the lease | no, explicit only | 30 min |
| `resynth` | One bounded stale-thesis re-synthesis pass | **yes**, under the lease | no, explicit only | 1 h |
| `thesis-monitor` | One deterministic sweep against the stored bar tip | no | no; alerts owns its separate model-free timer | 15 min |

The dispatcher adds no job logic. Each lane remains a bounded function owned by its service;
`make automate-lane LANE=<name>` is the uniform operator command for forcing one pass through the
ordinary enable gates and lease.

## Enabling a lane

**Two flags must both be true.** Automation is not a second way to switch a job on.

```bash
# 1. the master door (default false)
AUTOMATION_ENABLED=true

# All-in-one production clock. This never broadens beyond the five model-free events lanes.
PRODUCTION_SCHEDULER_ENABLED=true
PRODUCTION_SCHEDULER_POLL_SECONDS=60

# 2. the lane's own long-standing flag (all default false)
INGEST_ENABLED=true              # ingest
EVENT_ENRICH_ENABLED=true        # enrich
THESIS_RESYNTH_ENABLED=true      # resynth
THESIS_MONITOR_ENABLED=true      # thesis-monitor
REACTION_CAPTURE_ENABLED=true    # reactions
SCOUT_INGEST_ENABLED=true        # scout-intake
SCOUT_ENABLED=true               # scout
OPPORTUNITY_RADAR_ENABLED=true   # opportunity-radar

# 3. optional: narrow the run to specific lanes (empty = every lane whose own flag is true)
AUTOMATION_LANES=ingest,reactions,scout-intake,scout,opportunity-radar
```

The recommended production allowlist is
`ingest,reactions,scout-intake,scout,opportunity-radar`. Even if it is
accidentally broader, the production clock filters to its closed events-owned tuple before
dispatch.

Scout intake defaults to `google-news-rss`, five companies every four hours. Six successful slots
therefore cover the default 30-company universe once per day. Google News RSS has no API key, but
it still requires a truthful contact identity in `EVENTS_CONTACT_UA`; without it the intake lane
reports `degraded` and Scout continues from evidence already stored. `SCOUT_INTAKE_PROVIDERS` is a
closed allowlist: it cannot spend the Marketaux or Alpha Vantage allocation. The `scout` lane is a
separate store-to-store materialization pass, so an intake outage never turns an Explore page load
into a live provider request. Explore withholds a snapshot older than
`SCOUT_MAX_RUN_AGE_SECONDS` (12 hours by default) instead of presenting stale leads as current.

The Opportunity Radar refreshes the analysis bar store through the analysis service, rejects
synthetic or stale histories, and persists only when its completed-bar/source/Scout-context
fingerprint changes. Its score is evidence ordering, not a probability; an extended move is
explicitly labelled no-chase. See `docs/EARLY_OPPORTUNITY_RADAR.md` for the frozen detector rules.

**Disabling** is the same in reverse, and either flag is sufficient: setting
`AUTOMATION_ENABLED=false` stops every lane while leaving `make enrich-once` and friends working
exactly as before.

With an empty `.env` — the shipped default — a dispatch pass runs nothing, leases nothing, and
opens no socket at all. That is asserted by
`services/events/tests/test_automation.py::test_dispatch_with_the_master_flag_off_does_nothing_and_opens_no_connection`,
which patches the database connector to raise.

## Pacing

```bash
AUTOMATION_INTERVAL_INGEST=3600
AUTOMATION_INTERVAL_ENRICH=1800
AUTOMATION_INTERVAL_RESYNTH=3600
AUTOMATION_INTERVAL_THESIS_MONITOR=900
AUTOMATION_INTERVAL_REACTIONS=21600
AUTOMATION_INTERVAL_SCOUT_INTAKE=14400
AUTOMATION_INTERVAL_SCOUT=14400
AUTOMATION_INTERVAL_OPPORTUNITY_RADAR=14400
AUTOMATION_LEASE_SECONDS=900      # how long before an abandoned lane may be re-taken (cap 6 h)
PRODUCTION_SCHEDULER_POLL_SECONDS=60  # due-state checks, not the execution cadence
```

An interval is a **minimum**, not a fixed wall-clock appointment. It is recorded as the lane's
`next_eligible_at` when a run completes. The production clock checks that record for the five
events-owned lanes; explicit operator passes check it for the other lanes.

## Running a lane by hand

```bash
make automate-once                     # every DUE lane, both halves, then exit
make automate-lane LANE=ingest         # one lane, now, ignoring its cadence
make automate-lane LANE=enrich
make production-scheduler              # long-running, model-free production clock
```

`--force` (which `automate-lane` passes) skips the cadence check only. It still honours both flags
and still takes the lane lease, so a forced run cannot stampede a lane another dispatcher is
already inside.

The two halves can also be run directly:

```bash
docker compose run --rm events python -m app.automation           # five model-free events lanes
docker compose run --rm llm    python -m app.automation           # enrich, resynth, thesis-monitor
docker compose run --rm llm    python -m app.automation --lane enrich --force
```

## Watching it

```bash
make automation-status                 # curl :8004/automation/status
```

or in the app: **Settings → Preferences → Automation health**. Both report, per lane: enabled
state, whether a run is in flight, last outcome, last *clean* success, last failure, consecutive
failures, configured cadence, next eligible time, and the recent run ledger with records
read/written/skipped and queue depth.

The same screen reports the daily provider budget: calls, limit, remaining capacity, warning,
exhausted and backoff-blocked states. Its scope is stated on the screen: this ledger counts calls
reserved by `services/events` only. Gateway and prediction provider calls are separate and are not
silently presented as covered by this total.

Each allocation can be changed without code using `EVENTS_BUDGET_<PROVIDER>` (uppercase, hyphens
replaced by underscores), for example `EVENTS_BUDGET_ALPHAVANTAGE=100`. Set this to the portion of
the plan assigned to events ingestion, not the provider account's total when other services share
the same key.

Three outcomes are kept distinct on purpose:

- **success** — the pass ran and everything it tried worked. Only this stamps `lastSuccessAt`.
- **degraded** — the pass ran, but something was skipped or unavailable (a missing provider key, a
  stopped batch). It stamps `lastFinishedAt` and the reason, and it is **not** a success: a lane
  where every provider skips for want of a key must not read as healthy.
- **failure** — the pass raised. Only this stamps `lastFailureAt` and increments the counter.

The status read carries flag **names** and booleans, never a flag value and never a secret.

## Prediction challenger controller

Prediction qualification has a separate supervised process because it owns a different PostgreSQL
schema and lifecycle. It is off by default. With `PREDICTION_AUTOMATION_ENABLED=true`, it waits for
the configured number of newer completed real bars, trains immutable challengers, starts the fixed
price evaluator, and records a paired challenger/champion shadow on identical future bars.

Use **Settings → Preferences → Challenger automation** or the authenticated
`GET /api/prediction-automation/status` route to inspect its lease, trial budget, evaluator result
and forward-shadow progress. These reads start nothing. The controller cannot run PEAD, tune a
threshold, call Qwen, promote a model, reset the official paper clock, or place an order. See
[`PREDICTION_AUTOMATION_CONTRACT.md`](PREDICTION_AUTOMATION_CONTRACT.md) for the fixed contract.

## Concurrency, restarts and idempotency

- **One lease per lane, in PostgreSQL.** `services/llm` and `services/events` are separate images,
  so a file lock cannot span them. Acquisition is a single conditional `UPDATE` that PostgreSQL
  serialises on the row: two dispatchers racing for one lane produce exactly one winner.
- **Completion requires the exact lease token.** A worker whose lease expired and was taken over
  cannot overwrite the newer run's result; its own result is discarded and logged.
- **An abandoned run is reconciled, not stranded.** A killed dispatcher's lease expires; the next
  acquisition closes its orphaned run as `failure` with `abandoned: lease expired without
  completion` — never as a success, and never left `running` forever.
- **Idempotency is the jobs', not the dispatcher's.** Documents dedupe on content hash, scheduled
  events upsert on their occurrence key with write-once `first_seen_at`, enrichment posts once per
  event, and the re-synthesis queue is lease-token bound. Running a lane twice cannot duplicate
  anything; that was already true before automation existed and automation does not weaken it.
- **Provider budgets and the model lease are untouched.** Every provider call still goes through
  `budget.reserve()`; every generation still holds the cross-process `fcntl.flock` that interactive
  requests always win.

## What automation deliberately cannot do

- It cannot be started from a page load, an API read, a ticker change or a frontend timer. The
  gateway proxies only `GET /api/automation/status`; the lease and complete routes are
  server-to-server, behind the shared internal secret, and are not proxied at all.
- It cannot put the model in a loop. Each model lane is one bounded, queue-draining pass that
  exits.
- The production clock cannot dispatch `enrich`, `resynth` or any future worker-owned lane. It
  passes the closed `EVENTS_OWNED` tuple to the events dispatcher on every check.
- It cannot enable a job on its own. The lane's own flag still governs.
