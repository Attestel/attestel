"""The prod smoke — Wave 5 Lane 5C.

    python -m app.smoke            # or: make smoke-model

ONE RUNNABLE CHECK, executed by whoever brings the prod box up, which produces the measurement
table this programme has been deferring since Wave 1. It is not a test: it needs a real model on a
real endpoint, it takes minutes, and it is never part of a suite.

WHY IT EXISTS
-------------
Every measurement in this programme — 402 passing tests included — came from an injected fake or
`stub:offline`. `stub:offline` has never disagreed with the schema, never emitted an unserved
number, never triggered an override branch and never taken a second to answer. It is a fixture. The
fields below are the ones only a real model can produce, and until this runs they are **unmeasured**
— not zero. An absent observation is not an observation of absence (§9.44, one layer up).

THE PROOF COMES FIRST, AND IT IS A GATE
---------------------------------------
Phase 0 proves a real model served this run before Phase 1 reports a single number:

  1. a runtime is CONFIGURED (`runtime.py`, no default) — otherwise there is nothing to measure;
  2. a live generation returns a `model_used` that is NOT `stub:*`, and that equals the configured
     model id — a server that answers with a different model is a different experiment;
  3. the identity stamp on a full analyst envelope names that same model, so what the pipeline
     PERSISTS agrees with what the transport saw;
  4. the cross-process lease (§9.21) was taken and RELEASED — observed at the marker file, not
     assumed.

Any of those failing exits non-zero with the reason and prints NO measurements. This programme has
already shipped a fixture that matched the contract's own example because nothing compared them, and
an AD-8 test that passed against an unfixed gateway because its fixtures never reached the branch
under test. "The output looks like a model wrote it" is not evidence.

WHAT IT REFUSES TO GUESS
------------------------
Resident memory and time-to-first-token are reported as `unmeasured` with the reason, on any
deployment that cannot expose them (an OpenAI-compatible server publishes neither, and this
transport does not stream). Writing `0` there would be the exact failure the clause above forbids.

CONTEXT PROVENANCE IS SEPARATE FROM MODEL PROVENANCE, and both are printed. A run whose CONTEXT came
from the built-in deterministic scenarios still had its GENERATIONS produced by a real model; the
report says which was which, so no reader has to infer it.
"""
from __future__ import annotations

import json
import os
import statistics
import sys
import time
from datetime import datetime, timezone

from . import analyst, lease, llm
from .runtime import STUB_RUNTIME, is_stub

# --- exit codes, mirroring app.ablation's convention so an operator reads one across all harnesses
EXIT_OK = 0
EXIT_NO_RUNTIME = 2        # refused: no runtime configured — nothing to measure
EXIT_STUB_SERVED = 3       # refused: the stub answered; every number would describe the fixture
EXIT_IDENTITY_MISMATCH = 4  # refused: the server served a model other than the configured one
EXIT_LEASE_UNPROVEN = 5    # refused: the lease was not observed taken and released

DEFAULT_RUNS = 5
BAR = "=" * 78

#: A short, deterministic prompt used ONLY by the phase-0 proof. It measures the transport, not the
#: product, and it is deliberately not the analyst prompt: the proof must be cheap enough that a
#: misconfigured endpoint costs one generation, not eight.
PROBE = [
    {"role": "system", "content": "Reply with JSON only."},
    {"role": "user", "content": 'Return exactly {"ok": true} and nothing else.'},
]

