"""In-app runners for the price and PEAD offline evaluation harnesses.

WHY THIS EXISTS. Production is the single-container supervisord deploy, where the operator has no
shell and no docker access — the only surface is the application itself. Retraining was already
reachable (`POST /api/train/{ticker}`); the evaluator was not, so a verdict could not be produced in
production AT ALL. `docs/VALIDATION_AND_GO_LIVE.md` §2.3 was an instruction nobody in that
deployment could follow.

WHAT THIS IS NOT. It adds a way to RUN the harness. It adds no way to influence what the harness
CONCLUDES. Nothing here reads, writes, recomputes or re-tags a verdict: `evaluate.py` decides, and
`verdicts.py` persists what it decided, exactly as they did from the CLI. This module starts a
process, watches it, and reports what it did.

  * NO PARAMETERS (see `assert_no_parameters`). Runs use only the deployment's `EVAL_*` / `EVENT_*`
    environment, so request-time tuning cannot masquerade as the pre-registered strategy/study.
  * SINGLE-FLIGHT, via an atomic O_CREAT|O_EXCL lock file under `EVAL_OUT_DIR`. Two concurrent
    harnesses share one CPU budget; their logs, status records and reports remain separate.
  * MANUAL ONLY. Nothing here is scheduled, and nothing may schedule it. The harness is also
    CPU-heavy: `/predict` latency degrades measurably while a run is in flight, which is an accepted
    trade on a single-box deployment — and a reason a run is an operator's deliberate act, never a
    side effect of a page load.
  * EXIT CODES ARE MEANINGS, NOT ERRORS. 0 is "a verdict was produced" — including NO EDGE,
    INCONCLUSIVE and SUSPECT, which are successes of the harness and the most likely outcomes. 2 and
    3 are the harness REFUSING (synthetic data; nothing fetched), which is it working correctly.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import threading
from datetime import datetime, timezone

from . import config as _config
from . import verdicts as _verdicts
from . import db as _db
from .store import load_record

LOCK_NAME = "eval-run.lock"
LAST_RUN_NAME = "last-run.json"
LOG_PREFIX = "run-"
EVENT_LAST_RUN_NAME = "event-last-run.json"
EVENT_LOG_PREFIX = "event-run-"
ESTIMATE_LOCK_NAME = "estimate-run.lock"
ESTIMATE_LAST_RUN_NAME = "estimate-last-run.json"
ESTIMATE_LOG_PREFIX = "estimate-run-"
LOG_SUFFIX = ".log"
LOG_TAIL_LINES = 50

# Exit codes of `python -m app.evaluate`, named. Mirrors evaluate.EXIT_OK / EXIT_SYNTHETIC /
# EXIT_NO_DATA — imported by value rather than by reference so a status read never has to import
# numpy/pandas just to describe a finished run.
EXIT_MEANING = {
    0: "a verdict was produced (EDGE / NO EDGE / INCONCLUSIVE / SUSPECT — all of them are results)",
    2: "refused: a ticker resolved to synthetic data, so no verdict may be produced",
    3: "refused: nothing was fetched (analysis unreachable, or no usable history)",
}
UNKNOWN_EXIT = "the harness exited with an unrecognised code — read the log"

# A non-zero exit is not a crash: 2 and 3 are the harness refusing on purpose. Callers render these
# as honest refusals rather than as failures of the machinery.
REFUSAL_EXITS = (2, 3)
EVENT_EXIT_MEANING = {
    0: "a PEAD verdict was produced (EDGE / NO EDGE / INCONCLUSIVE / SUSPECT are results)",
    2: "refused: a ticker or benchmark resolved to synthetic data",
    3: "refused: no usable price, benchmark, or event window was available",
    4: "refused: no durable earnings history was available and no provider key was configured",
}
EVENT_REFUSAL_EXITS = (2, 3, 4)
ESTIMATE_EXIT_MEANING = {
    0: "the bounded estimate snapshot pass completed; inspect the result counts below",
    3: "refused: PostgreSQL, the provider key, or the provider calendar was unavailable",
}
ESTIMATE_REFUSAL_EXITS = (3,)

NO_PARAMETERS_RULE = (
    "POST /evaluate/run takes no parameters. The run uses only this deployment's EVAL_* "
    "environment, so every verdict it mints carries the deployment's own strategy version and can "
    "never be a custom-parameter run masquerading as the default strategy "
    "(docs/PAPER_EXECUTION_CONTRACT.md §4.3). To evaluate a different strategy, change the "
    "deployment's configuration — not one request."
)
EVENT_NO_PARAMETERS_RULE = (
    "POST /evaluate-events/run takes no parameters. The run uses only this deployment's EVENT_* "
    "environment and the pre-registered PEAD rule; request-time tuning is deliberately forbidden."
)
ESTIMATE_NO_PARAMETERS_RULE = (
    "POST /estimate-snapshots/run takes no parameters. The collector uses only this deployment's "
    "EVENT_UNIVERSE and ESTIMATE_* environment, and stores immutable pre-release evidence."
)


def _names(kind: str) -> dict:
    if kind == "estimate":
        return {
            "lock": ESTIMATE_LOCK_NAME, "last": ESTIMATE_LAST_RUN_NAME,
            "logPrefix": ESTIMATE_LOG_PREFIX, "reportPrefix": "estimate-report-",
            "module": "app.estimate_snapshots", "exitMeaning": ESTIMATE_EXIT_MEANING,
            "refusalExits": ESTIMATE_REFUSAL_EXITS,
        }
    if kind == "event":
        return {
            # Both harnesses share one CPU-heavy-job lock. Their state/report records stay separate,
            # but a PEAD run may not pile onto a price run (or vice versa) on the one-box deploy.
            "lock": LOCK_NAME, "last": EVENT_LAST_RUN_NAME,
            "logPrefix": EVENT_LOG_PREFIX, "reportPrefix": "event-report-",
            "module": "app.evaluate_events", "exitMeaning": EVENT_EXIT_MEANING,
            "refusalExits": EVENT_REFUSAL_EXITS,
        }
    if kind != "price":
        raise ValueError(f"unknown evaluation kind: {kind}")
    return {
        "lock": LOCK_NAME, "last": LAST_RUN_NAME, "logPrefix": LOG_PREFIX,
        "reportPrefix": "report-", "module": "app.evaluate", "exitMeaning": EXIT_MEANING,
        "refusalExits": REFUSAL_EXITS,
    }


class RunInFlight(Exception):
    """Raised by `start` when a run already holds the lock. Carries the running job's status."""

    def __init__(self, status: dict):
        super().__init__("an evaluation run is already in flight")
        self.status = status


