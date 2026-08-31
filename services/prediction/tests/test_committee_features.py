"""committee_features.py — history gate, no look-ahead, fail-soft neutrality.

Run:  cd services/prediction && python -m pytest -q tests/test_committee_features.py
"""
import json
import os
import time

import pandas as pd
import pytest

import app.committee_features as cf
from app.committee_features import (
    COMMITTEE_FEATURES,
    build_committee_features,
    load_committee_history,
)


def _idx(n=10, end="2026-07-10"):
    return pd.Index([str(d.date()) for d in pd.bdate_range(end=end, periods=n)])


def _snap(date, consensus=0.5, agreement=1.0, conf=0.7):
    return {"date": date, "featureVector": {
        "consensus": consensus, "agreement": agreement, "meanConfidence": conf}}


def test_neutral_when_no_snapshots():
    out = build_committee_features(_idx(), None)
    assert list(out.columns) == COMMITTEE_FEATURES
    assert (out == 0.0).all().all()


def test_history_gate_keeps_features_neutral(monkeypatch):
    """Below MIN_COMMITTEE_SNAPSHOTS real days -> neutral, even though snapshots exist. LLM stances
    cannot be backfilled, so a thin history must not influence the backtest."""
    monkeypatch.setattr(cf, "MIN_COMMITTEE_SNAPSHOTS", 5)
    snaps = [_snap(f"2026-07-0{i}") for i in range(1, 4)]  # 3 < 5
    out = build_committee_features(_idx(), snaps)
    assert (out == 0.0).all().all()


def test_no_look_ahead(monkeypatch):
    """A bar dated d sees only snapshots dated STRICTLY BEFORE d."""
    monkeypatch.setattr(cf, "MIN_COMMITTEE_SNAPSHOTS", 1)
    idx = pd.Index(["2026-07-06", "2026-07-07", "2026-07-08"])
    snaps = [_snap("2026-07-07", consensus=0.9)]
    out = build_committee_features(idx, snaps)
    assert out.loc["2026-07-06", "comm_consensus"] == 0.0  # before the snapshot -> neutral
    assert out.loc["2026-07-07", "comm_consensus"] == 0.0  # SAME day -> not public yet
    assert out.loc["2026-07-08", "comm_consensus"] == 0.9  # strictly after -> visible


def test_values_are_clipped(monkeypatch):
    monkeypatch.setattr(cf, "MIN_COMMITTEE_SNAPSHOTS", 1)
    idx = pd.Index(["2026-07-08"])
    snaps = [_snap("2026-07-07", consensus=5.0, agreement=-2.0, conf="junk")]
    out = build_committee_features(idx, snaps)
    assert out.iloc[0]["comm_consensus"] == 1.0
    assert out.iloc[0]["comm_agreement"] == 0.0
    assert out.iloc[0]["comm_conf"] == 0.0


def test_load_is_fail_soft_and_sorted(tmp_path):
    d = tmp_path / "NVDA" / "committee"
    d.mkdir(parents=True)
    for date in ("2026-07-02", "2026-07-01"):
        (d / f"{date}.json").write_text(json.dumps(_snap(date)))
    (d / "corrupt.json").write_text("{not json")
    snaps = load_committee_history("NVDA", reads_dir=str(tmp_path))
    assert [s["date"] for s in snaps] == ["2026-07-01", "2026-07-02"]  # oldest first
    assert load_committee_history("NVDA", reads_dir="/nonexistent") == []


@pytest.mark.skipif(
    not os.getenv("PREDICTION_TEST_DATABASE_URL"), reason="test database not configured"
)
def test_load_committee_history_from_llm_postgres(monkeypatch):
    url = os.environ["PREDICTION_TEST_DATABASE_URL"]
    schema = f"test_llm_features_{time.time_ns()}"
    import psycopg
    from psycopg import sql

    with psycopg.connect(url, autocommit=True) as conn:
        conn.execute(sql.SQL("CREATE SCHEMA {}").format(sql.Identifier(schema)))
        conn.execute(sql.SQL(
            "CREATE TABLE {}.snapshots(kind TEXT,ticker TEXT,snapshot_key TEXT,user_id TEXT,data JSONB)"
        ).format(sql.Identifier(schema)))
        for date in ("2026-07-02", "2026-07-01"):
            conn.execute(
                sql.SQL(
                    "INSERT INTO {}.snapshots VALUES('committee','NVDA',%s,'',%s::jsonb)"
                ).format(sql.Identifier(schema)),
                (date, json.dumps(_snap(date))),
            )
    monkeypatch.setenv("PREDICTION_DATABASE_URL", url)
    monkeypatch.setenv("LLM_DATABASE_SCHEMA", schema)
    try:
        snapshots = load_committee_history("NVDA")
        assert [row["date"] for row in snapshots] == ["2026-07-01", "2026-07-02"]
    finally:
        with psycopg.connect(url, autocommit=True) as conn:
            conn.execute(sql.SQL("DROP SCHEMA {} CASCADE").format(sql.Identifier(schema)))