#: Built-in deterministic contexts. THEY ARE FIXTURES AND THE REPORT SAYS SO. Their job is to reach
#: different override branches (§9.56's `DIRECTION_BRANCHES`) — a full-context run, a run missing an
#: evidence layer, and a bare run — so a distribution across the five branches is observable at all.
#: A live deployment should prefer `SMOKE_CONTEXT_URL`; see `_contexts`.
FIXTURE_CONTEXTS: tuple[tuple[str, dict], ...] = (
    ("full", {
        "ticker": "NVDA",
        "tickerContext": {
            "technical": {"regime": "uptrend", "keyLevels": {"support": [181.0, 173.8],
                                                             "resistance": [188.4]}},
            "recentEvents": [{"eventId": "evt_0000000000000001", "title": "supply agreement"}],
            "earnings": {"lastSurprisePct": 4.2},
            "macro": {"surprise": "in line"},
        },
        "marketContext": {"benchmark": "SPY", "realizedVolatility": 0.21},
    }),
    ("missing-layer", {
        "ticker": "NVDA",
        "tickerContext": {
            "technical": {"regime": "range", "keyLevels": {"support": [173.8], "resistance": []}},
        },
    }),
    ("bare", {"ticker": "NVDA"}),
)


def _now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _env(key: str, default: str) -> str:
    value = os.getenv(key)
    return value if value is not None and value.strip() else default


# =================================================================================================
# Phase 0 — the proof. Nothing below this section may run until every check here passes.
# =================================================================================================


def prove_real_model(*, call_model=None) -> tuple[dict, int]:
    """`(proof, exit_code)`. `exit_code != EXIT_OK` ⇒ report nothing and stop.

    `call_model` is injected in tests. In production it is `llm.call_model_meta`, which is the same
    transport every product path uses — a proof that ran through a private client would prove
    something about the private client.
    """
    proof: dict = {"at": _now(), "checks": []}

    def record(name: str, ok: bool, detail: str) -> None:
        proof["checks"].append({"check": name, "ok": bool(ok), "detail": detail})

    runtime = llm.RUNTIME
    proof["runtime"] = llm.runtime_report(runtime, llm.RUNTIME_REFUSALS)
    if runtime is None:
        record("runtime_configured", False,
               " ".join(llm.RUNTIME_REFUSALS) or "no runtime configured")
        return proof, EXIT_NO_RUNTIME
    record("runtime_configured", True, f"{runtime.name} · {runtime.kind} · {runtime.model}")

    # (2) a live generation, through the product transport, timed. This doubles as the COLD-LOAD
    #     measurement: on a box that has not served this model yet, the weight load is inside it.
    call = call_model or (lambda messages: llm.call_model_meta(messages, json_mode=True))
    started = time.monotonic()
    with lease.acquire_interactive():
        raw, model_used, meta = call(PROBE)
    proof["probeSeconds"] = round(time.monotonic() - started, 3)
    proof["probeModelUsed"] = model_used
    proof["probeRawLength"] = len(str(raw or ""))
    proof["probeTransport"] = (meta or {}).get("transport")

    if is_stub(model_used):
        record("real_model_served", False,
               f"the transport returned {model_used!r} — the deterministic stub answered, so every "
               "number this run could print would describe the fixture, not the model")
        return proof, EXIT_STUB_SERVED
    record("real_model_served", True, f"transport returned model_used={model_used!r}")

    if str(model_used).strip() != runtime.model.strip():
        record("identity_matches_config", False,
               f"configured model is {runtime.model!r} but the server answered as {model_used!r}. "
               "A server serving a different model is a different experiment; refusing rather than "
               "attributing this run's numbers to the configured weights.")
        return proof, EXIT_IDENTITY_MISMATCH
    record("identity_matches_config", True, f"server and configuration agree on {model_used!r}")

    # (4) the lease, OBSERVED. `interactive_lease_is_held()` is False here because the `with` above
    #     exited; the marker file is the cross-process record that it was held and released.
    marker = lease.marker_path()
    held_now = lease.interactive_lease_is_held()
    marker_state = ""
    try:
        with open(marker, encoding="utf-8") as fh:
            marker_state = str(json.load(fh).get("state") or "")
    except (OSError, ValueError):
        marker_state = ""
    proof["leaseMarkerPath"] = marker
    proof["leaseMarkerState"] = marker_state or "absent"
    if held_now or marker_state != "released":
        record("lease_taken_and_released", False,
               f"lease marker is {marker_state or 'absent'!r} and in-process held={held_now}. "
               "§9.21's lease must be observed taken AND released; an unproven lease means "
               "concurrent generation was never actually excluded on this box.")
        return proof, EXIT_LEASE_UNPROVEN
    record("lease_taken_and_released", True, f"marker at {marker} reads 'released'")

    return proof, EXIT_OK


