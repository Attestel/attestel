"""The prod smoke's GATE and its arithmetic — Wave 5 Lane 5C.

These tests never reach a model. They assert the two things that can be asserted without one:

  * the proof gate REFUSES on every path where the model was not proven, and prints nothing; and
  * given a set of envelopes, the seven measures compute what they claim to compute.

The measurements themselves are `unmeasured` until the smoke runs on the prod box. That is the
point of the wave, and a test file that pretended otherwise would be the fixture-matching-the-
contract's-own-example failure for the second time.
"""
from __future__ import annotations

import json

import pytest

from app import llm, runtime, smoke


@pytest.fixture()
def reads_dir(tmp_path, monkeypatch):
    monkeypatch.setenv("READS_DIR", str(tmp_path))
    return tmp_path


RT = runtime.Runtime(name="prod-a", kind="openai", base="http://box:8000/v1", model="qwen3-14b")


def _served(model: str):
    """A transport stand-in returning `(raw, model_used, meta)` like `llm.call_model_meta`."""
    return lambda messages: ('{"ok": true}', model, {"transport": "openai"})


# ── the gate ─────────────────────────────────────────────────────────────────────────────────────

def test_no_runtime_refuses_before_any_generation(monkeypatch, reads_dir):
    monkeypatch.setattr(llm, "RUNTIME", None)
    monkeypatch.setattr(llm, "RUNTIME_REFUSALS", ["MODEL_RUNTIME_URL is unset"])
    called = []
    proof, code = smoke.prove_real_model(call_model=lambda m: called.append(m))
    assert code == smoke.EXIT_NO_RUNTIME
    assert called == [], "the probe must not run when there is no runtime to probe"
    assert proof["checks"][0]["ok"] is False


def test_a_stub_answer_refuses_and_says_why(monkeypatch, reads_dir):
    monkeypatch.setattr(llm, "RUNTIME", RT)
    proof, code = smoke.prove_real_model(call_model=_served("stub:offline"))
    assert code == smoke.EXIT_STUB_SERVED
    detail = proof["checks"][-1]["detail"]
    assert "describe the fixture" in detail


def test_stub_quota_is_refused_too(monkeypatch, reads_dir):
    """`stub:quota` is a fixture as much as `stub:offline`. A gate that only knew the one label
    would license a claim about the model from the other."""
    monkeypatch.setattr(llm, "RUNTIME", RT)
    _, code = smoke.prove_real_model(call_model=_served("stub:quota"))
    assert code == smoke.EXIT_STUB_SERVED


def test_a_different_model_than_configured_is_refused(monkeypatch, reads_dir):
    monkeypatch.setattr(llm, "RUNTIME", RT)
    proof, code = smoke.prove_real_model(call_model=_served("llama3.1:8b"))
    assert code == smoke.EXIT_IDENTITY_MISMATCH
    assert "different experiment" in proof["checks"][-1]["detail"]


def test_the_happy_path_proves_model_identity_and_lease(monkeypatch, reads_dir):
    monkeypatch.setattr(llm, "RUNTIME", RT)
    proof, code = smoke.prove_real_model(call_model=_served("qwen3-14b"))
    assert code == smoke.EXIT_OK
    names = [c["check"] for c in proof["checks"]]
    assert names == ["runtime_configured", "real_model_served", "identity_matches_config",
                     "lease_taken_and_released"]
    assert all(c["ok"] for c in proof["checks"])
    # The lease was OBSERVED at the cross-process marker, not assumed from the `with` returning.
    assert proof["leaseMarkerState"] == "released"
    assert json.loads((reads_dir / ".model.lock.interactive").read_text())["state"] == "released"


def test_an_unreleased_lease_is_refused(monkeypatch, reads_dir):
    """§9.21's guarantee is cross-process. If the marker does not say `released`, concurrent
    generation was never actually excluded on this box and the run proves nothing about it."""
    monkeypatch.setattr(llm, "RUNTIME", RT)
    monkeypatch.setattr(smoke.lease, "interactive_lease_is_held", lambda: True)
    _, code = smoke.prove_real_model(call_model=_served("qwen3-14b"))
    assert code == smoke.EXIT_LEASE_UNPROVEN


