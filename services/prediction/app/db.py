"""Small PostgreSQL repository for durable prediction evidence.

The prediction service has two storage modes:

* PostgreSQL when ``PREDICTION_DATABASE_URL`` or ``DATABASE_URL`` is configured. This is the
  production path and owns the ``prediction`` schema by default.
* The existing files when no database URL is configured. This keeps deterministic unit tests and
  the zero-configuration local workflow working without pretending those files are durable.

Connections are intentionally short lived. Evaluation and training writes are infrequent, while
the managed deployment can recycle individual processes independently of PostgreSQL.
"""
from __future__ import annotations

import json
import hashlib
import os
import re
import threading
import time
from datetime import datetime, timedelta, timezone
from contextlib import contextmanager
from pathlib import Path

from . import config

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"
_SCHEMA_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
_migration_lock = threading.Lock()
_migrated_schemas: set[str] = set()


def database_url() -> str:
    return (
        os.getenv("PREDICTION_DATABASE_URL", "").strip()
        or os.getenv("DATABASE_URL", "").strip()
        or config.PREDICTION_DATABASE_URL
    )


def enabled() -> bool:
    return bool(database_url())


def database_schema() -> str:
    value = os.getenv("PREDICTION_DATABASE_SCHEMA", config.PREDICTION_DATABASE_SCHEMA).strip()
    value = value or "prediction"
    if not _SCHEMA_RE.fullmatch(value):
        raise RuntimeError("PREDICTION_DATABASE_SCHEMA must be a PostgreSQL identifier")
    return value


def _driver():
    try:
        import psycopg  # type: ignore
        from psycopg import sql  # type: ignore
    except ImportError as exc:  # pragma: no cover - production image installs the driver
        raise RuntimeError(
            "PostgreSQL prediction storage is configured but psycopg is not installed"
        ) from exc
    return psycopg, sql


def _raw_connect():
    url = database_url()
    if not url:
        raise RuntimeError("prediction PostgreSQL storage is not configured")
    if not url.startswith(("postgresql://", "postgres://")):
        raise RuntimeError("PREDICTION_DATABASE_URL must use postgresql:// or postgres://")
    psycopg, _ = _driver()
    delays = (0.3, 0.7)
    for attempt in range(len(delays) + 1):
        try:
            return psycopg.connect(url, connect_timeout=5)
        except psycopg.OperationalError:
            if attempt == len(delays):
                raise
            time.sleep(delays[attempt])
    raise AssertionError("unreachable")  # pragma: no cover


def _prepare(raw) -> None:
    _, sql = _driver()
    schema = database_schema()
    raw.execute(sql.SQL("CREATE SCHEMA IF NOT EXISTS {}").format(sql.Identifier(schema)))
    raw.execute(sql.SQL("SET search_path TO {}, public").format(sql.Identifier(schema)))
    raw.execute("SELECT set_config('statement_timeout', '15000', false)")

    if schema in _migrated_schemas:
        raw.commit()
        return
    with _migration_lock:
        if schema not in _migrated_schemas:
            raw.execute(
                "CREATE TABLE IF NOT EXISTS schema_migrations ("
                "version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())"
            )
            applied = {
                row[0] for row in raw.execute("SELECT version FROM schema_migrations").fetchall()
            }
            for path in sorted(MIGRATIONS_DIR.glob("*.sql")):
                if path.name in applied:
                    continue
                raw.execute(path.read_text())
                raw.execute("INSERT INTO schema_migrations(version) VALUES (%s)", (path.name,))
            _migrated_schemas.add(schema)
    raw.commit()


@contextmanager
def connection():
    raw = _raw_connect()
    try:
        _prepare(raw)
        yield raw
        raw.commit()
    except Exception:
        raw.rollback()
        raise
    finally:
        raw.close()