# =================================================================================================
# Phase 1 — the seven measures. Reached only after the proof passes.
# =================================================================================================

UNMEASURED = "unmeasured"


def _unmeasured(reason: str) -> dict:
    """The shape every unmeasurable field takes. NEVER `0`, never `None` on its own — a reader must
    be able to tell "we looked and it was zero" from "this deployment cannot report it"."""
    return {"value": UNMEASURED, "reason": reason}


def resident_memory(runtime, *, get=None) -> dict:
    """Measure 1 — resident memory while the model is loaded.

    Ollama publishes it at `/api/ps` (`size_vram`). An OpenAI-compatible server publishes nothing
    equivalent, and this process runs on a different machine from the weights, so there is nothing
    local to read either. That deployment reports `unmeasured` with the reason.
    """
    if runtime is None or runtime.kind != "ollama":
        return _unmeasured(
            "the OpenAI-compatible dialect exposes no memory endpoint, and the weights are not in "
            "this process. Read it on the model host (`nvidia-smi`, `ollama ps`, cgroup RSS) and "
            "record it in the deployment notes — it is a property of the deployment, not of the "
            "contract."
        )
    import requests  # local import: this path is optional and the module must import offline

    fetch = get or (lambda url: requests.get(url, timeout=15))
    try:
        resp = fetch(f"{runtime.base}/api/ps")
        models = (resp.json() or {}).get("models") or []
    except Exception as exc:  # noqa: BLE001 — an unreachable /api/ps is unmeasured, not a crash
        return _unmeasured(f"{runtime.base}/api/ps did not answer: {type(exc).__name__}: {exc}")
    for entry in models:
        if str(entry.get("name") or entry.get("model") or "").startswith(runtime.model):
            return {"value": entry.get("size_vram") or entry.get("size"), "unit": "bytes",
                    "source": f"{runtime.base}/api/ps"}
    return _unmeasured(f"{runtime.model!r} is not resident at {runtime.base}/api/ps")


def timings(probe_seconds: float, run_seconds: list[float]) -> dict:
    """Measure 2 — cold load, first token, full generation.

    `coldLoadPlusFirstGeneration` is the phase-0 probe: on a box that has not served this model, the
    weight load is inside that number and cannot be separated from it without a second, unloaded
    box. It is named for what it contains rather than reported as "cold load".

    TIME TO FIRST TOKEN IS `unmeasured` BY CONSTRUCTION: `_post_openai` and `_post_ollama` both send
    `stream: False`, so the first token and the last arrive together. Measuring it means adding a
    streaming transport, which is a change to the product, not to this file.
    """
    out: dict = {
        "coldLoadPlusFirstGenerationSeconds": round(probe_seconds, 3),
        "timeToFirstTokenSeconds": _unmeasured(
            "the transport sends stream:False, so the first and last tokens arrive together. "
            "Measuring TTFT requires a streaming transport."
        ),
    }
    if run_seconds:
        out["fullAnalystRunSeconds"] = {
            "runs": len(run_seconds),
            "min": round(min(run_seconds), 3),
            "median": round(statistics.median(run_seconds), 3),
            "max": round(max(run_seconds), 3),
            "mean": round(sum(run_seconds) / len(run_seconds), 3),
        }
        # THE NUMBER LANE 5A IS WAITING FOR. `ablation.py::plan_runs` sizes the ladder in
        # generations; multiply the two and the ladder's duration is arithmetic, not a redesign.
        out["secondsPerAnalystRun"] = round(sum(run_seconds) / len(run_seconds), 3)
    else:
        out["fullAnalystRunSeconds"] = _unmeasured("no analyst run completed")
    return out


