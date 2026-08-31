"""Serving the ablation verdict — contract §9.61, Wave 5 Lane 5A's exit criterion.

WHAT THESE PROVE AND WHAT THEY DO NOT. They prove that a verdict written by the ladder reaches the
§7 object the browser reads, and that every ambiguous shape fails CLOSED. They prove nothing about
whether a verdict will ever say `validated: true` — no ablation has run against a real model, so
the only state this repo can currently produce is the absent one, and that is correct.
"""
from __future__ import annotations

import json

import pytest

from app import ablation_verdict as av
from app import analyst


@pytest.fixture()
def verdict_file(tmp_path, monkeypatch):
    path = tmp_path / "verdict.json"
    monkeypatch.setenv(av.VERDICT_PATH_ENV, str(path))
    monkeypatch.setenv(av.RUNG_ENV, "C")
    return path


def write(path, verdicts, schema=av.VERDICT_SCHEMA):
    path.write_text(json.dumps({"schema": schema, "verdicts": verdicts}), encoding="utf-8")


def row(*, validated=True, rung="C", horizon="20d", **extra):
    return {"validated": validated, "rung": rung, "horizon": horizon,
            "verdict": "EDGE" if validated else "NO EDGE", "split": "validation",
            "scored": 120, "directionalAccuracy": 0.58, **extra}


# ── fail closed ──────────────────────────────────────────────────────────────────────────────────

def test_a_missing_file_is_the_ordinary_state_not_an_error(tmp_path, monkeypatch):
    monkeypatch.setenv(av.VERDICT_PATH_ENV, str(tmp_path / "nope.json"))
    monkeypatch.setenv(av.RUNG_ENV, "C")
    assert av.load_verdicts() == {}
    assert av.verdict_for("20d") is None


def test_an_unreadable_or_wrong_schema_file_closes_the_gate(verdict_file):
    verdict_file.write_text("{not json", encoding="utf-8")
    assert av.load_verdicts() == {}
    write(verdict_file, {"C|20d": row()}, schema="something-else@9")
    assert av.load_verdicts() == {}
    assert av.verdict_for("20d") is None


def test_a_database_failure_closes_the_gate(monkeypatch):
    monkeypatch.setattr(av.db, "enabled", lambda: True)
    monkeypatch.setattr(
        av.db, "load_prediction_artifact", lambda _name: (_ for _ in ()).throw(OSError("down"))
    )
    assert av.load_verdicts() == {}


def test_an_unset_rung_serves_nothing(verdict_file, monkeypatch):
    """`ABLATION_RUNG` has no default for the same reason `MODEL_RUNTIME_URL` has none: a defaulted
    rung would match a verdict earned by a different input bundle."""
    write(verdict_file, {"C|20d": row()})
    monkeypatch.delenv(av.RUNG_ENV, raising=False)
    assert av.configured_rung() == ""
    assert av.verdict_for("20d") is None


def test_a_verdict_for_another_rung_or_horizon_is_not_served(verdict_file):
    write(verdict_file, {"D|20d": row(rung="D"), "C|1d": row(horizon="1d")})
    assert av.verdict_for("20d") is None          # only rung D has one, and we are rung C
    assert av.verdict_for("1d") is not None
    assert av.verdict_for("5d") is None


def test_a_row_whose_key_and_body_disagree_about_the_rung_is_refused(verdict_file):
    write(verdict_file, {"C|20d": row(rung="D")})
    assert av.verdict_for("20d") is None


def test_a_non_boolean_validated_is_refused(verdict_file):
    """This is the field the whole gate turns on. A stringly-typed `"false"` is truthy."""
    for value in ("true", "false", 1, 0, None):
        write(verdict_file, {"C|20d": {**row(), "validated": value}})
        assert av.verdict_for("20d") is None, f"{value!r} was accepted"


# ── what is served ───────────────────────────────────────────────────────────────────────────────