def test_main_prints_no_measurement_when_the_gate_refuses(monkeypatch, reads_dir, capsys):
    monkeypatch.setattr(llm, "RUNTIME", None)
    monkeypatch.setattr(llm, "RUNTIME_REFUSALS", ["unset"])
    assert smoke.main() == smoke.EXIT_NO_RUNTIME
    out = capsys.readouterr().out
    assert "REFUSED" in out
    assert "Measurements" not in out
    assert "unmeasured" not in out, "even the word must not appear — nothing was measured at all"


# ── the seven measures ───────────────────────────────────────────────────────────────────────────

def _envelope(*, branches=("model",), directions=("bullish",), suppressions=(),
              outcomes=None, warnings=()):
    theses = {}
    for i, (branch, direction) in enumerate(zip(branches, directions)):
        theses[f"h{i}"] = {"direction": direction, "directionBranch": branch}
    return {
        "theses": theses,
        "proseSuppressions": [dict(s) for s in suppressions],
        "stageOutcomes": outcomes or {},
        "warnings": list(warnings),
        "runtime": {"served": "prod-a"},
    }


def test_branch_distribution_names_the_branches_no_model_ever_reached():
    envs = [_envelope(branches=("model", "model"), directions=("bullish", "bearish"))]
    out = smoke.branch_distribution(envs)
    assert out["counts"]["model"] == 2
    assert set(out["branchesNeverReached"]) == set(runtime_branches() - {"model"})
    assert "have not fired outside a fixture" in out["note"]


def runtime_branches():
    from app.analyst import DIRECTION_BRANCHES
    return set(DIRECTION_BRANCHES)


def test_branch_distribution_reports_a_real_override():
    envs = [_envelope(branches=("model", "missing-layer"), directions=("bullish", "unclear"))]
    out = smoke.branch_distribution(envs)
    assert out["counts"]["missing-layer"] == 1
    assert "At least one override branch fired" in out["note"]


def test_prose_suppressions_are_itemised_by_field_branch_and_literal():
    envs = [_envelope(
        branches=("model", "mixed"), directions=("bullish", "neutral"),
        suppressions=[{"horizon": "20d", "field": "thesis", "literals": ["173.80"],
                       "directionBranch": "model"}],
    )]
    out = smoke.prose_suppressions(envs)
    assert out["suppressions"] == 1 and out["theses"] == 2
    assert out["ratePerThesis"] == 0.5
    assert out["byField"] == {"thesis": 1}
    assert out["byDirectionBranch"] == {"model": 1}
    assert out["literals"] == {"173.80": 1}


def test_schema_failures_count_retries_and_stub_fallbacks_per_role():
    envs = [_envelope(outcomes={"final": {"retried": True, "stubbed": False},
                                "technical": {"retried": False, "stubbed": False}}),
            _envelope(outcomes={"final": {"retried": True, "stubbed": True}})]
    out = smoke.schema_failures(envs)
    assert out["stages"] == 3
    assert out["structuralFailures"] == 2
    assert out["fellBackToStubAfterRetry"] == 1
    assert out["perRole"]["final"] == {"stages": 2, "retried": 2, "stubbed": 1}


def test_the_enrichment_failed_path_is_reported_as_deleted_not_as_a_zero():
    """§9.54 binding 1 removed `failed` from `enrichment_state`. Reporting `0` for it would
    describe a branch that is not in the code as one that is in the code and never fired."""
    out = smoke.schema_failures([])
    path = out["enrichmentFailedPath"]
    assert path == {"reached": False, "reachable": False, "why": path["why"]}
    assert "resolved by" in path["why"] and "deletion" in path["why"]