def save_model_version(
    ticker: str,
    timeframe: str,
    horizon: int,
    model_version: str,
    model_blob: bytes,
    record: dict,
    *,
    parent_model_version: str | None = None,
    strategy_version: str | None = None,
) -> None:
    """Insert one immutable candidate. A duplicate version is an error, never an overwrite."""
    with connection() as conn:
        conn.execute(
            "INSERT INTO model_versions("
            "ticker, timeframe, horizon, model_version, parent_model_version, strategy_version, "
            "model_blob, record, created_at) "
            "VALUES (%s, %s, %s, %s, %s, %s, %s, %s::jsonb, now())",
            (
                ticker.upper(), timeframe, horizon, model_version, parent_model_version,
                strategy_version, model_blob, json.dumps(record, default=str),
            ),
        )


def load_model_version(
    ticker: str, timeframe: str, horizon: int, model_version: str
) -> tuple[bytes, dict] | None:
    with connection() as conn:
        row = conn.execute(
            "SELECT model_blob, record FROM model_versions "
            "WHERE ticker=%s AND timeframe=%s AND horizon=%s AND model_version=%s",
            (ticker.upper(), timeframe, horizon, model_version),
        ).fetchone()
    if row is None:
        return None
    return bytes(row[0]), row[1] if isinstance(row[1], dict) else json.loads(row[1])


def active_model_version(ticker: str, timeframe: str, horizon: int) -> str | None:
    with connection() as conn:
        row = conn.execute(
            "SELECT active_model_version FROM model_deployments "
            "WHERE ticker=%s AND timeframe=%s AND horizon=%s",
            (ticker.upper(), timeframe, horizon),
        ).fetchone()
    return None if row is None else row[0]


def load_model(ticker: str, timeframe: str, horizon: int) -> tuple[bytes, dict] | None:
    version = active_model_version(ticker, timeframe, horizon)
    return None if version is None else load_model_version(ticker, timeframe, horizon, version)


def load_record(ticker: str, timeframe: str, horizon: int) -> dict | None:
    stored = load_model(ticker, timeframe, horizon)
    return None if stored is None else stored[1]


def list_model_records(
    ticker: str, timeframe: str | None = None, horizon: int | None = None
) -> list[dict]:
    clauses = ["v.ticker=%s"]
    args: list[object] = [ticker.upper()]
    if timeframe is not None:
        clauses.append("v.timeframe=%s")
        args.append(timeframe)
    if horizon is not None:
        clauses.append("v.horizon=%s")
        args.append(horizon)
    with connection() as conn:
        rows = conn.execute(
            "SELECT v.record, (d.active_model_version = v.model_version) AS active, "
            "EXISTS(SELECT 1 FROM model_promotion_events e WHERE e.ticker=v.ticker "
            "AND e.timeframe=v.timeframe AND e.horizon=v.horizon "
            "AND e.to_model_version=v.model_version) AS was_deployed "
            "FROM model_versions v LEFT JOIN model_deployments d "
            "ON d.ticker=v.ticker AND d.timeframe=v.timeframe AND d.horizon=v.horizon "
            "WHERE " + " AND ".join(clauses) + " ORDER BY v.created_at DESC",
            tuple(args),
        ).fetchall()
    out: list[dict] = []
    for raw, active, was_deployed in rows:
        record = raw if isinstance(raw, dict) else json.loads(raw)
        state = "active" if active else ("archived" if was_deployed else "candidate")
        out.append({**record, "active": bool(active), "wasDeployed": bool(was_deployed),
                    "deploymentState": state})
    return out


def was_model_deployed(ticker: str, timeframe: str, horizon: int, model_version: str) -> bool:
    with connection() as conn:
        row = conn.execute(
            "SELECT EXISTS(SELECT 1 FROM model_promotion_events "
            "WHERE ticker=%s AND timeframe=%s AND horizon=%s AND to_model_version=%s)",
            (ticker.upper(), timeframe, horizon, model_version),
        ).fetchone()
    return bool(row and row[0])