class ParametersRejected(Exception):
    """Raised when a caller supplied a query string or body fields (see NO_PARAMETERS_RULE)."""


def _now() -> datetime:
    return datetime.now(timezone.utc)


def _stamp(dt: datetime) -> str:
    return dt.strftime("%Y%m%dT%H%M%SZ")


def out_dir(explicit: str | None = None, *, kind: str = "price") -> str:
    """The evaluator's output directory. Read from `config` at CALL time (not import time) so a test
    — or a deployment that rewrites the env — sees the value that is actually configured."""
    if explicit:
        return explicit
    if kind in ("event", "estimate"):
        return os.getenv("EVENT_OUT_DIR", "").strip() or _config.EVAL_OUT_DIR
    return _config.EVAL_OUT_DIR


def service_dir() -> str:
    """The directory `app.evaluate` must run from: the one containing the `app` package, so
    `-m app.evaluate` resolves and its relative default paths match the service's own."""
    return os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def _lock_path(d: str, kind: str = "price") -> str:
    return os.path.join(d, _names(kind)["lock"])


def _last_run_path(d: str, kind: str = "price") -> str:
    return os.path.join(d, _names(kind)["last"])


def _write_atomic(path: str, payload: dict) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(payload, f, indent=2)
    os.replace(tmp, path)


def _read_json(path: str) -> dict | None:
    try:
        with open(path) as f:
            data = json.load(f)
    except (OSError, json.JSONDecodeError):
        return None
    return data if isinstance(data, dict) else None


