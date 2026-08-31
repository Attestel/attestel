"""Immutable model registry with an explicit active deployment pointer.

Training writes a candidate version and never changes what ``/predict`` serves. Promotion or
rollback is the only operation that moves the active pointer. PostgreSQL is the durable production
path; the file layout mirrors it for zero-configuration local development and deterministic tests.

File layout::

    {MODELS_DIR}/{TICKER}_{tf}_{horizon}/
        active.json
        promotions.json
        versions/{modelVersion}/model.pkl
        versions/{modelVersion}/record.json

The pre-registry ``model.pkl`` + ``record.json`` pair remains a readable legacy active model and is
kept as a compatibility mirror after a file-mode promotion.
"""
from __future__ import annotations

import json
import os
import pickle
import re
from datetime import datetime, timezone
from io import BytesIO
from uuid import uuid4

from . import db
from .config import MODELS_DIR
from .verdicts import expected_strategy_version

_VERSION_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")


def _key(ticker: str, timeframe: str, horizon: int) -> str:
    return f"{ticker.upper()}_{timeframe}_{horizon}"


def _config_dir(ticker: str, timeframe: str, horizon: int, *, create: bool = False) -> str:
    path = os.path.join(MODELS_DIR, _key(ticker, timeframe, horizon))
    if create:
        os.makedirs(path, exist_ok=True)
    return path


def _version_dir(
    ticker: str, timeframe: str, horizon: int, model_version: str, *, create: bool = False
) -> str:
    if not _VERSION_RE.fullmatch(model_version):
        raise ValueError("invalid model version")
    path = os.path.join(
        _config_dir(ticker, timeframe, horizon, create=create), "versions", model_version
    )
    if create:
        os.makedirs(path, exist_ok=False)
    return path


def _read_json(path: str) -> dict | list | None:
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, json.JSONDecodeError):
        return None


def _write_json_atomic(path: str, value: dict | list) -> None:
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(value, f, indent=2)
    os.replace(tmp, path)


def _new_model_version(now: datetime) -> str:
    return f"m{now.strftime('%Y%m%dT%H%M%S')}-{uuid4().hex[:12]}"


def _ensure_legacy_file_is_versioned(ticker: str, timeframe: str, horizon: int) -> None:
    """Import the pre-registry file pair before a promotion can overwrite its compatibility copy."""
    if _active_file_version(ticker, timeframe, horizon):
        return
    base = _config_dir(ticker, timeframe, horizon)
    record = _read_json(os.path.join(base, "record.json"))
    if not isinstance(record, dict):
        return
    version = record.get("modelVersion")
    if not isinstance(version, str) or not _VERSION_RE.fullmatch(version):
        return
    try:
        with open(os.path.join(base, "model.pkl"), "rb") as f:
            payload = f.read()
    except OSError:
        return
    target = _version_dir(ticker, timeframe, horizon, version)
    if not os.path.isdir(target):
        os.makedirs(target, exist_ok=False)
        with open(os.path.join(target, "model.pkl"), "wb") as f:
            f.write(payload)
        _write_json_atomic(os.path.join(target, "record.json"), record)
    _write_json_atomic(os.path.join(base, "active.json"), {"modelVersion": version})


def save_candidate(
    ticker: str,
    timeframe: str,
    horizon: int,
    model: object,
    report: dict,
    trained_on_synthetic: bool,
    data_through,
    features: list[str] | None = None,
    calibrator: object | None = None,
    data_policy: str | None = None,
) -> dict:
    """Persist one immutable candidate and return its inspectable record."""
    ticker = ticker.upper()
    now = datetime.now(timezone.utc)
    model_version = _new_model_version(now)
    if not db.enabled():
        _ensure_legacy_file_is_versioned(ticker, timeframe, horizon)
    active = load_record(ticker, timeframe, horizon)
    parent = (active or {}).get("modelVersion")
    strategy_version = expected_strategy_version(report)
    record = {
        "ticker": ticker,
        "timeframe": timeframe,
        "horizon": horizon,
        "modelVersion": model_version,
        "parentModelVersion": parent,
        "strategyVersion": strategy_version,
        "trainedAt": now.isoformat(),
        "dataThrough": str(data_through),
        "trainedOnSynthetic": bool(trained_on_synthetic),
        "features": list(features) if features else None,
        "dataPolicy": data_policy,
        "report": report,
    }
    payload = pickle.dumps({"model": model, "calibrator": calibrator})
    if db.enabled():
        db.save_model_version(
            ticker, timeframe, horizon, model_version, payload, record,
            parent_model_version=parent, strategy_version=strategy_version,
        )
        return record

    d = _version_dir(ticker, timeframe, horizon, model_version, create=True)
    with open(os.path.join(d, "model.pkl"), "wb") as f:
        f.write(payload)
    _write_json_atomic(os.path.join(d, "record.json"), record)
    return record