def test_only_the_three_contract_keys_reach_the_client(verdict_file):
    """§9.61 binding 1's shape. The evidence in the file is for a human reading the file; a client
    that could see `verdict: "EDGE"` would eventually match on it instead of on `validated`, which
    is the ladder's own bar reimplemented in JavaScript."""
    write(verdict_file, {"C|20d": row(cutoffFingerprint="120@abcd", runtimes={"qwen3-14b": 120})})
    served = av.verdict_for("20d")
    assert served == {"validated": True, "rung": "C", "horizon": "20d"}


def test_validated_false_is_served_because_it_is_a_real_answer(verdict_file):
    """§9.61 binding 4. The ablation ran and did not license the direction. Both withhold the word;
    only one of them is evidence, and swallowing this one would destroy that distinction."""
    write(verdict_file, {"C|20d": row(validated=False)})
    assert av.verdict_for("20d") == {"validated": False, "rung": "C", "horizon": "20d"}


# ── the ladder's file and this reader agree ──────────────────────────────────────────────────────

def test_the_schema_string_matches_what_the_ladder_writes():
    """Both sides state it; this is the assertion that keeps them stated the same. `ablation.py`
    lives in another service with no shared package, so the constant is duplicated on purpose and
    the duplication is what needs a test."""
    assert av.VERDICT_SCHEMA == "ablation-verdict@1"
    assert av.DEFAULT_VERDICT_PATH.replace("\\", "/").endswith("data/eval/ablation/verdict.json")


# ── the §7 object carries it ─────────────────────────────────────────────────────────────────────

def _run(monkeypatch, ctx=None):
    """One analyst run with an injected model, so no generation happens and nothing here is a
    statement about model behaviour."""
    call = lambda messages, mode=None: ("{}", "stub:offline")   # noqa: E731
    return analyst.run_analyst("NVDA", ctx or {"ticker": "NVDA"}, call,
                               lambda raw: None, horizons=["20d"])


def test_the_seven_object_carries_the_verdict_when_one_exists(verdict_file, monkeypatch, tmp_path):
    monkeypatch.setenv("READS_DIR", str(tmp_path))
    write(verdict_file, {"C|20d": row(validated=False)})
    envelope = _run(monkeypatch)
    thesis = envelope["theses"]["20d"]
    assert thesis["ablationVerdict"] == {"validated": False, "rung": "C", "horizon": "20d"}


def test_the_key_is_omitted_rather_than_nulled_when_no_verdict_exists(monkeypatch, tmp_path):
    """A null would blur §9.61 binding 4's two states — 'ran and did not license it' versus 'never
    ran'. Absent is the ordinary state and must stay renderable."""
    monkeypatch.setenv("READS_DIR", str(tmp_path))
    monkeypatch.setenv(av.VERDICT_PATH_ENV, str(tmp_path / "absent.json"))
    monkeypatch.delenv(av.RUNG_ENV, raising=False)
    envelope = _run(monkeypatch)
    assert "ablationVerdict" not in envelope["theses"]["20d"]


def test_the_verdict_file_is_read_once_per_run_not_once_per_horizon(monkeypatch, tmp_path):
    """Four reads would be four chances for the file to change mid-run and for two theses in one
    envelope to disagree about whether the ladder has run."""
    monkeypatch.setenv("READS_DIR", str(tmp_path))
    reads = []
    monkeypatch.setattr(av, "load_verdicts", lambda path=None: reads.append(1) or {})
    analyst.run_analyst("NVDA", {"ticker": "NVDA"}, lambda m, mode=None: ("{}", "stub:offline"),
                        lambda raw: None, horizons=None)
    assert reads == [1]


def test_the_envelope_says_which_runtime_produced_it(monkeypatch, tmp_path):
    """Wave 5C's clause, at the point it matters most: a verdict-bearing envelope produced by
    `stub:offline` is a fixture, and the field that says so travels with it."""
    monkeypatch.setenv("READS_DIR", str(tmp_path))
    envelope = _run(monkeypatch)
    assert envelope["runtime"]["served"] == "stub:offline"
    assert envelope["runtime"]["isStub"] is True
    # And it is BESIDE identity, not inside it (§9.40 pins seven keys).
    assert "runtime" not in envelope["identity"]
    assert len(envelope["identity"]) == 7
