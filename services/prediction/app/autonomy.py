"""Bounded autonomous challenger training and price evaluation.

The controller reacts to completed real bars, not wall-clock cadence: its timer only asks whether
new evidence exists. One immutable candidate is created per config after a fixed number of new bars,
then the existing parameter-free evaluator is started once for the batch. Every attempt is durable
in PostgreSQL, including refusals and failures.

This module has no deployment operation. It cannot promote or roll back a model, cannot change a
threshold, and cannot start/reset the official paper experiment. Those remain explicit operator
actions after the evidence has been reviewed.
"""
from __future__ import annotations

import os
import secrets
import signal
from dataclasses import dataclass
from threading import Event

import pandas as pd
import requests

from . import db, shadow
from .features import fetch_feature_frame
from .store import load_record

VALID_TIMEFRAMES = {"1D", "1H", "15m", "5m"}
TERMINAL_EVAL_STATES = {"done", "failed"}
OPEN_TRIAL_STATES = (
    "reserved", "trained", "evaluating", "evaluated", "evaluation-failed", "shadowing"
)


def _flag(name: str, default: bool = False) -> bool:
    raw = os.getenv(name, "").strip().lower()
    return default if not raw else raw in {"1", "true", "yes", "on"}


def _bounded_int(name: str, default: int, low: int, high: int) -> int:
    try:
        value = int(os.getenv(name, "").strip() or default)
    except ValueError:
        value = default
    return max(low, min(value, high))


@dataclass(frozen=True)
class ModelConfig:
    ticker: str
    timeframe: str
    horizon: int

    @property
    def key(self) -> str:
        return f"{self.ticker}:{self.timeframe}:{self.horizon}"


def _configs() -> tuple[ModelConfig, ...]:
    raw = (
        os.getenv("PREDICTION_AUTOMATION_CONFIGS", "").strip()
        or os.getenv("PAPER_CONFIGS", "").strip()
        or "NVDA:1D:5,GOOGL:1D:5"
    )
    out: list[ModelConfig] = []
    seen: set[str] = set()
    for item in raw.split(","):
        parts = [part.strip() for part in item.split(":")]
        if len(parts) != 3:
            continue
        ticker, timeframe = parts[0].upper(), parts[1]
        try:
            horizon = int(parts[2])
        except ValueError:
            continue
        cfg = ModelConfig(ticker=ticker, timeframe=timeframe, horizon=horizon)
        if not ticker or timeframe not in VALID_TIMEFRAMES or not 1 <= horizon <= 252:
            continue
        if cfg.key not in seen:
            out.append(cfg)
            seen.add(cfg.key)
    return tuple(out)


@dataclass(frozen=True)
class AutomationConfig:
    enabled: bool
    configs: tuple[ModelConfig, ...]
    base_url: str
    poll_seconds: int
    lease_seconds: int
    min_new_bars: int
    max_trials_per_config: int
    lookback_days: int
    shadow_min_paired_bars: int

    @classmethod
    def from_env(cls) -> "AutomationConfig":
        return cls(
            enabled=_flag("PREDICTION_AUTOMATION_ENABLED", False),
            configs=_configs(),
            base_url=os.getenv(
                "PREDICTION_AUTOMATION_BASE_URL", "http://127.0.0.1:8003"
            ).rstrip("/"),
            poll_seconds=_bounded_int("PREDICTION_AUTOMATION_POLL_SECONDS", 300, 30, 3600),
            lease_seconds=_bounded_int("PREDICTION_AUTOMATION_LEASE_SECONDS", 600, 120, 3600),
            min_new_bars=_bounded_int("PREDICTION_AUTOMATION_MIN_NEW_BARS", 5, 1, 252),
            max_trials_per_config=_bounded_int(
                "PREDICTION_AUTOMATION_MAX_TRIALS", 3, 1, 20
            ),
            lookback_days=_bounded_int(
                "PREDICTION_AUTOMATION_LOOKBACK_DAYS", 1500, 400, 5000
            ),
            shadow_min_paired_bars=_bounded_int(
                "PREDICTION_SHADOW_MIN_PAIRED_BARS", 20, 5, 252
            ),
        )


