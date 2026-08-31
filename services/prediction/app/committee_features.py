"""Committee features — the HYBRID half of the agent pipeline.

The llm service persists one analyst-committee snapshot per ticker per day; each snapshot carries a
numeric `featureVector` (signed stance*confidence per analyst, deterministic consensus, agreement).
This module turns that history into CANDIDATE model features, mirroring context.py exactly:

  * FAILS SOFT to neutral 0.0 columns (zero-keys / zero-network invariant intact);
  * NO LOOK-AHEAD: a bar dated d sees only snapshots dated STRICTLY BEFORE d (a snapshot is
    written during day d, so it is public only after d), and everything is lagged one extra bar in
    build_dataset like every other feature;
  * HISTORY GATE: LLM stances cannot be backfilled — generating "historical" committee runs today
    would bake hindsight into the backtest. So the features stay neutral until at least
    MIN_COMMITTEE_SNAPSHOTS real snapshot days exist. Until then the walk-forward backtest — and
    therefore the signal — is exactly what it was without the committee. Neutral constants produce
    no LightGBM splits.

This is the honest version of ai-hedge-fund's "agents feed the portfolio manager": the agents feed
the FEATURE MATRIX, and the same walk-forward gate decides whether they add anything.
"""
from __future__ import annotations

import bisect
import json
import os
import re

import numpy as np
import pandas as pd

from .config import COMMITTEE_READS_DIR, MIN_COMMITTEE_SNAPSHOTS

COMMITTEE_FEATURES = [
    "comm_consensus",  # deterministic weighted stance vote, -1..1 (0 = neutral / unknown)
    "comm_agreement",  # fraction of analysts sharing the modal stance, 0..1 (0 = unknown)
    "comm_conf",       # mean analyst confidence, 0..1 (0 = unknown)
]


def load_committee_history(ticker: str, reads_dir: str | None = None) -> list[dict]:
    """All persisted committee snapshots for a ticker, OLDEST first: [{date, featureVector}, ...].
    Fail-soft: missing dir / unreadable files -> []. Never writes; the llm service owns the dir."""
    if reads_dir is None:
        database_url = (
            os.getenv("LLM_DATABASE_URL", "").strip()
            or os.getenv("DATABASE_URL", "").strip()
            or os.getenv("PREDICTION_DATABASE_URL", "").strip()
        )
        schema = os.getenv("LLM_DATABASE_SCHEMA", "llm").strip() or "llm"
        if database_url and re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", schema):
            try:
                import psycopg  # type: ignore
                from psycopg import sql  # type: ignore
                with psycopg.connect(database_url, connect_timeout=5) as conn:
                    rows = conn.execute(
                        sql.SQL(
                            "SELECT data FROM {}.snapshots "
                            "WHERE kind='committee' AND ticker=%s AND user_id='' "
                            "ORDER BY snapshot_key"
                        ).format(sql.Identifier(schema)),
                        (ticker.upper(),),
                    ).fetchall()
                out = []
                for row in rows:
                    rec = row[0] if isinstance(row[0], dict) else json.loads(row[0])
                    fv, date = rec.get("featureVector"), rec.get("date")
                    if isinstance(fv, dict) and isinstance(date, str):
                        out.append({"date": date[:10], "featureVector": fv})
                return out
            except Exception:
                return []
    d = os.path.join(reads_dir or COMMITTEE_READS_DIR, ticker.upper(), "committee")
    try:
        names = sorted(f for f in os.listdir(d) if f.endswith(".json"))
    except OSError:
        return []
    out = []
    for name in names:
        try:
            with open(os.path.join(d, name)) as f:
                rec = json.load(f)
        except (OSError, json.JSONDecodeError):
            continue
        fv = rec.get("featureVector")
        date = rec.get("date") or name[:-5]
        if isinstance(fv, dict) and isinstance(date, str):
            out.append({"date": date[:10], "featureVector": fv})
    return out


def _num(v) -> float:
    return float(v) if isinstance(v, (int, float)) and np.isfinite(v) else 0.0


def build_committee_features(index: pd.Index, snapshots: list[dict] | None) -> pd.DataFrame:
    """COMMITTEE_FEATURES aligned to the bar index. For bar date d, only snapshots dated STRICTLY
    BEFORE d are visible (public by then). Below the history gate, or with no snapshots at all,
    every column is neutral 0.0."""
    out = pd.DataFrame(index=index)
    snapshots = snapshots or []
    if len(snapshots) < MIN_COMMITTEE_SNAPSHOTS:
        for c in COMMITTEE_FEATURES:
            out[c] = 0.0
        return out[COMMITTEE_FEATURES]

    dates = [s["date"] for s in snapshots]  # oldest-first (load sorts)
    cons = np.zeros(len(index))
    agree = np.zeros(len(index))
    conf = np.zeros(len(index))
    for i, idx_value in enumerate(index):
        d = str(idx_value)[:10]
        j = bisect.bisect_left(dates, d)  # snapshots with date < d
        if j == 0:
            continue  # no PRIOR snapshot -> neutral
        fv = snapshots[j - 1]["featureVector"]
        cons[i] = float(np.clip(_num(fv.get("consensus")), -1.0, 1.0))
        agree[i] = float(np.clip(_num(fv.get("agreement")), 0.0, 1.0))
        conf[i] = float(np.clip(_num(fv.get("meanConfidence")), 0.0, 1.0))
    out["comm_consensus"] = cons
    out["comm_agreement"] = agree
    out["comm_conf"] = conf
    return out[COMMITTEE_FEATURES]