def deploy_model_version(
    ticker: str,
    timeframe: str,
    horizon: int,
    model_version: str,
    *,
    action: str,
    actor_uid: str,
    reason: str,
    evidence: dict | None = None,
) -> dict:
    """Atomically point serving at an immutable version and append its audit event."""
    if action not in {"promote", "rollback"}:
        raise ValueError("action must be promote or rollback")
    ticker = ticker.upper()
    with connection() as conn:
        # A deployment row does not exist before the first promotion, so SELECT ... FOR UPDATE
        # alone cannot serialize two simultaneous bootstrap promotions. This transaction-scoped
        # advisory lock gives every config a row-independent mutex without holding it after commit.
        conn.execute(
            "SELECT pg_advisory_xact_lock(hashtextextended(%s, 0))",
            (f"{ticker}:{timeframe}:{horizon}",),
        )
        target = conn.execute(
            "SELECT model_blob, record FROM model_versions "
            "WHERE ticker=%s AND timeframe=%s AND horizon=%s AND model_version=%s FOR SHARE",
            (ticker, timeframe, horizon, model_version),
        ).fetchone()
        if target is None:
            raise KeyError(model_version)
        current_row = conn.execute(
            "SELECT active_model_version FROM model_deployments "
            "WHERE ticker=%s AND timeframe=%s AND horizon=%s FOR UPDATE",
            (ticker, timeframe, horizon),
        ).fetchone()
        previous = None if current_row is None else current_row[0]
        if previous == model_version:
            record = target[1] if isinstance(target[1], dict) else json.loads(target[1])
            return {
                "changed": False, "fromModelVersion": previous,
                "toModelVersion": model_version, "record": record,
            }
        conn.execute(
            "INSERT INTO model_deployments(ticker, timeframe, horizon, active_model_version, updated_at) "
            "VALUES (%s, %s, %s, %s, now()) "
            "ON CONFLICT (ticker, timeframe, horizon) DO UPDATE SET "
            "active_model_version=excluded.active_model_version, updated_at=now()",
            (ticker, timeframe, horizon, model_version),
        )
        # Compatibility mirror for one rolling restart. It contains only the ACTIVE version; a
        # candidate can never leak into an older process through the legacy table.
        record = target[1] if isinstance(target[1], dict) else json.loads(target[1])
        conn.execute(
            "INSERT INTO models(ticker, timeframe, horizon, model_blob, record, updated_at) "
            "VALUES (%s, %s, %s, %s, %s::jsonb, now()) "
            "ON CONFLICT (ticker, timeframe, horizon) DO UPDATE SET "
            "model_blob=excluded.model_blob, record=excluded.record, updated_at=now()",
            (ticker, timeframe, horizon, bytes(target[0]), json.dumps(record, default=str)),
        )
        conn.execute(
            "INSERT INTO model_promotion_events("
            "ticker, timeframe, horizon, from_model_version, to_model_version, action, "
            "actor_uid, reason, evidence) VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s::jsonb)",
            (
                ticker, timeframe, horizon, previous, model_version, action, actor_uid,
                reason, json.dumps(evidence or {}, default=str),
            ),
        )
    return {
        "changed": True, "fromModelVersion": previous,
        "toModelVersion": model_version, "record": record,
    }


def save_verdict(ticker: str, timeframe: str, horizon: int, record: dict) -> None:
    with connection() as conn:
        conn.execute(
            "INSERT INTO verdicts(ticker, timeframe, horizon, record, updated_at) "
            "VALUES (%s, %s, %s, %s::jsonb, now()) "
            "ON CONFLICT (ticker, timeframe, horizon) DO UPDATE SET "
            "record=excluded.record, updated_at=now()",
            (ticker.upper(), timeframe, horizon, json.dumps(record, default=str)),
        )


def load_verdict(ticker: str, timeframe: str, horizon: int) -> dict | None:
    with connection() as conn:
        row = conn.execute(
            "SELECT record FROM verdicts WHERE ticker=%s AND timeframe=%s AND horizon=%s",
            (ticker.upper(), timeframe, horizon),
        ).fetchone()
    if row is None:
        return None
    return row[0] if isinstance(row[0], dict) else json.loads(row[0])