def _write_last_run(d: str, payload: dict, kind: str = "price") -> None:
    name = _names(kind)["last"]
    if _db.enabled():
        _db.save_artifact(name, "application/json", json.dumps(payload, indent=2).encode())
    else:
        _write_atomic(_last_run_path(d, kind), payload)


def _read_last_run(d: str, kind: str = "price") -> dict | None:
    name = _names(kind)["last"]
    if _db.enabled():
        raw = _db.load_artifact(name)
        if raw is None:
            return None
        try:
            value = json.loads(raw)
        except (TypeError, json.JSONDecodeError):
            return None
        return value if isinstance(value, dict) else None
    return _read_json(_last_run_path(d, kind))


def _pid_alive(pid: object) -> bool:
    """Fail-safe in the direction that matters: a pid we cannot interrogate is treated as ALIVE, so
    we never start a second harness on top of a live one. An absent or unparseable pid is dead —
    otherwise a lock written by a process that died mid-creation would block every future run."""
    try:
        pid_i = int(pid)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return False
    if pid_i <= 0:
        return False
    try:
        os.kill(pid_i, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except OSError:
        return True
    return True


# --------------------------------------------------------------------------------- the lock

def _acquire(d: str, kind: str = "price") -> int:
    """Create the lock atomically, reclaiming exactly one stale lock. Returns the open fd.

    A stale lock (the recorded pid is gone, or the file never got its contents) is reclaimed with a
    LOG LINE, never silently: "the previous run vanished" is information an operator needs, and a
    silent reclaim is indistinguishable from a lock that never existed.
    """
    os.makedirs(d, exist_ok=True)
    path = _lock_path(d, kind)
    try:
        return os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o644)
    except FileExistsError:
        pass

    held = _read_json(path)
    if held is not None and _pid_alive(held.get("pid")):
        held_kind = held.get("kind") if held.get("kind") in ("price", "event", "estimate") else "price"
        raise RunInFlight(status(d, kind=held_kind))

    print(
        f"[evalrun] reclaiming stale lock {path}: "
        f"{'pid ' + str(held.get('pid')) + ' is gone' if held else 'lock file was empty or unreadable'}"
    )
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass
    # One retry only. If someone else won the race in between, they hold a LIVE lock and the
    # FileExistsError below is correct — we do not loop, because a loop here is a way to eventually
    # start a second harness.
    try:
        return os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o644)
    except FileExistsError:
        held = _read_json(path) or {}
        held_kind = held.get("kind") if held.get("kind") in ("price", "event", "estimate") else "price"
        raise RunInFlight(status(d, kind=held_kind)) from None


def _release(d: str, kind: str = "price") -> None:
    try:
        os.unlink(_lock_path(d, kind))
    except FileNotFoundError:
        pass


# --------------------------------------------------------------------------------- starting a run

def assert_no_parameters(query_string: str, body: bytes | None, *, kind: str = "price") -> None:
    """Refuse a request that carried a query string or a non-empty JSON body.

    Ignoring the parameters would be worse than refusing them: the caller would believe they had
    asked for something the run did not do, and the resulting verdict would be attributed to a
    strategy nobody ran. See NO_PARAMETERS_RULE.
    """
    rule = {
        "price": NO_PARAMETERS_RULE,
        "event": EVENT_NO_PARAMETERS_RULE,
        "estimate": ESTIMATE_NO_PARAMETERS_RULE,
    }.get(kind)
    if rule is None:
        raise ValueError(f"unknown evaluation kind: {kind}")
    if query_string:
        raise ParametersRejected(rule)
    if not body:
        return
    text = body.decode("utf-8", "replace").strip()
    if text in ("", "{}", "null"):
        return
    try:
        parsed = json.loads(text)
    except json.JSONDecodeError:
        raise ParametersRejected(rule) from None
    if parsed in (None, {}, []):
        return
    raise ParametersRejected(rule)


def _spawn(log_file: str, cwd: str, module: str = "app.evaluate"):
    """Start the harness as a subprocess, stdout+stderr both to the run log. The service's own
    environment is inherited verbatim — that inheritance IS the no-parameters rule in practice."""
    log = open(log_file, "wb", buffering=0)  # noqa: SIM115 — closed by the reaper thread
    try:
        proc = subprocess.Popen(  # noqa: S603 — fixed argv, no shell, no caller-supplied values
            [sys.executable, "-m", module],
            cwd=cwd,
            stdout=log,
            stderr=subprocess.STDOUT,
        )
    except Exception:
        log.close()
        raise
    return proc, log