def branch_distribution(envelopes: list[dict]) -> dict:
    """Measure 3 — the override-branch distribution across `DIRECTION_BRANCHES`' five values.

    THE FINDING TO WATCH FOR: if every real run lands on `model`, §9.56's whole override machinery
    has never fired outside a fixture, and every test that exercises `missing-layer` / `mixed` /
    `no-stance` / `out-of-enum` is describing code no model has reached. That is a real result and
    the report states it in words, not only as a table of zeros.
    """
    counts = {b: 0 for b in analyst.DIRECTION_BRANCHES}
    unknown: dict[str, int] = {}
    total = 0
    for env in envelopes:
        for thesis in (env.get("theses") or {}).values():
            branch = str((thesis or {}).get("directionBranch") or "")
            total += 1
            if branch in counts:
                counts[branch] += 1
            elif branch:
                unknown[branch] = unknown.get(branch, 0) + 1
    never = [b for b, n in counts.items() if n == 0]
    return {
        "theses": total,
        "counts": counts,
        "unexpectedBranches": unknown,
        "branchesNeverReached": never,
        "note": (
            "Every thesis landed on `model`: the override branches have not fired outside a "
            "fixture on this runtime, so no claim about them may cite this run."
            if total and counts["model"] == total else
            "At least one override branch fired on a real generation."
            if total else "No thesis was produced."
        ),
    }


def prose_suppressions(envelopes: list[dict]) -> dict:
    """Measure 4 — the `proseSuppressions` rate, ITEMISED.

    §9.58 was designed from a single observation (`173.80` happening to be a served level on one
    fixture and not on another). This is the measurement it was built for: how often does a real
    model put a number in prose that the server never served, which field, and on which branch.
    """
    items: list[dict] = []
    for env in envelopes:
        for row in env.get("proseSuppressions") or []:
            items.append(dict(row))
    theses = sum(len(env.get("theses") or {}) for env in envelopes)
    by_field: dict[str, int] = {}
    by_branch: dict[str, int] = {}
    literals: dict[str, int] = {}
    for row in items:
        by_field[str(row.get("field"))] = by_field.get(str(row.get("field")), 0) + 1
        by_branch[str(row.get("directionBranch"))] = by_branch.get(str(row.get("directionBranch")), 0) + 1
        for lit in row.get("literals") or []:
            literals[str(lit)] = literals.get(str(lit), 0) + 1
    return {
        "theses": theses,
        "suppressions": len(items),
        "ratePerThesis": (len(items) / theses) if theses else None,
        "byField": by_field,
        "byDirectionBranch": by_branch,
        "literals": literals,
    }


def schema_failures(envelopes: list[dict]) -> dict:
    """Measure 5 — the schema-failure rate, and §9.52's `failed` path.

    Two different questions, and the second has an answer that is NOT a number:

    * The rate is countable here — `stageOutcomes[role].retried` is a structural failure that the
      one firmer retry was given a chance to rescue, and `.stubbed` is one it did not.
    * §9.52's `failed` path WAS RESOLVED BY DELETION in contract v1.11 (§9.54 binding 1):
      `enrichment_state` has two values, `raw` and `enriched`. So "was the `failed` path reached"
      is answered "the path does not exist", and reporting a rate of 0 for it would describe a
      branch that is not in the code as one that is in the code and never fired.
    """
    stages = 0
    retried = 0
    stubbed = 0
    per_role: dict[str, dict] = {}
    for env in envelopes:
        for role, outcome in (env.get("stageOutcomes") or {}).items():
            stages += 1
            slot = per_role.setdefault(role, {"stages": 0, "retried": 0, "stubbed": 0})
            slot["stages"] += 1
            if outcome.get("retried"):
                retried += 1
                slot["retried"] += 1
            if outcome.get("stubbed"):
                stubbed += 1
                slot["stubbed"] += 1
    return {
        "stages": stages,
        "structuralFailures": retried,
        "structuralFailureRate": (retried / stages) if stages else None,
        "fellBackToStubAfterRetry": stubbed,
        "stubFallbackRate": (stubbed / stages) if stages else None,
        "perRole": per_role,
        "enrichmentFailedPath": {
            "reached": False,
            "reachable": False,
            "why": "§9.54 binding 1 REMOVED `failed` from `enrichment_state` in contract v1.11; "
                   "the enum has two values, `raw` and `enriched`. The path was resolved by "
                   "deletion, so this is not a rate of zero — there is no branch to reach.",
        },
    }