def _http_json(
    cfg: AutomationConfig,
    method: str,
    path: str,
    *,
    params: dict | None = None,
    timeout: int = 130,
) -> dict:
    response = requests.request(
        method, cfg.base_url + path, params=params, timeout=timeout
    )
    response.raise_for_status()
    payload = response.json()
    if not isinstance(payload, dict):
        raise RuntimeError(f"{path} returned a non-object response")
    return payload


def _bar_progress(cfg: AutomationConfig, model_cfg: ModelConfig) -> tuple[str, int, str]:
    frame, source, synthetic = fetch_feature_frame(
        model_cfg.ticker, model_cfg.timeframe, cfg.lookback_days
    )
    if synthetic:
        raise RuntimeError(f"latest completed frame is synthetic ({source})")
    if frame.empty:
        raise RuntimeError("latest completed frame has no rows")
    index = pd.to_datetime(frame.index, utc=True, errors="coerce")
    index = index[~index.isna()]
    if len(index) == 0:
        raise RuntimeError("completed frame has no parseable bar timestamps")
    latest = index.max()
    baseline_raw = db.automation_trial_baseline(
        model_cfg.ticker, model_cfg.timeframe, model_cfg.horizon
    )
    if not baseline_raw:
        return latest.isoformat(), len(index), source
    baseline = pd.to_datetime(baseline_raw, utc=True, errors="coerce")
    if pd.isna(baseline):
        raise RuntimeError(f"stored dataThrough is not parseable: {baseline_raw}")
    return latest.isoformat(), int((index > baseline).sum()), source


def _match_verdict(status: dict, trial: dict) -> dict | None:
    for row in status.get("verdicts") or []:
        if (
            row.get("ticker") == trial["ticker"]
            and row.get("timeframe") == trial["timeframe"]
            and row.get("horizon") == trial["horizon"]
        ):
            return row
    return None


def _reconcile_evaluation(cfg: AutomationConfig, summary: dict) -> dict:
    status = _http_json(cfg, "GET", "/evaluate/status", timeout=30)
    evaluating = db.list_automation_trials(statuses=("evaluating",), limit=100)
    if evaluating and status.get("state") in TERMINAL_EVAL_STATES:
        for trial in evaluating:
            # A completed manual/older run must not be attached to a newer autonomous trial.
            if status.get("startedAt") != trial.get("evaluationStartedAt"):
                seen = pd.to_datetime(status.get("startedAt"), utc=True, errors="coerce")
                expected = pd.to_datetime(
                    trial.get("evaluationStartedAt"), utc=True, errors="coerce"
                )
                if not pd.isna(seen) and not pd.isna(expected) and seen > expected:
                    db.finish_automation_evaluation(
                        trial["id"], evaluation=status, finished_at=status.get("finishedAt"),
                        error="evaluation run record was superseded before this trial reconciled",
                    )
                    summary["evaluationFailed"].append(trial["id"])
                continue
            if status.get("state") == "failed":
                error = status.get("note") or status.get("exitMeaning") or "evaluation failed"
                db.finish_automation_evaluation(
                    trial["id"], evaluation=status, finished_at=status.get("finishedAt"),
                    error=str(error)[:500],
                )
                summary["evaluationFailed"].append(trial["id"])
                continue
            verdict = _match_verdict(status, trial)
            if verdict is None:
                db.finish_automation_evaluation(
                    trial["id"], evaluation=status, finished_at=status.get("finishedAt"),
                    error="evaluation completed without a verdict for this config",
                )
                summary["evaluationFailed"].append(trial["id"])
                continue
            db.finish_automation_evaluation(
                trial["id"],
                evaluation={"run": {
                    "startedAt": status.get("startedAt"),
                    "finishedAt": status.get("finishedAt"),
                    "report": status.get("latestReport"),
                }, "verdict": verdict},
                finished_at=status.get("finishedAt"),
            )
            summary["evaluated"].append(trial["id"])
    return status