def start(d: str | None = None, *, kind: str = "price") -> dict:
    """Start ONE configured evaluation or collector job. Returns its process metadata.

    Raises RunInFlight (-> 409) when one is already running. The harness takes minutes: it must
    never run inside the request, so this returns as soon as the process exists.
    """
    d = out_dir(d, kind=kind)
    names = _names(kind)
    fd = _acquire(d, kind)

    started_at = _now()
    log_file = os.path.join(d, names["logPrefix"] + _stamp(started_at) + LOG_SUFFIX)
    try:
        if kind == "price":
            proc, log = _spawn(log_file, service_dir())
        else:
            proc, log = _spawn(log_file, service_dir(), names["module"])
    except Exception:
        os.close(fd)
        _release(d, kind)
        raise

    record = {
        "pid": proc.pid,
        "startedAt": started_at.isoformat(),
        "logFile": os.path.basename(log_file),
        "kind": kind,
    }
    with os.fdopen(fd, "w") as f:
        json.dump(record, f, indent=2)

    try:
        _write_last_run(d, {"state": "running", **record}, kind)
    except Exception:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except Exception:
            proc.kill()
        log.close()
        _release(d, kind)
        raise

    _watch(proc, log, d, record, kind=kind)
    return {"started": True, **record}


def _watch(proc, log, d: str, record: dict, *, kind: str = "price") -> None:
    """Reap the subprocess on a daemon thread and record the outcome.

    The thread is best-effort by design: if the service restarts mid-run it dies with the process
    that owned it, and the NEXT status read notices the recorded pid is gone and reports `failed`
    with a note. Detection lives in `status`, not here, because a watcher that has itself died
    cannot report anything.
    """

    def _reap() -> None:
        try:
            code = proc.wait()
        finally:
            try:
                log.close()
            except OSError:
                pass
        finished = _now()
        completed = {
            "state": "done" if code == 0 else "failed",
            "startedAt": record["startedAt"],
            "finishedAt": finished.isoformat(),
            "exitCode": code,
            "exitMeaning": _names(kind)["exitMeaning"].get(code, UNKNOWN_EXIT),
            "refusal": code in _names(kind)["refusalExits"],
            "logFile": record["logFile"],
            "pid": record["pid"],
        }
        try:
            if _db.enabled():
                try:
                    with open(os.path.join(d, record["logFile"]), "rb") as f:
                        _db.save_artifact(record["logFile"], "text/plain", f.read())
                except OSError:
                    pass
            _write_last_run(d, completed, kind)
        except Exception as exc:
            # The evaluator has already exited. A metadata write failure must not strand the
            # single-flight lock forever; reports/verdicts have their own durable writes.
            print(f"[evalrun] could not persist completed-run metadata: {exc}")
        finally:
            _release(d, kind)

    threading.Thread(target=_reap, name="evalrun-reaper", daemon=True).start()


# --------------------------------------------------------------------------------- reporting

def _log_tail(d: str, log_file: str | None, lines: int = LOG_TAIL_LINES) -> list[str]:
    if not log_file:
        return []
    path = os.path.join(d, os.path.basename(log_file))
    try:
        with open(path, "rb") as f:
            raw = f.read()
    except OSError:
        if not _db.enabled():
            return []
        raw = _db.load_artifact(os.path.basename(log_file))
        if raw is None:
            return []
    text = raw.decode("utf-8", "replace")
    return text.splitlines()[-lines:]


def _collector_result(d: str, log_file: str | None) -> dict | None:
    """Parse the estimate collector's JSON stdout; its log contains exactly one result object."""
    if not log_file:
        return None
    path = os.path.join(d, os.path.basename(log_file))
    try:
        with open(path, "rb") as f:
            raw = f.read()
    except OSError:
        if not _db.enabled():
            return None
        raw = _db.load_artifact(os.path.basename(log_file))
        if raw is None:
            return None
    try:
        value = json.loads(raw)
    except (TypeError, json.JSONDecodeError):
        return None
    return value if isinstance(value, dict) else None