def abstention_reachable(envelopes: list[dict]) -> dict:
    """Measure 6 — are `neutral` and `unclear` reachable without the stub?

    Wave 3's exit criterion, still answered only by fixtures. A direction that only ever appears
    when the server overrode it is NOT the model reaching an abstention — so the branch is carried
    alongside every observation, and `fromModel` is the number that answers the question.
    """
    seen: dict[str, dict] = {"neutral": {"total": 0, "fromModel": 0},
                             "unclear": {"total": 0, "fromModel": 0}}
    directions: dict[str, int] = {}
    for env in envelopes:
        for thesis in (env.get("theses") or {}).values():
            direction = str((thesis or {}).get("direction") or "")
            directions[direction] = directions.get(direction, 0) + 1
            if direction in seen:
                seen[direction]["total"] += 1
                if str((thesis or {}).get("directionBranch")) == "model":
                    seen[direction]["fromModel"] += 1
    return {
        "directions": directions,
        "abstentions": seen,
        "modelReachedAbstention": any(v["fromModel"] > 0 for v in seen.values()),
        "note": "`fromModel` counts abstentions the MODEL chose. A `neutral` the server imposed "
                "(§9.56) is the override working, not the model abstaining, and the two must not "
                "be pooled.",
    }


def banned_hits(envelopes: list[dict]) -> dict:
    """Measure 7 — the `BANNED_ANALYST` hit rate, AND across how many runs.

    Zero across three runs is not "clean"; it is three runs. The denominator is reported beside the
    numerator for exactly that reason, and the verdict sentence refuses to say "clean" below a
    stated floor.
    """
    hits: list[str] = []
    for env in envelopes:
        for warning in env.get("warnings") or []:
            if "contains advice phrase" in str(warning):
                hits.append(str(warning))
    runs = len(envelopes)
    floor = 30
    return {
        "runs": runs,
        "hits": len(hits),
        "examples": hits[:10],
        "ratePerRun": (len(hits) / runs) if runs else None,
        "verdict": (
            f"{len(hits)} hit(s) across {runs} run(s)." if hits else
            f"0 hits across {runs} run(s) — that is a SAMPLE SIZE, not a clean bill of health. "
            f"Below {floor} runs this line asserts nothing about the model's propensity to emit "
            "advice phrasing."
        ),
    }


# =================================================================================================
# The run
# =================================================================================================


def _contexts(runs: int) -> tuple[list[tuple[str, dict]], str]:
    """`(contexts, provenance)`.

    `SMOKE_CONTEXT_URL` (e.g. `http://gateway:8080/api/context/NVDA`) is preferred and makes the run
    a genuine end-to-end measurement. Without it the built-in scenarios are used, and the report
    says so — a fixture CONTEXT with a real MODEL is a legitimate measurement of the model, and
    mislabelling it as live would be the fourth time this programme confused the two.
    """
    url = _env("SMOKE_CONTEXT_URL", "")
    if url:
        import requests

        try:
            payload = requests.get(url, timeout=60).json()
            return [(f"live-{i}", payload) for i in range(runs)], f"live: {url}"
        except Exception as exc:  # noqa: BLE001
            print(f"  SMOKE_CONTEXT_URL unreachable ({type(exc).__name__}: {exc}); "
                  "falling back to the built-in scenarios, and the report will say so.")
    picked = [FIXTURE_CONTEXTS[i % len(FIXTURE_CONTEXTS)] for i in range(runs)]
    return picked, "fixture: app.smoke.FIXTURE_CONTEXTS (deterministic; the MODEL is real, the CONTEXT is not)"