def _active_file_version(ticker: str, timeframe: str, horizon: int) -> str | None:
    pointer = _read_json(os.path.join(_config_dir(ticker, timeframe, horizon), "active.json"))
    if not isinstance(pointer, dict):
        return None
    version = pointer.get("modelVersion")
    return version if isinstance(version, str) and _VERSION_RE.fullmatch(version) else None


def active_model_version(ticker: str, timeframe: str, horizon: int) -> str | None:
    if db.enabled():
        return db.active_model_version(ticker, timeframe, horizon)
    version = _active_file_version(ticker, timeframe, horizon)
    if version:
        return version
    legacy = _read_json(os.path.join(_config_dir(ticker, timeframe, horizon), "record.json"))
    if isinstance(legacy, dict) and isinstance(legacy.get("modelVersion"), str):
        return legacy["modelVersion"]
    return None


def load_version_record(
    ticker: str, timeframe: str, horizon: int, model_version: str
) -> dict | None:
    if db.enabled():
        stored = db.load_model_version(ticker, timeframe, horizon, model_version)
        return None if stored is None else stored[1]
    try:
        d = _version_dir(ticker, timeframe, horizon, model_version)
    except ValueError:
        return None
    record = _read_json(os.path.join(d, "record.json"))
    if isinstance(record, dict):
        return record
    legacy = _read_json(os.path.join(_config_dir(ticker, timeframe, horizon), "record.json"))
    if isinstance(legacy, dict) and legacy.get("modelVersion") == model_version:
        return legacy
    return None


def load_version_model(ticker: str, timeframe: str, horizon: int, model_version: str):
    """Load one immutable version without changing or consulting the active pointer."""
    if db.enabled():
        stored = db.load_model_version(ticker, timeframe, horizon, model_version)
        if stored is None:
            return None, None, None
        payload, record = stored
        return _unpickle(payload, record)
    try:
        d = _version_dir(ticker, timeframe, horizon, model_version)
    except ValueError:
        return None, None, None
    record = load_version_record(ticker, timeframe, horizon, model_version)
    try:
        with open(os.path.join(d, "model.pkl"), "rb") as f:
            payload = f.read()
    except OSError:
        return None, None, record
    return _unpickle(payload, record)


def load_record(ticker: str, timeframe: str, horizon: int) -> dict | None:
    if db.enabled():
        record = db.load_record(ticker, timeframe, horizon)
    else:
        version = _active_file_version(ticker, timeframe, horizon)
        if version:
            record = load_version_record(ticker, timeframe, horizon, version)
        else:
            legacy = _read_json(os.path.join(_config_dir(ticker, timeframe, horizon), "record.json"))
            record = legacy if isinstance(legacy, dict) else None
    if record is None:
        return None
    return {**record, "active": True, "deploymentState": "active"}


def _unpickle(payload: bytes, record: dict | None):
    try:
        obj = pickle.load(BytesIO(payload))
    except (OSError, pickle.UnpicklingError, EOFError):
        return None, None, record
    if isinstance(obj, dict) and "model" in obj:
        return obj["model"], obj.get("calibrator"), record
    return obj, None, record


def load_model(ticker: str, timeframe: str, horizon: int):
    """Return the active ``(model, calibrator, record)``; candidates are never served."""
    if db.enabled():
        stored = db.load_model(ticker, timeframe, horizon)
        if stored is None:
            return None, None, None
        payload, record = stored
        return _unpickle(payload, {**record, "active": True, "deploymentState": "active"})

    version = _active_file_version(ticker, timeframe, horizon)
    if version:
        try:
            d = _version_dir(ticker, timeframe, horizon, version)
        except ValueError:
            return None, None, None
        record = load_version_record(ticker, timeframe, horizon, version)
        path = os.path.join(d, "model.pkl")
    else:
        record = load_record(ticker, timeframe, horizon)
        path = os.path.join(_config_dir(ticker, timeframe, horizon), "model.pkl")
    try:
        with open(path, "rb") as f:
            payload = f.read()
    except OSError:
        return None, None, record
    return _unpickle(payload, record)