def _train_due_configs(cfg: AutomationConfig, summary: dict) -> None:
    open_keys = {
        f"{row['ticker']}:{row['timeframe']}:{row['horizon']}"
        for row in db.list_automation_trials(statuses=OPEN_TRIAL_STATES, limit=500)
    }
    for model_cfg in cfg.configs:
        if model_cfg.key in open_keys:
            summary["skipped"].append({"config": model_cfg.key, "reason": "trial-in-progress"})
            continue
        if db.count_automation_trials(
            model_cfg.ticker, model_cfg.timeframe, model_cfg.horizon
        ) >= cfg.max_trials_per_config:
            summary["skipped"].append({"config": model_cfg.key, "reason": "trial-budget-spent"})
            continue
        try:
            trigger_bar, new_bars, source = _bar_progress(cfg, model_cfg)
        except Exception as exc:  # noqa: BLE001 — one config must not stop the other configs
            summary["skipped"].append({
                "config": model_cfg.key, "reason": f"data-unavailable: {exc}"[:500]
            })
            continue
        if new_bars < cfg.min_new_bars:
            summary["skipped"].append({
                "config": model_cfg.key,
                "reason": f"new-bars {new_bars}/{cfg.min_new_bars}",
                "latestBar": trigger_bar,
            })
            continue

        active = load_record(model_cfg.ticker, model_cfg.timeframe, model_cfg.horizon)
        champion = (active or {}).get("modelVersion")
        allow_short = bool(((active or {}).get("report") or {}).get("allowShort", False))
        trial = db.reserve_automation_trial(
            model_cfg.ticker, model_cfg.timeframe, model_cfg.horizon, champion, trigger_bar
        )
        if trial is None:
            summary["skipped"].append({"config": model_cfg.key, "reason": "already-reserved"})
            continue
        try:
            trained = _http_json(
                cfg, "POST", f"/train/{model_cfg.ticker}",
                params={
                    "timeframe": model_cfg.timeframe,
                    "horizon": model_cfg.horizon,
                    "allowShort": str(allow_short).lower(),
                    "lookbackDays": cfg.lookback_days,
                },
            )
            if not trained.get("trained") or not trained.get("candidate"):
                raise RuntimeError(trained.get("reason") or "training produced no candidate")
            db.finish_automation_training(
                trial["id"], candidate_model_version=trained.get("modelVersion"),
                strategy_version=trained.get("strategyVersion"),
                data_through=trained.get("dataThrough"),
            )
            summary["trained"].append({
                "trialId": trial["id"], "config": model_cfg.key,
                "candidateModelVersion": trained.get("modelVersion"),
                "dataThrough": trained.get("dataThrough"), "source": source,
            })
        except Exception as exc:  # noqa: BLE001 — persist the failed attempt and continue
            db.finish_automation_training(trial["id"], error=str(exc)[:500])
            summary["trainingFailed"].append({
                "trialId": trial["id"], "config": model_cfg.key, "error": str(exc)[:500]
            })


def _start_evaluation(cfg: AutomationConfig, status: dict, summary: dict) -> None:
    pending = db.list_automation_trials(statuses=("trained",), limit=100)
    if not pending:
        return
    if status.get("state") == "running":
        summary["evaluation"] = "waiting-for-existing-run"
        return
    try:
        started = _http_json(cfg, "POST", "/evaluate/run", timeout=30)
    except requests.HTTPError as exc:
        if exc.response is not None and exc.response.status_code == 409:
            summary["evaluation"] = "waiting-for-existing-run"
            return
        raise
    started_at = started.get("startedAt")
    if not started.get("started") or not started_at:
        raise RuntimeError("evaluation runner did not return startedAt")
    trial_ids = [int(row["id"]) for row in pending]
    db.mark_automation_trials_evaluating(trial_ids, started_at)
    summary["evaluation"] = {"startedAt": started_at, "trialIds": trial_ids}


def _advance_shadows(cfg: AutomationConfig, summary: dict) -> None:
    trials = db.list_automation_trials(
        statuses=("evaluated", "evaluation-failed", "shadowing"), limit=100
    )
    for trial in trials:
        try:
            result = shadow.advance_trial(
                trial, lookback_days=cfg.lookback_days,
                min_paired_bars=cfg.shadow_min_paired_bars,
            )
            summary["shadow"].append({"trialId": trial["id"], **result})
        except shadow.ShadowDeferred as exc:
            summary["shadowDeferred"].append({
                "trialId": trial["id"], "reason": str(exc)[:500]
            })
        except shadow.ShadowInvalid as exc:
            db.set_automation_trial_status(trial["id"], "shadow-failed", str(exc)[:500])
            summary["shadowFailed"].append({
                "trialId": trial["id"], "error": str(exc)[:500]
            })