def measure(runs: int, *, run_analyst=None) -> dict:
    """Run the analyst `runs` times and assemble the seven measures."""
    execute = run_analyst or analyst.run_analyst
    contexts, provenance = _contexts(runs)
    envelopes: list[dict] = []
    seconds: list[float] = []
    for label, ctx in contexts:
        started = time.monotonic()
        env = execute("NVDA", dict(ctx))
        seconds.append(time.monotonic() - started)
        env["_smokeContextLabel"] = label
        envelopes.append(env)
    return {
        "runs": runs,
        "contextProvenance": provenance,
        "envelopes": envelopes,
        "seconds": seconds,
    }


def report(proof: dict, measured: dict | None) -> dict:
    runtime = llm.RUNTIME
    envelopes = (measured or {}).get("envelopes") or []
    seconds = (measured or {}).get("seconds") or []
    served = {str((env.get("runtime") or {}).get("served") or STUB_RUNTIME) for env in envelopes}
    return {
        "generatedAt": _now(),
        "runtime": proof.get("runtime"),
        "proof": proof,
        "contextProvenance": (measured or {}).get("contextProvenance"),
        "runs": len(envelopes),
        "servedRuntimes": sorted(served),
        "measures": {
            "residentMemory": resident_memory(runtime),
            "timings": timings(float(proof.get("probeSeconds") or 0.0), seconds),
            "overrideBranches": branch_distribution(envelopes),
            "proseSuppressions": prose_suppressions(envelopes),
            "schemaFailures": schema_failures(envelopes),
            "abstentionReachable": abstention_reachable(envelopes),
            "bannedAnalyst": banned_hits(envelopes),
        },
    }


def render_markdown(result: dict) -> str:
    m = result["measures"]
    rt = result.get("runtime") or {}
    lines = [
        "# Model runtime prod smoke",
        "",
        f"- Generated: {result['generatedAt']}",
        f"- Runtime: `{rt.get('name')}` · {rt.get('kind')} · model `{rt.get('model')}` · keyless",
        f"- Endpoint: `{rt.get('baseUrl')}`",
        f"- Runs: {result['runs']}",
        f"- Context provenance: {result.get('contextProvenance')}",
        f"- Served runtimes seen in the envelopes: {', '.join(result.get('servedRuntimes') or []) or '—'}",
        "",
        "## Proof that a real model served this run",
        "",
        "| check | ok | detail |",
        "|---|---|---|",
    ]
    for check in result["proof"].get("checks", []):
        lines.append(f"| `{check['check']}` | {'yes' if check['ok'] else 'NO'} | {check['detail']} |")
    lines += [
        "",
        "## Measurements",
        "",
        "| measure | value |",
        "|---|---|",
        f"| Resident memory while loaded | {_cell(m['residentMemory'])} |",
        f"| Cold load + first generation | {m['timings']['coldLoadPlusFirstGenerationSeconds']} s |",
        f"| Time to first token | {_cell(m['timings']['timeToFirstTokenSeconds'])} |",
        f"| Full analyst run | {_cell(m['timings']['fullAnalystRunSeconds'])} |",
        f"| Override-branch distribution | {m['overrideBranches']['counts']} |",
        f"| Branches never reached | {m['overrideBranches']['branchesNeverReached'] or 'none'} |",
        f"| `proseSuppressions` per thesis | {_num(m['proseSuppressions']['ratePerThesis'])} "
        f"({m['proseSuppressions']['suppressions']} over {m['proseSuppressions']['theses']}) |",
        f"| Schema-failure rate (structural) | {_num(m['schemaFailures']['structuralFailureRate'])} "
        f"({m['schemaFailures']['structuralFailures']} of {m['schemaFailures']['stages']} stages) |",
        f"| §9.52 `failed` path | {m['schemaFailures']['enrichmentFailedPath']['why']} |",
        f"| `neutral`/`unclear` reached by the MODEL | "
        f"{'yes' if m['abstentionReachable']['modelReachedAbstention'] else 'no'} "
        f"({m['abstentionReachable']['abstentions']}) |",
        f"| `BANNED_ANALYST` | {m['bannedAnalyst']['verdict']} |",
        "",
        "## Itemised `proseSuppressions`",
        "",
        f"- by field: `{m['proseSuppressions']['byField']}`",
        f"- by direction branch: `{m['proseSuppressions']['byDirectionBranch']}`",
        f"- literals: `{m['proseSuppressions']['literals']}`",
        "",
        "## For Wave 5A",
        "",
    ]
    per_run = m["timings"].get("secondsPerAnalystRun")
    if per_run:
        lines.append(
            f"`secondsPerAnalystRun = {per_run}`. `app.ablation`'s `plan_runs()` sizes the ladder in "
            "GENERATIONS; multiply the two for a wall-clock estimate. The ladder was sized in runs "
            "precisely so this stays a multiplication rather than a redesign."
        )
    else:
        lines.append("No analyst run completed, so 5A's per-run cost stays **unmeasured**.")
    lines += [
        "",
        "_Every field above describes the named runtime. Nothing here may be cited as a measurement "
        "of any other runtime, and nothing measured against `stub:offline` appears in this file — "
        "the proof gate above refuses to reach this section._",
        "",
    ]
    return "\n".join(lines)