def list_verdicts() -> list[dict]:
    with connection() as conn:
        rows = conn.execute(
            "SELECT record FROM verdicts ORDER BY ticker, timeframe, horizon"
        ).fetchall()
    return [row[0] if isinstance(row[0], dict) else json.loads(row[0]) for row in rows]


# ---- autonomous candidate trials ---------------------------------------------------------------

def acquire_automation_lease(token: str, lease_seconds: int) -> bool:
    """Acquire the singleton controller lease without waiting for another replica."""
    now = datetime.now(timezone.utc)
    expires = now + timedelta(seconds=lease_seconds)
    with connection() as conn:
        row = conn.execute(
            "UPDATE automation_controller SET lease_token=%s, lease_expires_at=%s, "
            "last_poll_at=%s, last_error=NULL, updated_at=%s "
            "WHERE singleton=TRUE AND (lease_token IS NULL OR lease_expires_at <= %s) "
            "RETURNING singleton",
            (token, expires, now, now, now),
        ).fetchone()
    return row is not None


def release_automation_lease(token: str, error: str | None = None) -> None:
    with connection() as conn:
        conn.execute(
            "UPDATE automation_controller SET lease_token=NULL, lease_expires_at=NULL, "
            "last_error=%s, updated_at=now() WHERE singleton=TRUE AND lease_token=%s",
            ((error or None), token),
        )


def reserve_automation_trial(
    ticker: str,
    timeframe: str,
    horizon: int,
    champion_model_version: str | None,
    trigger_bar: str,
) -> dict | None:
    """Reserve one trial per completed trigger bar. A concurrent duplicate gets no row."""
    with connection() as conn:
        row = conn.execute(
            "INSERT INTO automation_trials("
            "ticker,timeframe,horizon,champion_model_version,trigger_bar,status) "
            "VALUES (%s,%s,%s,%s,%s,'reserved') "
            "ON CONFLICT (ticker,timeframe,horizon,trigger_bar) DO NOTHING "
            "RETURNING id,ticker,timeframe,horizon,champion_model_version,trigger_bar,status,created_at",
            (ticker.upper(), timeframe, horizon, champion_model_version, trigger_bar),
        ).fetchone()
    if row is None:
        return None
    return {
        "id": row[0], "ticker": row[1], "timeframe": row[2], "horizon": row[3],
        "championModelVersion": row[4], "triggerBar": row[5], "status": row[6],
        "createdAt": row[7].isoformat(),
    }


def finish_automation_training(
    trial_id: int,
    *,
    candidate_model_version: str | None = None,
    strategy_version: str | None = None,
    data_through: str | None = None,
    error: str | None = None,
) -> None:
    status = "training-failed" if error else "trained"
    with connection() as conn:
        conn.execute(
            "UPDATE automation_trials SET candidate_model_version=%s,strategy_version=%s,"
            "data_through=%s,status=%s,error=%s,updated_at=now() WHERE id=%s AND status='reserved'",
            (candidate_model_version, strategy_version, data_through, status, error, trial_id),
        )


def mark_automation_trials_evaluating(trial_ids: list[int], started_at: str) -> None:
    if not trial_ids:
        return
    with connection() as conn:
        conn.execute(
            "UPDATE automation_trials SET status='evaluating',evaluation_started_at=%s,"
            "error=NULL,updated_at=now() WHERE id = ANY(%s) AND status='trained'",
            (started_at, trial_ids),
        )


def finish_automation_evaluation(
    trial_id: int,
    *,
    evaluation: dict | None,
    finished_at: str | None,
    error: str | None = None,
) -> None:
    status = "evaluation-failed" if error else "evaluated"
    with connection() as conn:
        conn.execute(
            "UPDATE automation_trials SET status=%s,evaluation_finished_at=%s,"
            "evaluation=%s::jsonb,error=%s,updated_at=now() "
            "WHERE id=%s AND status='evaluating'",
            (status, finished_at, json.dumps(evaluation or {}), error, trial_id),
        )


