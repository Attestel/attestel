"""Read persistence. PostgreSQL is production; files are the no-database fallback/import source.

Fallback layout: {READS_DIR}/{TICKER}/{YYYY-MM-DD}.json (latest write of the day wins)
"""
from __future__ import annotations

import json
import os
from datetime import datetime, timezone

from . import db

READS_DIR = os.getenv("READS_DIR", os.path.join(os.getcwd(), "data", "reads"))


def _dir(ticker: str) -> str:
    d = os.path.join(READS_DIR, ticker.upper())
    os.makedirs(d, exist_ok=True)
    return d


def save_read(ticker: str, structured: dict, model_used: str, regime: dict) -> str:
    """Persist today's snapshot (overwriting any earlier one today). Returns the date key."""
    now = datetime.now(timezone.utc)
    date = now.strftime("%Y-%m-%d")
    record = {
        "date": date,
        "timestamp": now.isoformat(),
        "ticker": ticker.upper(),
        "modelUsed": model_used,
        "structured": structured,
        "regimeSnapshot": {
            "lastClose": regime.get("lastClose"),
            "trend": regime.get("trend"),
            "momentum": regime.get("momentum"),
            "macdState": regime.get("macdState"),
            "keyLevels": regime.get("keyLevels"),
        },
    }
    if db.enabled():
        db.save("read", ticker, date, record)
    else:
        with open(os.path.join(_dir(ticker), f"{date}.json"), "w") as f:
            json.dump(record, f, indent=2)
    return date


def list_reads(ticker: str, limit: int = 10) -> list[dict]:
    if db.enabled():
        return db.list_records("read", ticker, limit)
    d = _dir(ticker)
    files = sorted((f for f in os.listdir(d) if f.endswith(".json")), reverse=True)[:limit]
    out = []
    for name in files:
        try:
            with open(os.path.join(d, name)) as f:
                out.append(json.load(f))
        except (OSError, json.JSONDecodeError):
            continue
    return out


# --- committee snapshots (agent-pipeline runs; kept in a SUBDIR so the daily-read history and
# --- its diff stay unpolluted). One snapshot per ticker per day, latest write of the day wins.

def _committee_dir(ticker: str) -> str:
    d = os.path.join(READS_DIR, ticker.upper(), "committee")
    os.makedirs(d, exist_ok=True)
    return d


def save_committee(ticker: str, committee: dict) -> str:
    """Persist today's committee snapshot. The `featureVector` inside is what the prediction
    service may later consume as candidate features (once enough history exists)."""
    now = datetime.now(timezone.utc)
    date = now.strftime("%Y-%m-%d")
    record = {"date": date, "timestamp": now.isoformat(), **committee}
    if db.enabled():
        db.save("committee", ticker, date, record)
    else:
        with open(os.path.join(_committee_dir(ticker), f"{date}.json"), "w") as f:
            json.dump(record, f, indent=2)
    return date


def list_committee(ticker: str, limit: int = 10) -> list[dict]:
    if db.enabled():
        return db.list_records("committee", ticker, limit)
    d = _committee_dir(ticker)
    files = sorted((f for f in os.listdir(d) if f.endswith(".json")), reverse=True)[:limit]
    out = []
    for name in files:
        try:
            with open(os.path.join(d, name)) as f:
                out.append(json.load(f))
        except (OSError, json.JSONDecodeError):
            continue
    return out


def _pct(a, b):
    try:
        return round((b / a - 1) * 100, 2)
    except (TypeError, ZeroDivisionError):
        return None


def diff_latest(ticker: str) -> dict | None:
    """Diff the two most recent distinct-day snapshots. None if fewer than two exist."""
    reads = list_reads(ticker, limit=2)
    if len(reads) < 2:
        return None
    curr, prev = reads[0], reads[1]
    c, p = curr["regimeSnapshot"], prev["regimeSnapshot"]
    changes: list[str] = []

    if p.get("trend") != c.get("trend"):
        changes.append(f"Trend: {p.get('trend')} → {c.get('trend')}")
    if p.get("momentum") != c.get("momentum"):
        changes.append(f"Momentum: {p.get('momentum')} → {c.get('momentum')}")
    if p.get("macdState") != c.get("macdState"):
        changes.append(f"MACD: {p.get('macdState')} → {c.get('macdState')}")

    lc_p, lc_c = p.get("lastClose"), c.get("lastClose")
    if lc_p is not None and lc_c is not None and lc_p != lc_c:
        pct = _pct(lc_p, lc_c)
        changes.append(f"Last close: {lc_p} → {lc_c}" + (f" ({pct:+}%)" if pct is not None else ""))

    def levels(snap, side):
        return set(map(str, (snap.get("keyLevels", {}) or {}).get(side, []) or []))

    for side in ("support", "resistance"):
        added = levels(c, side) - levels(p, side)
        removed = levels(p, side) - levels(c, side)
        if added:
            changes.append(f"{side.capitalize()} added: {', '.join(sorted(added))}")
        if removed:
            changes.append(f"{side.capitalize()} cleared: {', '.join(sorted(removed))}")

    if prev["structured"].get("summary") != curr["structured"].get("summary"):
        changes.append("Summary narrative changed")

    return {
        "from": prev["date"],
        "to": curr["date"],
        "trendChanged": p.get("trend") != c.get("trend"),
        "changes": changes or ["No material change since last snapshot"],
    }