def _cell(value) -> str:
    if isinstance(value, dict) and value.get("value") == UNMEASURED:
        return f"**unmeasured** — {value['reason']}"
    if isinstance(value, dict):
        return "`" + json.dumps(value, sort_keys=True) + "`"
    return str(value)


def _num(value) -> str:
    return "—" if value is None else f"{value:.4f}"


def write_report(result: dict, out_dir: str) -> dict:
    os.makedirs(out_dir, exist_ok=True)
    stem = "model-smoke-" + result["generatedAt"].replace(":", "")
    md_path = os.path.join(out_dir, f"{stem}.md")
    json_path = os.path.join(out_dir, f"{stem}.json")
    with open(md_path, "w", encoding="utf-8") as fh:
        fh.write(render_markdown(result))
    with open(json_path, "w", encoding="utf-8") as fh:
        # `envelopes` are dropped: the report is a measurement record, not a transcript store, and
        # persisted reads already have a home (`store.py`).
        slim = {k: v for k, v in result.items() if k != "envelopes"}
        json.dump(slim, fh, indent=2, sort_keys=True, default=str)
    return {"md": md_path, "json": json_path}


def main(argv: list[str] | None = None) -> int:
    runs = int(_env("SMOKE_RUNS", str(DEFAULT_RUNS)))
    out_dir = _env("SMOKE_OUT_DIR", os.path.join(_env("READS_DIR", "data/reads"), "smoke"))

    print(BAR)
    print("  MODEL RUNTIME PROD SMOKE — proving a real model before measuring anything")
    print(BAR)

    proof, code = prove_real_model()
    for check in proof["checks"]:
        print(f"  [{'ok ' if check['ok'] else 'FAIL'}] {check['check']}: {check['detail']}")
    if code != EXIT_OK:
        print(BAR)
        print("  REFUSED — no measurement is printed.")
        print("  A number produced without a proven model is a claim about a fixture wearing the")
        print("  model's name. `stub:offline` never licenses a claim about the model.")
        print(BAR)
        return code

    print(f"\n  Proof passed. Running {runs} analyst run(s)...\n")
    measured = measure(runs)
    result = report(proof, measured)
    files = write_report(result, out_dir)
    print(render_markdown(result))
    print(f"  Report written: {files['md']}")
    print(f"                  {files['json']}")
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