def list_model_records(
    ticker: str, timeframe: str | None = None, horizon: int | None = None
) -> list[dict]:
    if db.enabled():
        return db.list_model_records(ticker, timeframe, horizon)
    out: list[dict] = []
    prefix = ticker.upper() + "_"
    try:
        names = os.listdir(MODELS_DIR)
    except OSError:
        return []
    for name in names:
        if not name.startswith(prefix):
            continue
        base = os.path.join(MODELS_DIR, name)
        versions = os.path.join(base, "versions")
        try:
            version_names = os.listdir(versions)
        except OSError:
            version_names = []
        for version in version_names:
            record = _read_json(os.path.join(versions, version, "record.json"))
            if not isinstance(record, dict):
                continue
            if timeframe is not None and record.get("timeframe") != timeframe:
                continue
            if horizon is not None and record.get("horizon") != horizon:
                continue
            active = _active_file_version(
                record.get("ticker", ticker), record.get("timeframe", ""), record.get("horizon", 0)
            ) == record.get("modelVersion")
            deployed = was_model_deployed(
                record.get("ticker", ticker), record.get("timeframe", ""),
                record.get("horizon", 0), record.get("modelVersion", ""),
            )
            state = "active" if active else ("archived" if deployed else "candidate")
            out.append({**record, "active": active, "wasDeployed": deployed,
                        "deploymentState": state})
        legacy = _read_json(os.path.join(base, "record.json"))
        if isinstance(legacy, dict):
            if timeframe is not None and legacy.get("timeframe") != timeframe:
                continue
            if horizon is not None and legacy.get("horizon") != horizon:
                continue
            if not any(r.get("modelVersion") == legacy.get("modelVersion") for r in out):
                active = _active_file_version(
                    legacy.get("ticker", ticker), legacy.get("timeframe", ""), legacy.get("horizon", 0)
                ) is None
                out.append({**legacy, "active": active, "wasDeployed": True,
                            "deploymentState": "active" if active else "archived"})
    return sorted(out, key=lambda r: str(r.get("trainedAt") or ""), reverse=True)


def was_model_deployed(ticker: str, timeframe: str, horizon: int, model_version: str) -> bool:
    if db.enabled():
        return db.was_model_deployed(ticker, timeframe, horizon, model_version)
    if active_model_version(ticker, timeframe, horizon) == model_version:
        return True
    events = _read_json(os.path.join(
        _config_dir(ticker, timeframe, horizon), "promotions.json"
    ))
    return bool(isinstance(events, list) and any(
        event.get("fromModelVersion") == model_version
        or event.get("toModelVersion") == model_version
        for event in events if isinstance(event, dict)
    ))


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
    if action not in {"promote", "rollback"}:
        raise ValueError("action must be promote or rollback")
    if db.enabled():
        return db.deploy_model_version(
            ticker, timeframe, horizon, model_version, action=action,
            actor_uid=actor_uid, reason=reason, evidence=evidence,
        )

    target = load_version_record(ticker, timeframe, horizon, model_version)
    if target is None:
        raise KeyError(model_version)
    d = _version_dir(ticker, timeframe, horizon, model_version)
    try:
        with open(os.path.join(d, "model.pkl"), "rb") as f:
            payload = f.read()
    except OSError as exc:
        raise KeyError(model_version) from exc
    previous = active_model_version(ticker, timeframe, horizon)
    if previous == model_version:
        return {
            "changed": False, "fromModelVersion": previous,
            "toModelVersion": model_version, "record": target,
        }
    base = _config_dir(ticker, timeframe, horizon, create=True)
    _write_json_atomic(os.path.join(base, "active.json"), {"modelVersion": model_version})
    with open(os.path.join(base, "model.pkl.tmp"), "wb") as f:
        f.write(payload)
    os.replace(os.path.join(base, "model.pkl.tmp"), os.path.join(base, "model.pkl"))
    _write_json_atomic(os.path.join(base, "record.json"), target)
    promotions_path = os.path.join(base, "promotions.json")
    existing = _read_json(promotions_path)
    events = existing if isinstance(existing, list) else []
    events.append({
        "fromModelVersion": previous,
        "toModelVersion": model_version,
        "action": action,
        "actorUid": actor_uid,
        "reason": reason,
        "evidence": evidence or {},
        "createdAt": datetime.now(timezone.utc).isoformat(),
    })
    _write_json_atomic(promotions_path, events)
    return {
        "changed": True, "fromModelVersion": previous,
        "toModelVersion": model_version, "record": target,
    }