def automation_trial_baseline(ticker: str, timeframe: str, horizon: int) -> str | None:
    """Newest candidate data cutoff, falling back to the active model's cutoff."""
    with connection() as conn:
        row = conn.execute(
            "SELECT data_through FROM automation_trials WHERE ticker=%s AND timeframe=%s "
            "AND horizon=%s AND data_through IS NOT NULL ORDER BY created_at DESC LIMIT 1",
            (ticker.upper(), timeframe, horizon),
        ).fetchone()
        if row is not None:
            return row[0]
        row = conn.execute(
            "SELECT v.record->>'dataThrough' FROM model_deployments d JOIN model_versions v "
            "ON v.ticker=d.ticker AND v.timeframe=d.timeframe AND v.horizon=d.horizon "
            "AND v.model_version=d.active_model_version "
            "WHERE d.ticker=%s AND d.timeframe=%s AND d.horizon=%s",
            (ticker.upper(), timeframe, horizon),
        ).fetchone()
    return None if row is None else row[0]


def count_automation_trials(ticker: str, timeframe: str, horizon: int) -> int:
    with connection() as conn:
        row = conn.execute(
            "SELECT count(*) FROM automation_trials WHERE ticker=%s AND timeframe=%s AND horizon=%s",
            (ticker.upper(), timeframe, horizon),
        ).fetchone()
    return int(row[0])


def list_automation_trials(*, statuses: tuple[str, ...] | None = None, limit: int = 100) -> list[dict]:
    where = ""
    args: list[object] = []
    if statuses:
        where = "WHERE status = ANY(%s)"
        args.append(list(statuses))
    args.append(max(1, min(int(limit), 500)))
    with connection() as conn:
        rows = conn.execute(
            "SELECT id,ticker,timeframe,horizon,champion_model_version,candidate_model_version,"
            "strategy_version,trigger_bar,data_through,status,evaluation_started_at,"
            "evaluation_finished_at,evaluation,error,created_at,updated_at "
            f"FROM automation_trials {where} ORDER BY created_at DESC LIMIT %s",
            tuple(args),
        ).fetchall()
    out: list[dict] = []
    for row in rows:
        evaluation = row[12] if isinstance(row[12], dict) else json.loads(row[12] or "{}")
        out.append({
            "id": row[0], "ticker": row[1], "timeframe": row[2], "horizon": row[3],
            "championModelVersion": row[4], "candidateModelVersion": row[5],
            "strategyVersion": row[6], "triggerBar": row[7], "dataThrough": row[8],
            "status": row[9],
            "evaluationStartedAt": row[10].isoformat() if row[10] else None,
            "evaluationFinishedAt": row[11].isoformat() if row[11] else None,
            "evaluation": evaluation, "error": row[13],
            "createdAt": row[14].isoformat(), "updatedAt": row[15].isoformat(),
        })
    return out


def automation_controller_status() -> dict:
    with connection() as conn:
        row = conn.execute(
            "SELECT lease_token,lease_expires_at,last_poll_at,last_error,updated_at "
            "FROM automation_controller WHERE singleton=TRUE"
        ).fetchone()
    return {
        "leaseHeld": bool(row and row[0] and row[1] and row[1] > datetime.now(timezone.utc)),
        "leaseExpiresAt": row[1].isoformat() if row and row[1] else None,
        "lastPollAt": row[2].isoformat() if row and row[2] else None,
        "lastError": row[3] if row else None,
        "updatedAt": row[4].isoformat() if row else None,
    }


def set_automation_trial_status(trial_id: int, status: str, error: str | None = None) -> None:
    with connection() as conn:
        conn.execute(
            "UPDATE automation_trials SET status=%s,error=COALESCE(%s,error),updated_at=now() "
            "WHERE id=%s",
            (status, error, trial_id),
        )


