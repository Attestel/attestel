"""Opt-in PostgreSQL restart coverage for durable LLM research state."""
from __future__ import annotations

import json
import os
import time

import pytest

from app import ablation_verdict, db, personas, store, transcript


@pytest.mark.skipif(not os.getenv("LLM_TEST_DATABASE_URL"), reason="test database not configured")
def test_llm_research_state_survives_database_reopen(monkeypatch, tmp_path):
    url = os.environ["LLM_TEST_DATABASE_URL"]
    schema = f"test_llm_{time.time_ns()}"
    prediction_schema = f"{schema}_prediction"
    monkeypatch.setenv("LLM_DATABASE_URL", url)
    monkeypatch.setenv("LLM_DATABASE_SCHEMA", schema)
    monkeypatch.setenv("PREDICTION_DATABASE_SCHEMA", prediction_schema)
    monkeypatch.setenv("READS_DIR", str(tmp_path / "reads"))
    monkeypatch.setenv("PERSONAS_DIR", str(tmp_path / "personas"))

    try:
        date = store.save_read("NVDA", {"summary": "grounded"}, "qwen", {"trend": "up"})
        store.save_committee("NVDA", {"featureVector": {"consensus": 0.5}})
        transcript.save_transcript_analysis("NVDA", "2026-Q2", {"summary": "call"}, "qwen")
        created = personas.create_persona("alice", "Risk first", "Challenge every assumption")
        psycopg, sql = db._driver()
        verdict = {
            "schema": ablation_verdict.VERDICT_SCHEMA,
            "verdicts": {"C|20d": {"validated": True, "rung": "C", "horizon": "20d"}},
        }
        with psycopg.connect(url, autocommit=True) as conn:
            conn.execute(sql.SQL("CREATE SCHEMA {}").format(sql.Identifier(prediction_schema)))
            conn.execute(sql.SQL(
                "CREATE TABLE {}.artifacts(name TEXT PRIMARY KEY,payload BYTEA NOT NULL)"
            ).format(sql.Identifier(prediction_schema)))
            conn.execute(sql.SQL(
                "INSERT INTO {}.artifacts(name,payload) VALUES(%s,%s)"
            ).format(sql.Identifier(prediction_schema)),
                ("ablation-verdict.json", json.dumps(verdict).encode()))

        db._prepared.discard((url, schema))  # force reopen instead of using process cache
        assert store.list_reads("NVDA")[0]["date"] == date
        assert store.list_committee("NVDA")[0]["featureVector"]["consensus"] == 0.5
        assert transcript.list_transcript_analyses("NVDA")[0]["label"] == "2026-Q2"
        assert personas.get_persona(created["id"], "alice")["name"] == "Risk first"
        assert personas.get_persona(created["id"], "bob") is None
        assert ablation_verdict.load_verdicts()["C|20d"]["validated"] is True
    finally:
        psycopg, sql = db._driver()
        with psycopg.connect(url, autocommit=True) as conn:
            conn.execute(sql.SQL("DROP SCHEMA IF EXISTS {} CASCADE").format(sql.Identifier(schema)))
            conn.execute(sql.SQL("DROP SCHEMA IF EXISTS {} CASCADE").format(
                sql.Identifier(prediction_schema)
            ))
        db._prepared.discard((url, schema))