def run_once(cfg: AutomationConfig | None = None) -> dict:
    cfg = cfg or AutomationConfig.from_env()
    summary: dict = {
        "enabled": cfg.enabled,
        "configs": [item.key for item in cfg.configs],
        "trained": [], "trainingFailed": [], "evaluated": [], "evaluationFailed": [],
        "shadow": [], "shadowDeferred": [], "shadowFailed": [],
        "skipped": [], "evaluation": "idle",
    }
    if not cfg.enabled:
        return {**summary, "reason": "disabled"}
    if not db.enabled():
        return {**summary, "reason": "postgresql-required"}
    if not cfg.configs:
        return {**summary, "reason": "no-valid-configs"}

    token = secrets.token_hex(16)
    if not db.acquire_automation_lease(token, cfg.lease_seconds):
        return {**summary, "reason": "controller-locked"}
    error: str | None = None
    try:
        eval_status = _reconcile_evaluation(cfg, summary)
        _advance_shadows(cfg, summary)
        _train_due_configs(cfg, summary)
        # Re-read status only when the reconciliation completed an old run; it is cheap and avoids
        # treating the just-finished lock as still held while starting the next candidate batch.
        if summary["evaluated"] or summary["evaluationFailed"]:
            eval_status = _http_json(cfg, "GET", "/evaluate/status", timeout=30)
        _start_evaluation(cfg, eval_status, summary)
        return summary
    except Exception as exc:  # noqa: BLE001 — the next poll must survive transient outages
        error = f"{type(exc).__name__}: {exc}"[:500]
        return {**summary, "reason": "controller-error", "error": error}
    finally:
        db.release_automation_lease(token, error)


def status(cfg: AutomationConfig | None = None) -> dict:
    cfg = cfg or AutomationConfig.from_env()
    base = {
        "enabled": cfg.enabled,
        "storage": "postgresql" if db.enabled() else "unavailable",
        "configs": [item.key for item in cfg.configs],
        "policy": {
            "trigger": f"{cfg.min_new_bars} new completed real bars",
            "maxTrialsPerConfig": cfg.max_trials_per_config,
            "shadowMinPairedBars": cfg.shadow_min_paired_bars,
            "pollSeconds": cfg.poll_seconds,
            "promotion": "manual-only",
            "thresholdTuning": False,
        },
        "trials": [],
    }
    if not db.enabled():
        return {**base, "available": False, "reason": "postgresql-required"}
    try:
        trials = db.list_automation_trials(limit=100)
        # Shadow returns belong to the immutable trial that produced them. Attach the summary here
        # so the gateway/UI never has to reconstruct evidence or make one request per trial.
        for trial in trials:
            rows = db.load_shadow_observations(int(trial["id"]))
            trial["shadow"] = shadow.metrics(rows, cfg.shadow_min_paired_bars)
        return {
            **base, "available": True,
            "controller": db.automation_controller_status(),
            "trials": trials,
        }
    except Exception as exc:  # noqa: BLE001 — status is diagnostic and fail-soft
        return {**base, "available": False, "reason": f"{type(exc).__name__}: {exc}"[:500]}


def run_forever(*, stop_event: Event | None = None, emit=print) -> int:
    cfg = AutomationConfig.from_env()
    if not cfg.enabled:
        emit(str(run_once(cfg)))
        return 0
    stop = stop_event or Event()
    while not stop.is_set():
        emit(str(run_once(cfg)))
        stop.wait(cfg.poll_seconds)
    return 0


def main() -> int:
    stop = Event()

    def request_stop(_signum, _frame) -> None:
        stop.set()

    signal.signal(signal.SIGTERM, request_stop)
    signal.signal(signal.SIGINT, request_stop)
    return run_forever(stop_event=stop, emit=lambda line: print(line, flush=True))


if __name__ == "__main__":
    raise SystemExit(main())