def record_shadow_bar(
    trial_id: int,
    *,
    bar_time: str,
    close: float,
    source: str,
    candidate_target: int,
    champion_target: int,
    candidate_probability: float,
    champion_probability: float,
    candidate_cost_bps: float,
    champion_cost_bps: float,
    min_paired_bars: int,
) -> dict:
    """Settle the prior decision and append the new one in one transaction.

    When settlement reaches the requested paired sample, no dangling next prediction is inserted.
    A repeated or older bar is a no-op, which makes restarts idempotent.
    """
    with connection() as conn:
        previous = conn.execute(
            "SELECT bar_time,close,candidate_target,champion_target,candidate_turnover,"
            "champion_turnover,candidate_cost_bps,champion_cost_bps,"
            "candidate_net_return IS NOT NULL "
            "FROM automation_shadow_observations WHERE trial_id=%s "
            "ORDER BY bar_time DESC LIMIT 1 FOR UPDATE",
            (trial_id,),
        ).fetchone()
        if previous is not None and bar_time <= previous[0]:
            return {"appended": False, "settled": False, "complete": False,
                    "reason": "bar-not-newer"}

        settled = False
        previous_candidate_target = 0
        previous_champion_target = 0
        if previous is not None:
            previous_candidate_target = int(previous[2])
            previous_champion_target = int(previous[3])
            if not previous[8]:
                move = float(close) / float(previous[1]) - 1.0
                candidate_return = (
                    previous_candidate_target * move
                    - float(previous[6]) / 10000.0 * float(previous[4])
                )
                champion_return = (
                    previous_champion_target * move
                    - float(previous[7]) / 10000.0 * float(previous[5])
                )
                conn.execute(
                    "UPDATE automation_shadow_observations SET next_bar_time=%s,next_close=%s,"
                    "candidate_net_return=%s,champion_net_return=%s,settled_at=now() "
                    "WHERE trial_id=%s AND bar_time=%s AND candidate_net_return IS NULL",
                    (
                        bar_time, close, candidate_return, champion_return,
                        trial_id, previous[0],
                    ),
                )
                settled = True

        paired = conn.execute(
            "SELECT count(*) FROM automation_shadow_observations "
            "WHERE trial_id=%s AND candidate_net_return IS NOT NULL",
            (trial_id,),
        ).fetchone()[0]
        if int(paired) >= min_paired_bars:
            return {"appended": False, "settled": settled, "complete": True,
                    "pairedBars": int(paired)}

        candidate_turnover = abs(int(candidate_target) - previous_candidate_target)
        champion_turnover = abs(int(champion_target) - previous_champion_target)
        conn.execute(
            "INSERT INTO automation_shadow_observations("
            "trial_id,bar_time,close,source,candidate_target,champion_target,"
            "candidate_probability,champion_probability,candidate_turnover,champion_turnover,"
            "candidate_cost_bps,champion_cost_bps) "
            "VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)",
            (
                trial_id, bar_time, close, source, candidate_target, champion_target,
                candidate_probability, champion_probability, candidate_turnover,
                champion_turnover, candidate_cost_bps, champion_cost_bps,
            ),
        )
    return {"appended": True, "settled": settled, "complete": False,
            "pairedBars": int(paired)}


def load_shadow_observations(trial_id: int) -> list[dict]:
    with connection() as conn:
        rows = conn.execute(
            "SELECT bar_time,close,source,candidate_target,champion_target,"
            "candidate_probability,champion_probability,candidate_turnover,champion_turnover,"
            "candidate_cost_bps,champion_cost_bps,next_bar_time,next_close,"
            "candidate_net_return,champion_net_return,settled_at "
            "FROM automation_shadow_observations WHERE trial_id=%s ORDER BY bar_time",
            (trial_id,),
        ).fetchall()
    return [{
        "barTime": row[0], "close": row[1], "source": row[2],
        "candidateTarget": row[3], "championTarget": row[4],
        "candidateProbability": row[5], "championProbability": row[6],
        "candidateTurnover": row[7], "championTurnover": row[8],
        "candidateCostBps": row[9], "championCostBps": row[10],
        "nextBarTime": row[11], "nextClose": row[12],
        "candidateNetReturn": row[13], "championNetReturn": row[14],
        "settledAt": row[15].isoformat() if row[15] else None,
    } for row in rows]