def latest_report(d: str | None = None, *, kind: str = "price") -> dict | None:
    """The newest `report-*.json`, summarised. None when the harness has never written one.

    Surface the verdict plus its compact checklist. The full per-ticker report stays out of this
    status response, but a NO EDGE label without the failed checks is not actionable evidence.
    """
    d = out_dir(d, kind=kind)
    if kind == "estimate":
        return None
    if _db.enabled():
        stored = _db.latest_artifact(_names(kind)["reportPrefix"], ".json")
        if stored is None:
            return None
        newest, raw = stored
        try:
            report = json.loads(raw)
        except (TypeError, json.JSONDecodeError):
            report = None
    else:
        try:
            names = sorted(
                n for n in os.listdir(d)
                if n.startswith(_names(kind)["reportPrefix"]) and n.endswith(".json")
            )
        except OSError:
            return None
        if not names:
            return None
        newest = names[-1]  # the stamp is lexicographically ordered (…%Y%m%dT%H%M%SZ…)
        report = _read_json(os.path.join(d, newest))
    if report is None:
        return {"file": newest, "unreadable": True}
    by_horizon = report.get("byHorizon")
    summary = {}
    if isinstance(by_horizon, dict):
        for h, block in by_horizon.items():
            if isinstance(block, dict):
                checklist = block.get("checklist")
                if not isinstance(checklist, list):
                    checklist = block.get("reasons") if isinstance(block.get("reasons"), list) else []
                pooled = block.get("pooled") if isinstance(block.get("pooled"), dict) else {}
                if kind == "event" and not pooled:
                    pooled = {"all": block.get("all"), "holdout": block.get("holdout")}
                summary[str(h)] = {
                    "verdict": block.get("verdict"),
                    "checklist": checklist,
                    "pooled": {
                        "all": pooled.get("all"),
                        "holdout": pooled.get("holdout"),
                    },
                    "buyHold": block.get("buyHold"),
                    "permutation": block.get("permutation"),
                    "sufficiency": block.get("sufficiency"),
                }
    return {
        "file": newest,
        "verdict": report.get("verdict"),
        "meaning": report.get("meaning"),
        "method": report.get("method"),
        "generatedAt": report.get("generatedAt"),
        "strategyVersion": (report.get("config") or {}).get("strategyVersion"),
        "studyVersion": (report.get("config") or {}).get("studyVersion"),
        "byHorizon": summary,
        # A refusal report has no verdict and says so in its own field; surfaced verbatim.
        "refused": report.get("refused"),
    }


def stored_verdicts(d: str | None = None) -> list[dict]:
    """Every persisted verdict record, each with the SERVED RECORD'S `current` flag.

    `current` comes from `verdicts.evaluation_block` against that config's own trained record — the
    same computation `/predict` serves and the same one paper gate 4 spends. Nothing is recomputed
    here, so the operator sees from the app exactly whether gate 4 would now pass, not a second
    opinion about it.
    """
    d = out_dir(d)
    rows: list[dict] = []
    for name, record in _verdicts.list_verdicts(d):
        if record is None:
            rows.append({"file": name, "unreadable": True, "current": False})
            continue
        ticker = record.get("ticker")
        timeframe = record.get("timeframe")
        horizon = record.get("horizon")
        block = None
        if isinstance(ticker, str) and isinstance(timeframe, str) and isinstance(horizon, int):
            served = load_record(ticker, timeframe, horizon)
            block = _verdicts.evaluation_block(
                ticker, timeframe, horizon, (served or {}).get("report"), out_dir=d,
                data_policy=(served or {}).get("dataPolicy"),
            )
        rows.append({
            "file": name,
            "ticker": ticker,
            "timeframe": timeframe,
            "horizon": horizon,
            "verdict": record.get("verdict"),
            "evaluatedAt": record.get("evaluatedAt"),
            "scope": record.get("scope"),
            "report": record.get("report"),
            "strategyVersion": record.get("strategyVersion"),
            # False when there is no trained record to compare against — fail closed, exactly as
            # gate 4 does. "We could not establish a match" is not a match.
            "current": bool(block and block.get("current")),
            "expectedStrategyVersion": (block or {}).get("expectedStrategyVersion"),
            "method": (block or {}).get("method"),
            "expectedMethod": (block or {}).get("expectedMethod"),
            "dataPolicy": (block or {}).get("dataPolicy"),
            "servedDataPolicy": (block or {}).get("servedDataPolicy"),
            "expectedDataPolicy": (block or {}).get("expectedDataPolicy"),
            "dataPolicyCurrent": bool(block and block.get("dataPolicyCurrent")),
        })
    return rows