def test_abstention_separates_a_model_abstention_from_a_server_override():
    envs = [_envelope(branches=("model", "no-stance"), directions=("neutral", "unclear"))]
    out = smoke.abstention_reachable(envs)
    assert out["abstentions"]["neutral"] == {"total": 1, "fromModel": 1}
    assert out["abstentions"]["unclear"] == {"total": 1, "fromModel": 0}
    assert out["modelReachedAbstention"] is True


def test_zero_banned_hits_is_reported_as_a_sample_size_not_a_clean_bill():
    out = smoke.banned_hits([_envelope() for _ in range(3)])
    assert out["hits"] == 0 and out["runs"] == 3
    assert "SAMPLE SIZE" in out["verdict"]
    assert "clean" in out["verdict"]  # only ever in the phrase denying it


def test_banned_hits_are_counted_with_their_denominator():
    envs = [_envelope(warnings=["contains advice phrase: 'buy the dip'"]), _envelope()]
    out = smoke.banned_hits(envs)
    assert out["hits"] == 1 and out["runs"] == 2 and out["ratePerRun"] == 0.5


# ── unmeasured is never zero ─────────────────────────────────────────────────────────────────────

def test_resident_memory_is_unmeasured_on_an_openai_dialect_not_zero():
    out = smoke.resident_memory(RT)
    assert out["value"] == smoke.UNMEASURED
    assert "exposes no memory endpoint" in out["reason"]


def test_resident_memory_reads_ollama_api_ps_when_it_can():
    rt = runtime.Runtime(name="p", kind="ollama", base="http://box:11434", model="qwen3:14b")

    class Resp:
        def json(self):
            return {"models": [{"name": "qwen3:14b", "size_vram": 9_000_000_000}]}

    out = smoke.resident_memory(rt, get=lambda url: Resp())
    assert out["value"] == 9_000_000_000 and out["unit"] == "bytes"


def test_time_to_first_token_is_unmeasured_by_construction():
    out = smoke.timings(4.0, [10.0, 12.0])
    assert out["timeToFirstTokenSeconds"]["value"] == smoke.UNMEASURED
    assert "stream:False" in out["timeToFirstTokenSeconds"]["reason"]
    assert out["coldLoadPlusFirstGenerationSeconds"] == 4.0
    # The number Lane 5A multiplies by its run count.
    assert out["secondsPerAnalystRun"] == 11.0


def test_no_runs_leaves_the_per_run_cost_unmeasured():
    out = smoke.timings(4.0, [])
    assert out["fullAnalystRunSeconds"]["value"] == smoke.UNMEASURED
    assert "secondsPerAnalystRun" not in out


# ── the report ───────────────────────────────────────────────────────────────────────────────────

def test_the_report_names_the_runtime_and_the_context_provenance(monkeypatch, reads_dir):
    monkeypatch.setattr(llm, "RUNTIME", RT)
    proof, code = smoke.prove_real_model(call_model=_served("qwen3-14b"))
    assert code == smoke.EXIT_OK
    measured = smoke.measure(2, run_analyst=lambda t, c: _envelope())
    result = smoke.report(proof, measured)
    md = smoke.render_markdown(result)
    assert "prod-a" in md
    assert "the MODEL is real, the CONTEXT is not" in md
    assert "unmeasured" in md          # resident memory and TTFT, stated as such
    assert result["servedRuntimes"] == ["prod-a"]


def test_the_written_json_drops_the_transcripts(monkeypatch, reads_dir):
    monkeypatch.setattr(llm, "RUNTIME", RT)
    proof, _ = smoke.prove_real_model(call_model=_served("qwen3-14b"))
    result = smoke.report(proof, smoke.measure(1, run_analyst=lambda t, c: _envelope()))
    files = smoke.write_report(result, str(reads_dir / "smoke"))
    written = json.loads(open(files["json"], encoding="utf-8").read())
    assert "envelopes" not in written
    assert written["runtime"]["name"] == "prod-a"