def save_artifact(name: str, media_type: str, payload: bytes) -> None:
    """Persist a report, run record, or completed evaluation log by logical filename."""
    name = os.path.basename(name)
    with connection() as conn:
        conn.execute(
            "INSERT INTO artifacts(name, media_type, payload, updated_at) "
            "VALUES (%s, %s, %s, now()) ON CONFLICT (name) DO UPDATE SET "
            "media_type=excluded.media_type, payload=excluded.payload, updated_at=now()",
            (name, media_type, payload),
        )


def load_artifact(name: str) -> bytes | None:
    with connection() as conn:
        row = conn.execute(
            "SELECT payload FROM artifacts WHERE name=%s", (os.path.basename(name),)
        ).fetchone()
    return None if row is None else bytes(row[0])


def latest_artifact(prefix: str, suffix: str) -> tuple[str, bytes] | None:
    with connection() as conn:
        row = conn.execute(
            "SELECT name, payload FROM artifacts "
            "WHERE name LIKE %s AND name LIKE %s ORDER BY name DESC LIMIT 1",
            (prefix + "%", "%" + suffix),
        ).fetchone()
    return None if row is None else (row[0], bytes(row[1]))


def save_earnings_payload(
    ticker: str, provider: str, payload: dict, *, vintage_status: str = "unverified"
) -> None:
    """Persist one provider payload with the provenance needed to audit later PEAD runs."""
    raw = json.dumps(payload, sort_keys=True, separators=(",", ":"), default=str).encode()
    quarters = payload.get("quarterlyEarnings") or []
    dates = sorted(
        str(q.get("reportedDate")) for q in quarters
        if isinstance(q, dict) and q.get("reportedDate")
    )
    with connection() as conn:
        conn.execute(
            "INSERT INTO earnings_payloads("
            "ticker, provider, payload, payload_sha256, vintage_status, coverage_start, "
            "coverage_end, event_count, fetched_at, updated_at) "
            "VALUES (%s, %s, %s::jsonb, %s, %s, %s, %s, %s, now(), now()) "
            "ON CONFLICT (ticker, payload_sha256) DO UPDATE SET "
            "provider=excluded.provider, vintage_status=excluded.vintage_status, updated_at=now()",
            (
                ticker.upper(), provider, raw.decode(), hashlib.sha256(raw).hexdigest(),
                vintage_status, dates[0] if dates else None, dates[-1] if dates else None,
                len(quarters),
            ),
        )


def load_earnings_payload(ticker: str) -> dict | None:
    with connection() as conn:
        row = conn.execute(
            "SELECT provider, payload, payload_sha256, vintage_status, fetched_at, updated_at, "
            "coverage_start, coverage_end, event_count "
            "FROM earnings_payloads WHERE ticker=%s ORDER BY updated_at DESC LIMIT 1",
            (ticker.upper(),),
        ).fetchone()
    if row is None:
        return None
    payload = row[1] if isinstance(row[1], dict) else json.loads(row[1])
    return {
        "provider": row[0], "payload": payload, "payloadSha256": row[2],
        "vintageStatus": row[3], "fetchedAt": row[4].isoformat(),
        "lastSeenAt": row[5].isoformat(),
        "coverageStart": row[6].isoformat() if row[6] else None,
        "coverageEnd": row[7].isoformat() if row[7] else None,
        "eventCount": row[8],
    }