def status(d: str | None = None, *, kind: str = "price") -> dict:
    """The whole operator view: run state, the last exit and what it MEANT, a log tail, the newest
    report's verdict, and every stored verdict with its `current` flag."""
    d = out_dir(d, kind=kind)
    held = None
    lock_exists = os.path.exists(_lock_path(d, kind))
    if lock_exists:
        held = _read_json(_lock_path(d, kind))
    last = _read_last_run(d, kind)

    state = "idle"
    note = None
    started_at = None
    finished_at = None
    exit_code = None
    exit_meaning = None
    refusal = False
    log_file = None
    result_name = "collector result" if kind == "estimate" else "verdict"
    restart_path = {
        "price": "/evaluate/run",
        "event": "/evaluate-events/run",
        "estimate": "/estimate-snapshots/run",
    }[kind]

    if lock_exists:
        started_at = (held or {}).get("startedAt")
        held_kind = (held or {}).get("kind") or "price"
        same_kind = held_kind == kind
        log_file = (held or {}).get("logFile") if same_kind else None
        if held is not None and _pid_alive(held.get("pid")):
            state = "running"
            if not same_kind:
                note = (
                    f"a {held_kind} evaluation is running; PEAD and price evaluation share one "
                    "single-flight CPU budget, so this runner can start after it finishes."
                )
        else:
            # The lock outlived the process that held it: the service (or the box) restarted
            # mid-run. Say so, rather than reporting `running` forever against a pid that is gone.
            state = "failed"
            note = (
                "the previous run's process is gone but its lock remains — the service most likely "
                f"restarted mid-run. No {result_name} was recorded for it. The lock is reclaimed "
                f"by the next POST {restart_path}."
            )
    elif last is not None and last.get("state") == "running":
        started_at = last.get("startedAt")
        log_file = last.get("logFile")
        state = "failed"
        note = (
            f"the service restarted while the previous run was active. No completed {result_name} "
            "was recorded for it; start a new run."
        )
    elif last is not None:
        started_at = last.get("startedAt")
        finished_at = last.get("finishedAt")
        exit_code = last.get("exitCode")
        exit_meaning = last.get("exitMeaning") or _names(kind)["exitMeaning"].get(
            exit_code, UNKNOWN_EXIT
        )
        refusal = bool(last.get("refusal"))
        log_file = last.get("logFile")
        state = "done" if exit_code == 0 else "failed"

    return {
        "state": state,
        "note": note,
        "startedAt": started_at,
        "finishedAt": finished_at,
        "exitCode": exit_code,
        "exitMeaning": exit_meaning,
        # A non-zero exit that is 2 or 3 is the harness REFUSING, not the machinery breaking. The
        # distinction is served so a UI never has to infer it from the number.
        "refusal": refusal,
        "logFile": log_file,
        "logTail": _log_tail(d, log_file),
        "latestReport": latest_report(d, kind=kind),
        "collectorResult": _collector_result(d, log_file) if kind == "estimate" else None,
        "verdicts": stored_verdicts(d) if kind == "price" else [],
        "outDir": d,
        "kind": kind,
    }


def start_event(d: str | None = None) -> dict:
    return start(d, kind="event")


def status_event(d: str | None = None) -> dict:
    return status(d, kind="event")


def start_estimates(d: str | None = None) -> dict:
    return start(d, kind="estimate")


def status_estimates(d: str | None = None) -> dict:
    return status(d, kind="estimate")