def save_estimate_snapshot(
    ticker: str,
    fiscal_date: str,
    expected_report_date: str,
    stage: str,
    provider: str,
    payload: dict,
    *,
    consensus_eps: float,
    captured_at: str,
    estimate_high: float | None = None,
    estimate_low: float | None = None,
    analyst_count: int | None = None,
) -> bool:
    """Insert one immutable pre-release consensus snapshot; identical retries are no-ops."""
    raw = json.dumps(payload, sort_keys=True, separators=(",", ":"), default=str).encode()
    with connection() as conn:
        row = conn.execute(
            "INSERT INTO earnings_estimate_snapshots("
            "ticker, fiscal_date, expected_report_date, stage, provider, payload, payload_sha256, "
            "consensus_eps, estimate_high, estimate_low, analyst_count, captured_at) "
            "VALUES (%s, %s, %s, %s, %s, %s::jsonb, %s, %s, %s, %s, %s, %s) "
            "ON CONFLICT DO NOTHING RETURNING 1",
            (
                ticker.upper(), fiscal_date, expected_report_date, stage, provider,
                raw.decode(), hashlib.sha256(raw).hexdigest(), consensus_eps,
                estimate_high, estimate_low, analyst_count, captured_at,
            ),
        ).fetchone()
    return row is not None


def estimate_snapshot_exists(ticker: str, fiscal_date: str, stage: str) -> bool:
    with connection() as conn:
        row = conn.execute(
            "SELECT 1 FROM earnings_estimate_snapshots "
            "WHERE ticker=%s AND fiscal_date=%s AND stage=%s LIMIT 1",
            (ticker.upper(), fiscal_date, stage),
        ).fetchone()
    return row is not None


def list_estimate_snapshots(ticker: str) -> list[dict]:
    """Return forward snapshots oldest-first for point-in-time matching in the PEAD harness."""
    with connection() as conn:
        rows = conn.execute(
            "SELECT fiscal_date, expected_report_date, stage, provider, payload_sha256, "
            "consensus_eps, estimate_high, estimate_low, analyst_count, captured_at "
            "FROM earnings_estimate_snapshots WHERE ticker=%s "
            "ORDER BY fiscal_date, captured_at",
            (ticker.upper(),),
        ).fetchall()
    return [
        {
            "ticker": ticker.upper(),
            "fiscalDate": row[0].isoformat(),
            "expectedReportDate": row[1].isoformat(),
            "stage": row[2],
            "provider": row[3],
            "payloadSha256": row[4],
            "consensusEPS": float(row[5]),
            "estimateHigh": None if row[6] is None else float(row[6]),
            "estimateLow": None if row[7] is None else float(row[7]),
            "analystCount": row[8],
            "capturedAt": row[9].isoformat(),
        }
        for row in rows
    ]


def save_earnings_text(
    ticker: str, reported_date: str, source: str, text_body: str
) -> None:
    body = text_body.strip()
    digest = hashlib.sha256(body.encode()).hexdigest()
    with connection() as conn:
        conn.execute(
            "INSERT INTO earnings_event_texts("
            "ticker, reported_date, source, text_body, text_sha256, fetched_at, updated_at) "
            "VALUES (%s, %s, %s, %s, %s, now(), now()) "
            "ON CONFLICT (ticker, reported_date, source, text_sha256) DO UPDATE SET updated_at=now()",
            (ticker.upper(), reported_date, source, body, digest),
        )


def load_earnings_text(ticker: str, reported_date: str) -> dict | None:
    with connection() as conn:
        row = conn.execute(
            "SELECT source, text_body, text_sha256, fetched_at FROM earnings_event_texts "
            "WHERE ticker=%s AND reported_date=%s ORDER BY updated_at DESC LIMIT 1",
            (ticker.upper(), reported_date),
        ).fetchone()
    if row is None:
        return None
    return {
        "source": row[0], "text": row[1], "textSha256": row[2],
        "fetchedAt": row[3].isoformat(),
    }
