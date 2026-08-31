"""Wave 3 Lane 3B — the analyst pipeline (contract §7, §5, §9.11, §9.17, §9.18, §9.19, §9.21).

THE LOAD-BEARING TEST IN THIS FILE IS `test_two_concurrent_analyst_posts_serialise_across_processes`.

Everything else here could be satisfied by a pipeline that never takes the model lease. §9.21's
whole point is that a `threading.Lock` gives ZERO exclusion between the `uvicorn` server and
`python -m app.enrich_worker`, which are different address spaces — so the serialisation test
spawns a genuine second interpreter, stands a real FastAPI app up inside it, and issues a real
`POST /analyst` from it while this process issues its own. It then proves, from timestamps written
by both processes, that no two generations ever overlapped.

Run:  cd services/llm && python -m pytest -q tests/test_analyst.py
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import textwrap
import threading
import time

import pytest

from app import analyst, lease, modes, tools
from app.committee import STANCE_NUM
from app.llm import safe_json
from app.prompt import BANNED

REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", ".."))


@pytest.fixture(autouse=True)
def reads_dir(tmp_path, monkeypatch):
    """Every test gets its own `{READS_DIR}/.model.lock`, so the lease is real but isolated."""
    monkeypatch.setenv("READS_DIR", str(tmp_path))
    lease.clear_marker()
    yield tmp_path
    lease.clear_marker()


# ═══════════════════════════════════════════════════════════════ fixtures

FULL_CTX = {
    "ticker": "NVDA",
    "regime": {"trend": "uptrend", "momentum": "neutral", "shortTermTrend": "rising",
               "volatility": "normal", "keyLevels": {"support": [181.0, 173.8],
                                                     "resistance": [188.4]}},
    "confluence": {"trendAlignment": "aligned"},
    "news": [{"headline": "supply deal", "sentiment": 0.4}],
    "macro": {"cpi": {"actual": 3.1, "expected": 3.2}},
    "marketContext": {"spx": "up"},
    "earnings": [{"surprise": 0.12}],
}


def _reply(**over) -> str:
    """One JSON object that satisfies EVERY role's schema at once, so a single fake `call_model` can
    drive the whole pipeline. Overrides let a test bend one field."""
    obj = {
        # thesis
        "direction": "bullish", "confidenceBucket": "high", "regime": "uptrend",
        "evidence": {"technical": "bullish", "companyNews": "positive", "macro": "neutral",
                     "marketContext": "positive", "fundamentals": "positive"},
        "thesis": "Structure and flow both point the same way over this horizon.",
        "invalidation": "daily close below 173.80 on above-average volume",
        "riskFlags": ["earnings in 6 sessions"],
        "whatChanged": [{"at": "2026-08-14T13:00:00Z", "eventId": "evt_1", "effect": "raised conviction"}],
        "sources": [{"eventId": "evt_1", "documentId": "doc_1", "url": "https://example.invalid/1"}],
        # orchestrator
        "specialists": ["technical", "news", "macro"], "why": "all three layers carry evidence",
        # specialists
        "stance": "bullish", "confidence": 0.8,
        "trendRegime": "uptrend", "structure": "higher highs", "momentumState": "neutral",
        "volatilityState": "normal", "levelsObserved": [181.0], "priceActionReading": "steady",
        "technicalRisks": ["extended above the 20 EMA"],
        "events": [], "newsRisks": [],
        "surprises": [], "sensitivity": "high", "marketBackdrop": "risk-on", "macroRisks": [],
        # conflict
        "weighting": "technical", "resolution": "structure outweighs a single headline",
    }
    obj.update(over)
    return json.dumps(obj)


def fake_model(reply: str | list[str], model_used="qwen3-14b", meta=None):
    """A `call_model` with the 3-tuple shape. `reply` may be a list, consumed one call at a time
    (the tail repeats), which is how the retry tests drive attempt 1 vs attempt 2."""
    replies = list(reply) if isinstance(reply, list) else [reply]
    calls: list[list[dict]] = []

    def call(messages, *, mode=None):
        calls.append(messages)
        r = replies[min(len(calls) - 1, len(replies) - 1)]
        m = dict(meta or {})
        m.setdefault("reasoningModeRequested", mode)
        m.setdefault("reasoningModeUsed", mode)
        return r, model_used, m

    call.calls = calls  # type: ignore[attr-defined]
    return call


def run(ctx=None, reply=None, **kw):
    return analyst.run_analyst("NVDA", ctx if ctx is not None else FULL_CTX,
                               fake_model(reply if reply is not None else _reply()),
                               safe_json, as_of="2026-08-15T14:32:00Z", **kw)


# ═══════════════════════════════════════════════════ 1. §7 conformance, all four horizons

def test_section_7_object_for_every_horizon():
    res = run()
    assert list(res["theses"]) == ["1d", "5d", "20d", "60d"]

    for key, obj in res["theses"].items():
        missing = [k for k in analyst.SECTION_7_KEYS if k not in obj]
        assert not missing, f"{key}: §7 keys missing {missing}"
        assert obj["ticker"] == "NVDA"
        assert obj["asOf"] == "2026-08-15T14:32:00Z"          # §1.4 echo
        assert obj["horizon"] == analyst.HORIZON_FIELD[key]
        assert obj["direction"] in analyst.DIRECTIONS
        assert obj["confidenceBucket"] in analyst.CONFIDENCE_BUCKETS
        assert set(obj["evidence"]) == set(analyst.EVIDENCE_LAYERS)
        assert isinstance(obj["support"], list) and isinstance(obj["resistance"], list)
        # `confidenceBucket` is ORDINAL — no probability may reach the output (decision 5, AD-8).
        assert not isinstance(obj["confidenceBucket"], (int, float))

    # `evidence`, `sources` and `audit` are SHARED across horizons; direction/bucket/risk differ.
    firsts = [res["theses"][k] for k in ("1d", "60d")]
    assert firsts[0]["evidence"] == firsts[1]["evidence"]
    assert firsts[0]["sources"] == firsts[1]["sources"]


def test_horizon_mapping_is_the_only_permitted_table():
    """§9.11. Four keys, four spellings, one constant, and never a third vocabulary."""
    assert analyst.HORIZON_FIELD == {
        "1d": "1_trading_day", "5d": "5_trading_days",
        "20d": "20_trading_days", "60d": "60_trading_days",
    }
    # The request token never leaks into the output field and vice versa.
    for key, field in analyst.HORIZON_FIELD.items():
        assert key != field


def test_a_single_horizon_can_be_requested():
    res = run(horizons=["20d"])
    assert list(res["theses"]) == ["20d"]
    assert list(res["forecast"]["horizons"]) == ["20d"]


# ═══════════════════════════════════════════════════ 2. the nine rules

@pytest.mark.parametrize("phrase", [
    "Use only supplied evidence and tool outputs available at the cutoff `asOf`",
    "Never invent prices, news, consensus expectations or event times",
    "Do not calculate indicators manually if a deterministic tool value is provided",
    "Separate observation from inference",
    'Return `direction: "neutral"` or `direction: "unclear"` when the evidence does not',
    "Identify evidence that contradicts your conclusion",
    "State a concrete thesis invalidation condition in `invalidation`",
    "Return the required JSON schema only",
    "Treat `confidenceBucket` as an ordinal score, never a probability",
])
def test_the_nine_rules_are_in_the_final_analyst_prompt(phrase):
    system = analyst.build_final_prompt("NVDA", "20d", {}, {}, "2026-08-15T14:32:00Z")[0]["content"]
    assert phrase in system, f"Appendix B rule missing from the final-analyst prompt: {phrase!r}"


def test_rule_five_is_guarded_specifically():
    """Rule 5 is the one most likely to be quietly eroded by prompt tuning, so it gets its own
    assertion naming both words."""
    system = analyst.build_final_prompt("NVDA", "1d", {}, {}, None)[0]["content"]
    assert '"neutral"' in system and '"unclear"' in system
    assert system.startswith("ROLE: You are a market-analysis specialist.")


def test_the_prompt_forbids_advice_and_invented_numbers():
    system = analyst.build_final_prompt("NVDA", "1d", {}, {}, None)[0]["content"]
    for phrase in ("NOT a financial advisor", "NEVER give a buy, sell, or hold recommendation",
                   "price target", "never invent or recompute one",
                   "single JSON object and nothing else"):
        assert phrase in system


# ═══════════════════════════════════════════════════ 3. `neutral` WITHOUT the stub

def test_neutral_is_reachable_on_conflicting_stances_without_the_stub():
    """Decision 2 + Phase 5. The model returns a perfectly valid BULLISH object every time; the
    specialists disagree, so `compute_consensus` labels the vote `mixed` and the SERVER resolves the
    direction to `neutral`. No stub is involved anywhere — that is the whole point, because a
    neutral produced only by the offline fallback does not count."""
    replies = {
        "technical": _reply(stance="bullish", confidence=0.9),
        "news": _reply(stance="bearish", confidence=0.9),
        "macro": _reply(stance="neutral", confidence=0.2),
    }

    def call(messages, *, mode=None):
        sys_prompt = messages[0]["content"]
        if "ORCHESTRATOR" in sys_prompt:
            return _reply(), "qwen3-14b", {"reasoningModeUsed": mode}
        for role, r in replies.items():
            if role == "technical" and "TECHNICAL specialist" in sys_prompt:
                return r, "qwen3-14b", {"reasoningModeUsed": mode}
            if role == "news" and "NEWS & EVENT specialist" in sys_prompt:
                return r, "qwen3-14b", {"reasoningModeUsed": mode}
            if role == "macro" and "MACRO & MARKET specialist" in sys_prompt:
                return r, "qwen3-14b", {"reasoningModeUsed": mode}
        return _reply(direction="bullish", confidenceBucket="high"), "qwen3-14b", {"reasoningModeUsed": mode}

    res = analyst.run_analyst("NVDA", FULL_CTX, call, safe_json, as_of="2026-08-15T14:32:00Z")

    assert res["consensus"]["label"] == "mixed", res["consensus"]
    for key, obj in res["theses"].items():
        assert obj["direction"] == "neutral", (key, obj["direction"], obj["directionReason"])
        assert res["forecast"]["horizons"][key]["abstain"] is True
        assert res["forecast"]["horizons"][key]["score"] == 0.0
    # NO stub anywhere: every stage ran on a real model reply.
    assert all("stub" not in str(m) for m in res["modelsUsed"].values()), res["modelsUsed"]
    assert not any("_stub" in json.dumps(s) for s in res["specialists"].values())


# ═══════════════════════════════════════════════════ 4. `unclear` WITHOUT the stub

def test_unclear_is_reachable_on_a_missing_evidence_layer_without_the_stub():
    """A cutoff at which the technical layer simply is not available. The model still answers, and
    still answers BULLISH — the server overrides it, because a thesis reasoned over a hole is not a
    low-confidence thesis, it is an absent one (§7's binding rule, Phase 3)."""
    ctx = {"ticker": "NVDA", "news": [{"headline": "x", "sentiment": 0.2}]}  # no technical evidence
    res = analyst.run_analyst("NVDA", ctx, fake_model(_reply(direction="bullish",
                                                            confidenceBucket="high")),
                              safe_json, as_of="2026-08-15T14:32:00Z")

    assert res["evidence"]["layersAvailable"]["technical"] is False
    for key, obj in res["theses"].items():
        assert obj["direction"] == "unclear", (key, obj["directionReason"])
        assert obj["evidence"]["technical"] == "unclear"
        assert obj["confidenceBucket"] == "low"
        assert res["forecast"]["horizons"][key]["abstain"] is True
        assert res["forecast"]["horizons"][key]["score"] == 0.0
        assert "required evidence layer unavailable" in obj["directionReason"]
    assert all("stub" not in str(m) for m in res["modelsUsed"].values()), res["modelsUsed"]


def test_a_terminal_tool_failure_is_evidence_not_an_abort(monkeypatch):
    """Phase 3: 3A's second-attempt failure is recorded, the layer degrades, the run continues."""
    def boom(tool, args, ctx):
        raise RuntimeError("upstream gone")

    monkeypatch.setattr(analyst, "_tools_execute", boom)
    res = run()
    trace = res["evidence"]["toolCalls"]
    assert trace, "a tool failure must still appear in the §5 toolCalls trace"
    assert any(t["status"].startswith("failed") for t in trace)
    assert all(t["status"].startswith(("failed", "skipped")) for t in trace)
    assert any("tool" in w and "failed" in w for w in res["warnings"])
    # and the run still produced four valid §7 objects rather than raising
    assert len(res["theses"]) == 4


def test_gateway_context_shape_activates_layers_and_reimposes_nested_levels(monkeypatch):
    """Production sends `tickerContext.{technical,recentEvents,...}`, not the legacy flat shape."""
    gateway_ctx = {
        "ticker": "NVDA",
        "asOf": "2026-08-15T14:32:00Z",
        "horizon": "20d",
        "subscriptions": [],
        "tickerContext": {
            "technical": {
                "price": {"lastClose": 205.0},
                "structure": {"structure": {
                    "supports": [200.0], "resistances": [210.0],
                }},
            },
            "recentEvents": {"events": [{"id": "evt_1a2b3c4d5e6f7081"}]},
            "earnings": {"points": [{"surprise": 0.12}]},
            "macro": {"macro": [{"release": "CPI"}]},
        },
        "marketContext": {"benchmark": {"ticker": "SPY"}},
    }
    monkeypatch.setattr(analyst, "_tools_execute", None)
    res = analyst.run_analyst("NVDA", gateway_ctx, fake_model(_reply()), safe_json,
                              as_of=None, horizons=["20d"])

    assert res["asOf"] == gateway_ctx["asOf"]
    assert res["evidence"]["layersAvailable"] == {
        "technical": True, "companyNews": True, "macro": True,
        "marketContext": True, "fundamentals": True,
    }
    thesis = res["theses"]["20d"]
    assert thesis["support"] == [200.0]
    assert thesis["resistance"] == [210.0]


def test_tool_runner_uses_each_declared_schema_and_event_dependency(monkeypatch):
    calls = []

    def execute(tool, args, _ctx):
        calls.append((tool, args))
        assert tools.validate_args(tool, args) == []
        payload = {"events": [{"id": "evt_1a2b3c4d5e6f7081"}]} if tool == "get_ticker_events" else {}
        return {"tool": tool, "ok": True, "result": payload, "error": None}

    monkeypatch.setattr(analyst, "_tools_execute", execute)
    trace, _results, any_success = analyst._run_tools(
        "news", {"ticker": "NVDA", "horizon": "20d"},
        "2026-08-15T14:32:00Z", [12],
    )

    assert any_success is True
    assert [c[0] for c in calls] == ["get_ticker_events", "get_event_details"]
    assert calls[0][1]["since"] == "2026-07-16T14:32:00Z"
    assert calls[1][1] == {"eventId": "evt_1a2b3c4d5e6f7081"}
    assert all(t["status"] == "ok" for t in trace)


def test_tool_error_envelope_is_not_reported_as_success(monkeypatch):
    def unavailable(tool, args, _ctx):
        assert args == {}
        return {"tool": tool, "ok": False, "result": None,
                "error": {"code": "upstream_unreachable"}}

    monkeypatch.setattr(analyst, "_tools_execute", unavailable)
    trace, results, any_success = analyst._run_tools(
        "macro", {"ticker": "NVDA"}, "2026-08-15T14:32:00Z", [12],
    )

    assert any_success is False
    assert len(results) == 2
    assert all(t["status"] == "failed: upstream_unreachable" for t in trace)


def test_the_run_is_bounded():
    assert analyst.MAX_TOOL_CALLS_PER_SPECIALIST == 3       # §6's per-specialist cap
    assert analyst.MAX_TOOL_CALLS_PER_RUN >= analyst.MAX_TOOL_CALLS_PER_SPECIALIST


# ═══════════════════════════════════════════════════ 5. levels re-imposed

def test_support_and_resistance_are_reimposed_over_adversarial_model_values():
    """Invariant #3 / §7. The model returns levels off by two orders of magnitude and claims they
    are support; the server overwrites them unconditionally with `services/analysis`'s computed
    values, exactly as `main.py::read` overwrites `keyLevels`."""
    absurd = _reply(support=[1.0, 2.0], resistance=[99999.0], levelsObserved=[1.0])
    res = analyst.run_analyst("NVDA", FULL_CTX, fake_model(absurd), safe_json,
                              as_of="2026-08-15T14:32:00Z")
    for obj in res["theses"].values():
        assert obj["support"] == [181.0, 173.8]
        assert obj["resistance"] == [188.4]


def test_levels_prefer_the_section_4_3_structure_when_present():
    ctx = dict(FULL_CTX)
    ctx["technicalContext"] = {"structure": {"supports": [200.0], "resistances": [210.0]}}
    res = analyst.run_analyst("NVDA", ctx, fake_model(_reply()), safe_json, as_of=None)
    assert res["theses"]["1d"]["support"] == [200.0]
    assert res["theses"]["1d"]["resistance"] == [210.0]


def test_missing_levels_produce_empty_lists_not_invented_ones():
    ctx = {"ticker": "NVDA", "regime": {"trend": "up"}, "news": [{"headline": "x"}]}
    res = analyst.run_analyst("NVDA", ctx, fake_model(_reply(support=[5.0])), safe_json, as_of=None)
    assert res["theses"]["1d"]["support"] == []


# ═══════════════════════════════════════════════════ 6. BANNED / BANNED_ANALYST (§9.19)

def test_banned_is_untouched():
    """§9.19: `BANNED` stays EXACTLY as it is — extending it would change `/read` for every user."""
    assert BANNED == ("i recommend buying", "i recommend selling", "you should buy",
                      "you should sell", "strong buy", "strong sell", "price target")


def test_banned_analyst_is_banned_plus_exactly_eight():
    assert analyst.BANNED_ANALYST[:len(BANNED)] == BANNED
    assert analyst.BANNED_ANALYST[len(BANNED):] == (
        "accumulate", "take profits", "add on weakness", "trim",
        "overweight", "underweight", "load up", "average down")


@pytest.mark.parametrize("phrase", ["accumulate", "take profits", "add on weakness", "trim",
                                    "overweight", "underweight", "load up", "average down",
                                    "price target", "strong buy"])
def test_banned_analyst_flags_every_phrase_case_insensitively(phrase):
    hits = analyst.banned_analyst_hits({"thesis": f"Investors should {phrase.upper()} here."})
    assert hits and phrase in hits[0]


@pytest.mark.parametrize("field", ["thesis", "invalidation", "transmission", "possibleReadThrough"])
def test_banned_analyst_covers_transmission_and_read_through(field):
    """§9.19 names four surfaces; a phrase in any of them must be flagged."""
    assert analyst.banned_analyst_hits({field: "time to take profits"})


def test_a_banned_phrase_in_the_model_reply_is_flagged_and_replaced():
    bad = _reply(thesis="Investors should trim into strength.")
    res = analyst.run_analyst("NVDA", FULL_CTX, fake_model(bad), safe_json, as_of=None)
    assert any("advice phrase" in w for w in res["warnings"])
    # and the offending text never reaches the output — the stub replaces it, as on /read.
    assert "trim into strength" not in json.dumps(res)


def test_validate_thesis_uses_the_same_posture_as_validate_structured():
    warnings = analyst.validate_thesis(json.loads(_reply(thesis="a strong buy setup")))
    assert any("advice phrase: 'strong buy'" in w for w in warnings)
    # A soft advice warning is NOT a structural failure — same rule `prompt.needs_retry` encodes.
    assert analyst.needs_thesis_retry(["contains advice phrase: 'strong buy'"]) is False
    assert analyst.needs_thesis_retry(["missing key: direction"]) is True
    assert analyst.needs_thesis_retry(["invalid direction: 'up'"]) is True


# ═══════════════════════════════════════════════════ 7. retry / stub

def test_exactly_one_firmer_retry_then_the_stub():
    """Mirrors `main.py::read`: attempt -> ONE firmer retry on structural failure -> stub."""
    cm = fake_model(["not json at all", "still not json"])
    res = analyst.run_analyst("NVDA", FULL_CTX, cm, safe_json, as_of=None, horizons=["1d"])

    # 5 stages (orchestrator, 3 specialists, 1 final) x 2 attempts each = 10 calls. Never 3.
    assert len(cm.calls) == 10, [m[0]["content"][:40] for m in cm.calls]
    # the second attempt of each pair carries the firmer instruction, and only the second
    firmer = ["Your previous reply was invalid" in m[-1]["content"] for m in cm.calls]
    assert firmer == [False, True] * 5

    assert res["modelsUsed"]["final"].endswith("(fell back to stub)")
    assert res["theses"]["1d"]["direction"] in analyst.DIRECTIONS


def test_a_stub_run_is_labelled_and_still_returns_a_valid_object():
    res = analyst.run_analyst("NVDA", FULL_CTX, fake_model("garbage", model_used="stub:offline"),
                              safe_json, as_of=None, horizons=["5d"])
    assert res["modelsUsed"]["final"] == "stub:offline"       # `_stub_label`'s convention preserved
    assert res["identity"]["modelUsed"] == "stub:offline"
    assert all(k in res["theses"]["5d"] for k in analyst.SECTION_7_KEYS)


def test_a_stub_model_is_not_retried():
    """`_stub_label` already means every provider was tried; a firmer retry would be theatre."""
    cm = fake_model("garbage", model_used="stub:quota")
    analyst.run_analyst("NVDA", FULL_CTX, cm, safe_json, as_of=None, horizons=["1d"])
    assert len(cm.calls) == 5     # one per stage, no retries


# ═══════════════════════════════════════════════════ 8. mode routing / recording

def test_mode_routing_follows_doc_section_8_via_lane_1d():
    """The table lives in `modes.py` as DATA; `analyst.py` maps role -> task and stops. A second
    implementation is how the mode ablation silently becomes meaningless."""
    assert modes.mode_for(analyst.ROLE_TASK["orchestrator"]) == modes.NON_THINKING
    assert modes.mode_for(analyst.ROLE_TASK["news"]) == modes.NON_THINKING
    assert modes.mode_for(analyst.ROLE_TASK["technical"]) == modes.THINKING
    assert modes.mode_for(analyst.ROLE_TASK["macro"]) == modes.THINKING
    assert modes.mode_for(analyst.ROLE_TASK["news_conflict"]) == modes.THINKING
    assert modes.mode_for(analyst.ROLE_TASK["final"]) == modes.THINKING


def test_the_requested_mode_is_sent_per_stage():
    seen: list[tuple[str, str | None]] = []

    def call(messages, *, mode=None):
        head = messages[0]["content"]
        role = ("orchestrator" if "ORCHESTRATOR" in head else
                "technical" if "TECHNICAL specialist" in head else
                "news" if "NEWS & EVENT specialist" in head else
                "macro" if "MACRO & MARKET specialist" in head else "final")
        seen.append((role, mode))
        return _reply(), "qwen3-14b", {"reasoningModeUsed": mode}

    analyst.run_analyst("NVDA", FULL_CTX, call, safe_json, as_of=None, horizons=["1d"])
    by_role = dict(seen)
    assert by_role["orchestrator"] == modes.NON_THINKING
    assert by_role["news"] == modes.NON_THINKING
    assert by_role["technical"] == modes.THINKING
    assert by_role["final"] == modes.THINKING


def test_a_rejected_mode_records_the_ACTUAL_mode_not_the_requested_one():
    """Invariant #8. A server that silently ignores `enable_thinking: false` and leaves thinking on
    must be visible in the record, or Wave 5A attributes the run to a cause it did not have."""
    cm = fake_model(_reply(), meta={"reasoningModeRequested": modes.NON_THINKING,
                                    "reasoningModeUsed": modes.THINKING,
                                    "degraded": True, "transport": "openai"})
    res = analyst.run_analyst("NVDA", FULL_CTX, cm, safe_json, as_of=None, horizons=["1d"])

    orch = res["reasoningModes"]["orchestrator"]
    assert orch["requested"] == modes.NON_THINKING
    assert orch["used"] == modes.THINKING, "the ACTUAL mode must be recorded"
    assert orch["degraded"] is True
    # and the §7 audit block carries the ACTUAL mode too
    assert res["theses"]["1d"]["audit"]["reasoningMode"] == modes.THINKING


def test_thinking_blocks_are_stripped_before_parsing():
    payload = "<think>let me reason about this at length</think>" + _reply()
    res = analyst.run_analyst("NVDA", FULL_CTX, fake_model(payload), safe_json, as_of=None,
                              horizons=["1d"])
    assert res["theses"]["1d"]["direction"] == "bullish"
    assert "<think" not in json.dumps(res)


# ═══════════════════════════════════════════════════ 9. version stamps (AD-4, §9.40)

def test_every_artefact_carries_its_version_stamp():
    res = run()
    for block in [res["identity"]] + [t["audit"] for t in res["theses"].values()]:
        for k in ("promptVersion", "schemaVersion", "toolSchemaVersion", "modelUsed",
                  "reasoningMode", "quantization", "generationSettings"):
            assert k in block, f"missing {k} in {block}"
        # §9.40: temperature lives in exactly ONE place.
        assert "temperature" not in block
        assert "temperature" in block["generationSettings"]
        assert block["promptVersion"] == analyst.PROMPT_VERSION_FINAL
        assert block["schemaVersion"] == analyst.SCHEMA_VERSION_THESIS


def test_prompt_versions_use_the_name_at_n_form():
    versions = run()["promptVersions"]
    assert set(versions) == {"orchestrator", "technical", "news", "macro", "final"}
    for v in versions.values():
        name, _, n = v.partition("@")
        assert name and n.isdigit(), v
    assert versions["final"] == "final-analyst@1"


# ═══════════════════════════════════════════════════ §9.17 — the computed score

def test_the_score_is_computed_in_code_and_the_model_never_emits_a_float():
    """§9.17. The model volunteers `score: 0.99`; it is recorded as a warning and STRIPPED, and the
    number in the record is `STANCE_NUM[direction] * CONFIDENCE_WEIGHT[bucket]`."""
    res = analyst.run_analyst("NVDA", FULL_CTX,
                              fake_model(_reply(score=0.99, probability=0.87)), safe_json,
                              as_of=None, horizons=["20d"])
    assert any("model emitted 'score'" in w for w in res["warnings"])
    assert "score" not in res["theses"]["20d"]
    assert "probability" not in res["theses"]["20d"]
    assert res["forecast"]["horizons"]["20d"]["score"] == 0.85   # 1.0 * 0.85, not 0.99
    # and 0.99 appears nowhere in the serialised result
    assert "0.99" not in json.dumps(res)


def test_compute_score_is_the_contract_formula_with_committee_stance_num():
    assert analyst.CONFIDENCE_WEIGHT == {"low": 0.25, "medium": 0.55, "high": 0.85}
    assert STANCE_NUM == {"bullish": 1.0, "neutral": 0.0, "bearish": -1.0}
    for d in ("bullish", "neutral", "bearish"):
        for b, wgt in analyst.CONFIDENCE_WEIGHT.items():
            assert analyst.compute_score(d, b) == round(STANCE_NUM[d] * wgt, 4)
    # `unclear` is absent from STANCE_NUM (a measured gap in §9.17 — see the handoff). An
    # abstention has no directional magnitude, so it scores 0.0, exactly as `neutral` does.
    for b in analyst.CONFIDENCE_WEIGHT:
        assert analyst.compute_score("unclear", b) == 0.0


def test_abstain_is_set_for_neutral_and_unclear_only():
    assert analyst._abstain("neutral") and analyst._abstain("unclear")
    assert not analyst._abstain("bullish") and not analyst._abstain("bearish")


# ═══════════════════════════════════════════════════ §9.18 — the disclosure string

def test_the_disclaimer_is_stamped_on_every_response_adjacent_to_the_output():
    res = run()
    assert res["disclaimer"] == analyst.ANALYST_DISCLAIMER
    for obj in res["theses"].values():
        # "adjacent to the output it qualifies" (DISCLOSURE_CLASSIFICATION rule 3): on each §7
        # object, not only once at the envelope level.
        assert obj["disclaimer"] == analyst.ANALYST_DISCLAIMER


def test_analyst_disclaimer_matches_the_gateway_literal_byte_for_byte():
    """§9.18's reconciliation. Lane 2C shipped `gateway/explore.go`'s `analystDisclaimer` before
    `analyst.py` existed; it must stay a Go literal (the gateway renders Explore with no network and
    no model), so the two constants can only be kept in step by a test that reads both."""
    src = open(os.path.join(REPO_ROOT, "gateway", "explore.go"), encoding="utf-8").read()
    start = src.index("const analystDisclaimer = ")
    end = src.index("\n\n", start)
    literal = src[start:end]
    # Reassemble the Go concatenated string literal into the string it denotes.
    parts = [seg.split('"')[1] for seg in literal.split("\n") if '"' in seg]
    go_value = "".join(parts).replace('\\"', '"')
    assert go_value == analyst.ANALYST_DISCLAIMER, (
        "§9.18 drifted between gateway/explore.go and services/llm/app/analyst.py:\n"
        f"  go: {go_value!r}\n  py: {analyst.ANALYST_DISCLAIMER!r}")


def test_the_model_never_supplies_the_disclaimer():
    res = analyst.run_analyst("NVDA", FULL_CTX,
                              fake_model(_reply(disclaimer="whatever the model felt like")),
                              safe_json, as_of=None, horizons=["1d"])
    assert res["theses"]["1d"]["disclaimer"] == analyst.ANALYST_DISCLAIMER
    assert "whatever the model felt like" not in json.dumps(res)


# ═══════════════════════════════════════════════════ 10. leakage (§1.5)

def test_a_historical_run_never_raises_with_the_http_client_patched_to_raise(monkeypatch):
    """§1.5's leakage test. Every outbound HTTP call raises, so `llm.py` exhausts its provider chain
    and returns its deterministic stub; the pipeline must still return a valid §7 object for the
    past cutoff, and must never raise.

    NOTE, recorded rather than papered over: `analyst.py` owns no store — Lane 3C persists the §5
    prediction record — so "from the store" resolves here to "from the deterministic floor, with no
    provider fetch". The assertion that matters either way is that a past cutoff issues no
    successful network call and still yields a valid object.
    """
    import requests

    def raise_always(*a, **kw):
        raise AssertionError("a historical request must not reach the network (§1.3)")

    monkeypatch.setattr(requests, "post", raise_always)
    monkeypatch.setattr(requests, "get", raise_always)

    res = analyst.run_analyst("NVDA", FULL_CTX, as_of="2026-01-05T21:00:00Z", horizons=["20d"])

    assert res["asOf"] == "2026-01-05T21:00:00Z"
    obj = res["theses"]["20d"]
    assert all(k in obj for k in analyst.SECTION_7_KEYS)
    assert obj["asOf"] == "2026-01-05T21:00:00Z"
    assert obj["direction"] in analyst.DIRECTIONS
    assert str(res["modelsUsed"]["final"]).startswith("stub:")
    # the SERVER-computed levels survive the offline path — they never came from a model
    assert obj["support"] == [181.0, 173.8]


# ═══════════════════════════════════════════════════ 11. invariant #6

def test_committee_py_is_untouched():
    """`services/prediction/app/committee_features.py` reads `save_committee`'s exact path and its
    `featureVector`. PARALLEL_CONTRACTS invariant #6 pins it, which is why this pipeline is a new
    file that IMPORTS `STANCE_NUM` and `compute_consensus` rather than editing them."""
    out = subprocess.run(["git", "diff", "HEAD", "--", "services/llm/app/committee.py"],
                         cwd=REPO_ROOT, capture_output=True, text=True)
    assert out.stdout.strip() == "", f"committee.py was modified:\n{out.stdout}"

    from app import store
    import inspect
    assert "committee" in inspect.getsource(store.save_committee)


def test_the_deterministic_primitives_are_imported_not_redefined():
    import app.committee as committee
    assert analyst.STANCE_NUM is committee.STANCE_NUM
    src = open(analyst.__file__, encoding="utf-8").read()
    assert "STANCE_NUM = {" not in src, "STANCE_NUM must be imported from committee.py, not redefined"
    assert "def compute_consensus" not in src


# ═══════════════════════════════════════════════════ 12. §9.21 — the lease

def test_every_generation_runs_inside_the_interactive_lease():
    """A positive measurement (§9.44), not a log grep: the fake model asserts, from inside the call,
    that this process holds the interactive lease. If any call site in `analyst.py` were moved
    outside `_generate`, this fails."""
    observed: list[bool] = []

    def call(messages, *, mode=None):
        observed.append(lease.interactive_lease_is_held())
        return _reply(), "qwen3-14b", {"reasoningModeUsed": mode}

    analyst.run_analyst("NVDA", FULL_CTX, call, safe_json, as_of=None)
    assert observed and all(observed), (
        f"{observed.count(False)} of {len(observed)} generations ran OUTSIDE the model lease")
    # and the lease is released afterwards
    assert lease.interactive_lease_is_held() is False


def test_there_is_no_second_in_process_lock():
    src = open(analyst.__file__, encoding="utf-8").read()
    assert "threading.Lock" not in src, (
        "§9.21: a threading lock gives zero exclusion across address spaces; the flock is the lock")
    # exactly one call site, and it is inside the lease
    assert src.count("with lease.acquire_interactive()") == 1


# --- the cross-process proof -----------------------------------------------------------------
#
# Modelled on `tests/test_lease.py::test_the_flock_excludes_a_real_second_os_process`, which is the
# Wave 2 test that would fail if someone "simplified" lease.py to a threading.Lock. The difference
# here is that the second process runs the REAL FastAPI app and issues a REAL `POST /analyst`,
# because §9.21's requirement is about two concurrent analyst requests, not about the lease in
# isolation.

_CHILD = textwrap.dedent(
    """
    import json, os, sys, time
    sys.path.insert(0, {app_root!r})
    os.environ["READS_DIR"] = {reads!r}
    os.environ["ENRICH_IDLE_SECONDS"] = "0"

    from app import llm

    LOG = {log!r}
    TAG = "child"

    def slow(messages, *, json_mode=True, mode=None):
        start = time.monotonic()
        time.sleep({gen})                      # a "generation" long enough to overlap if unlocked
        end = time.monotonic()
        with open(LOG, "a") as fh:
            fh.write(json.dumps({{"tag": TAG, "pid": os.getpid(),
                                  "start": start, "end": end}}) + "\\n")
        return {reply!r}, "qwen3-14b", {{"reasoningModeUsed": mode}}

    llm.call_model_meta = slow                 # patched in THIS process only

    from fastapi.testclient import TestClient
    from app.main import app

    with TestClient(app) as client:
        open({ready!r}, "w").write(str(os.getpid()))
        while not os.path.exists({go!r}):
            time.sleep(0.01)
        r = client.post("/analyst", json={{"ticker": "NVDA", "horizon": "20d",
                                           "context": {ctx!r}}})
        assert r.status_code == 200, r.text
        body = r.json()
        assert body["theses"]["20d"]["horizon"] == "20_trading_days", body
    """
)


#: How long one fake "generation" takes on both sides. Long enough that two unlocked generations
#: would visibly overlap, short enough that ten of them are a few seconds.
GEN_SECONDS = 0.25


def test_two_concurrent_analyst_posts_serialise_across_processes(tmp_path):
    """§9.21, THE test this lane exists for.

    A REAL second OS process (`subprocess.Popen(sys.executable, ...)`) stands up the real FastAPI
    app and posts `/analyst`; this process does the same, at the same time, against the same
    `{READS_DIR}/.model.lock`. Each "generation" writes its own [start, end] interval to a shared
    log. The assertion is that NO interval from one process overlaps ANY interval from the other.

    A `threading` test cannot prove this. uvicorn and `python -m app.enrich_worker` are different
    address spaces, so an in-process lock excludes nothing between them — which is exactly the
    situation the lease exists for and exactly what this spawns.
    """
    from fastapi.testclient import TestClient

    log = tmp_path / "gen.log"
    ready = tmp_path / "child.ready"
    go = tmp_path / "go"
    app_root = os.path.dirname(os.path.dirname(os.path.abspath(analyst.__file__)))
    ctx = {"regime": {"trend": "uptrend", "keyLevels": {"support": [181.0], "resistance": [188.4]}},
           "news": [{"headline": "x"}]}

    child = subprocess.Popen(
        [sys.executable, "-c", _CHILD.format(app_root=app_root, reads=str(tmp_path),
                                             log=str(log), ready=str(ready), go=str(go),
                                             reply=_reply(), ctx=ctx, gen=GEN_SECONDS)],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )

    deadline = time.monotonic() + 60
    while not ready.exists() and time.monotonic() < deadline:
        if child.poll() is not None:
            out, err = child.communicate()
            raise AssertionError(f"child died before serving:\n{err.decode()[-3000:]}")
        time.sleep(0.02)
    assert ready.exists(), "the child never became ready"

    # This process: patch the same seam, in OUR address space only.
    from app import llm as _llm
    saved = _llm.call_model_meta

    def slow(messages, *, json_mode=True, mode=None):
        start = time.monotonic()
        time.sleep(GEN_SECONDS)
        end = time.monotonic()
        with open(log, "a") as fh:
            fh.write(json.dumps({"tag": "parent", "pid": os.getpid(),
                                 "start": start, "end": end}) + "\n")
        return _reply(), "qwen3-14b", {"reasoningModeUsed": mode}

    _llm.call_model_meta = slow
    try:
        from app.main import app as fastapi_app
        result: dict = {}

        # The client is built BEFORE the starting gun, so neither side pays app-startup cost inside
        # the measured window and both really are released at the same instant.
        with TestClient(fastapi_app) as client:
            def post():
                r = client.post("/analyst", json={"ticker": "NVDA", "horizon": "20d",
                                                  "context": ctx})
                result["status"] = r.status_code
                result["body"] = r.json()

            t = threading.Thread(target=post)
            t.start()
            go.write_text("go")      # release the child at the same instant
            t.join(timeout=180)
            assert not t.is_alive(), "this process's analyst POST never returned"
        child.wait(timeout=180)
    finally:
        _llm.call_model_meta = saved
        if child.poll() is None:  # pragma: no cover
            child.kill()

    out, err = child.communicate()
    assert child.returncode == 0, f"child failed:\n{err.decode()[-3000:]}"
    assert result["status"] == 200, result

    records = [json.loads(line) for line in log.read_text().splitlines() if line.strip()]
    pids = {r["pid"] for r in records}
    assert len(pids) == 2, (
        f"expected generations from TWO processes, saw {len(pids)} — a single-process run proves "
        f"nothing about a cross-process lease. records={records}")
    parent = [r for r in records if r["tag"] == "parent"]
    kid = [r for r in records if r["tag"] == "child"]
    assert len(parent) >= 5 and len(kid) >= 5, (len(parent), len(kid))

    # ASSERTION 1 — NO OVERLAP. `time.monotonic()` is boot-relative on both Linux (CLOCK_MONOTONIC)
    # and macOS (mach_absolute_time), so it shares an epoch across processes on one machine and the
    # intervals are directly comparable. Any overlap at all means two generations ran at once, on a
    # box whose whole memory budget assumes one.
    overlaps = [(a, b) for a in parent for b in kid
                if a["start"] < b["end"] and b["start"] < a["end"]]
    assert not overlaps, (
        f"{len(overlaps)} generation pairs from two different OS processes OVERLAPPED — the "
        f"analyst is not taking the cross-process model lease (§9.21). first: {overlaps[0]}")

    # ASSERTION 2 — THE POSITIVE CONTROL (§9.44: measure, do not infer from an absence).
    #
    # No-overlap on its own could also be produced by two runs that simply never met. They met: the
    # child wrote `ready` and then blocked on `go`, which was written after this process's request
    # thread had already started, so both were released within milliseconds of each other and both
    # wanted the model at the same time. Under that condition, WALL TIME is the discriminator —
    # ten 0.25 s generations that serialise take ~2.5 s end to end, while ten that ran two-at-a-time
    # would take ~1.25 s. A pipeline that dropped the lease fails here even if the sampling happened
    # to miss an overlap.
    span = max(r["end"] for r in records) - min(r["start"] for r in records)
    serial_floor = 0.9 * GEN_SECONDS * len(records)
    assert span >= serial_floor, (
        f"{len(records)} generations of {GEN_SECONDS}s finished in {span:.2f}s (serial floor "
        f"{serial_floor:.2f}s) — they overlapped in wall time, so the lease is not holding")
    assert go.exists()

    # ASSERTION 3 — the lease is PER GENERATION, not per run: control changed hands at least once,
    # so a waiting interactive request is never stuck behind a whole eight-call pipeline.
    ordered = sorted(records, key=lambda r: r["start"])
    switches = sum(1 for i in range(1, len(ordered)) if ordered[i]["tag"] != ordered[i - 1]["tag"])
    assert switches >= 1, (
        f"the two processes never interleaved ({[r['tag'] for r in ordered]}) — one held the model "
        "for its entire run, which starves the other for the whole pipeline")


# ═══════════════════════════════════════════════════ invariant #1 / #4 source assertions

def test_no_timer_or_scheduler_in_the_analyst_module():
    """Invariant #4 as a source-level assertion, the same shape §9.22 uses on `enrich_worker.py`."""
    src = open(analyst.__file__, encoding="utf-8").read()
    for forbidden in ("while True:", "time.sleep(", "threading.Timer(", "asyncio.create_task(",
                      "BackgroundTasks", "sched.scheduler"):
        assert forbidden not in src, f"invariant #4: {forbidden!r} must not appear in analyst.py"


def test_the_route_is_post_only():
    from app.main import app
    paths = app.openapi()["paths"]
    assert "/analyst" in paths, "§9.28: analyst.py must decorate the Wave 0 analyst_router seam"
    assert set(paths["/analyst"]) == {"post"}, (
        "a GET invites a prefetch, and a prefetch is an invariant #4 violation")


def test_the_pipeline_runs_with_no_context_at_all():
    """Invariant #1's floor: zero keys, zero network, empty envelope. It must still answer, and the
    honest answer with no evidence is `unclear`."""
    res = analyst.run_analyst("NVDA", {}, fake_model(_reply()), safe_json, as_of=None)
    for obj in res["theses"].values():
        assert obj["direction"] == "unclear"
        assert all(v == "unclear" for v in obj["evidence"].values())
        assert obj["disclaimer"] == analyst.ANALYST_DISCLAIMER


def test_specialists_run_sequentially_one_model_call_at_a_time():
    """Decision 7. Measured by counting how many calls are in flight at once."""
    inflight = {"now": 0, "max": 0}

    def call(messages, *, mode=None):
        inflight["now"] += 1
        inflight["max"] = max(inflight["max"], inflight["now"])
        time.sleep(0.005)
        inflight["now"] -= 1
        return _reply(), "qwen3-14b", {"reasoningModeUsed": mode}

    analyst.run_analyst("NVDA", FULL_CTX, call, safe_json, as_of=None)
    assert inflight["max"] == 1, f"{inflight['max']} model calls were in flight at once"


# ══════════════════════════════════ 12. §9.56 — prose and direction share one authority

MODEL_PROSE = "Structure and flow both point the same way over this horizon."
"""The literal sentence Wave 3 integration measured surviving an override to `unclear`. It is the
whole reason §9.56 exists, so the tests below assert against it by name rather than by shape."""


def test_a_server_override_to_neutral_replaces_the_model_prose_and_keeps_it_for_audit():
    """§9.56 on the `mixed` branch. Same fixture as the `neutral`-without-the-stub test: the model
    answers bullish at every stage and writes bullish prose; the server resolves `neutral`."""
    replies = {
        "technical": _reply(stance="bullish", confidence=0.9),
        "news": _reply(stance="bearish", confidence=0.9),
        "macro": _reply(stance="neutral", confidence=0.2),
    }

    def call(messages, *, mode=None):
        sys_prompt = messages[0]["content"]
        for role, r in replies.items():
            if role == "technical" and "TECHNICAL specialist" in sys_prompt:
                return r, "qwen3-14b", {"reasoningModeUsed": mode}
            if role == "news" and "NEWS & EVENT specialist" in sys_prompt:
                return r, "qwen3-14b", {"reasoningModeUsed": mode}
            if role == "macro" and "MACRO & MARKET specialist" in sys_prompt:
                return r, "qwen3-14b", {"reasoningModeUsed": mode}
        return _reply(thesis=MODEL_PROSE), "qwen3-14b", {"reasoningModeUsed": mode}

    res = analyst.run_analyst("NVDA", FULL_CTX, call, safe_json, as_of=None)

    for key, obj in res["theses"].items():
        assert obj["direction"] == "neutral", (key, obj["directionReason"])
        assert obj["directionBranch"] == "mixed"
        assert obj["modelThesis"] == MODEL_PROSE          # kept, verbatim, for audit
        assert obj["thesis"] != MODEL_PROSE               # not rendered
        assert "does not resolve to a directional lean" in obj["thesis"]
        assert "net out mixed" in obj["thesis"]
    # §5's forecast block reads `theses[keys[0]]["thesis"]`, so it carries the server line too.
    assert res["forecast"]["thesis"] == res["theses"]["1d"]["thesis"]


def test_a_server_override_to_unclear_replaces_the_model_prose():
    """§9.56 on the `missing-layer` branch — the exact run Wave 3 integration measured."""
    ctx = {"ticker": "NVDA", "news": [{"headline": "x", "sentiment": 0.2}]}   # no technical layer
    res = analyst.run_analyst("NVDA", ctx,
                              fake_model(_reply(direction="bullish", confidenceBucket="high",
                                                thesis=MODEL_PROSE)),
                              safe_json, as_of=None)

    for key, obj in res["theses"].items():
        assert obj["direction"] == "unclear", (key, obj["directionReason"])
        assert obj["directionBranch"] == "missing-layer"
        assert obj["modelThesis"] == MODEL_PROSE
        assert "does not support a directional read" in obj["thesis"]
        assert "required evidence layer" in obj["directionReason"]


def test_the_model_prose_survives_when_the_model_owns_the_direction():
    """The other half of §9.56: no override, no replacement, and NO `modelThesis`. Its presence is
    the audit signal, so it must be absent exactly when nothing was replaced."""
    res = analyst.run_analyst("NVDA", FULL_CTX, fake_model(_reply(thesis=MODEL_PROSE)),
                              safe_json, as_of=None)
    for key, obj in res["theses"].items():
        assert obj["direction"] == "bullish", (key, obj["directionReason"])
        assert obj["directionBranch"] == "model"
        assert obj["thesis"] == MODEL_PROSE
        assert "modelThesis" not in obj


def test_no_rendered_thesis_asserts_a_direction_inconsistent_with_direction():
    """Clause A's binding test, stated at the level the server can actually guarantee.

    An abstaining `direction` with prose asserting a lean is the defect. Where the SERVER resolved
    the direction (`directionBranch != "model"`) it also wrote the prose, so the guarantee is
    absolute and asserted as such. Where the MODEL owns the direction the server has no standing
    to rewrite, so the guarantee is a recorded `warnings` entry — asserted here too, because a
    detector nobody checks is not a detector."""
    cases = [
        # (ctx, reply overrides) -> every one of these resolves to an abstaining direction
        (FULL_CTX, {"direction": "neutral", "thesis": "The setup looks bullish into the print."}),
        ({"ticker": "NVDA", "news": [{"headline": "x"}]},
         {"direction": "bullish", "thesis": MODEL_PROSE}),
        (FULL_CTX, {"direction": "sideways-ish", "thesis": MODEL_PROSE}),
    ]
    for ctx, over in cases:
        res = analyst.run_analyst("NVDA", ctx, fake_model(_reply(**over)), safe_json, as_of=None,
                                  horizons=["5d"])
        obj = res["theses"]["5d"]
        # WIDENED by binding 9 from "every override path" to EVERY path. The `elif` this replaces
        # asserted only that a warning was recorded on the model-owned branch — and a warning does
        # not stop rendering, so that branch still shipped the defect. The guarantee is now
        # absolute on both: an abstaining direction never renders prose the model wrote.
        if obj["directionBranch"] != "model" or obj["direction"] in ("neutral", "unclear"):
            assert obj["thesis"] == analyst._server_thesis(
                obj["direction"], obj["directionBranch"],
                analyst.HORIZON_FIELD["5d"].replace("_", " ")), over
        low = obj["thesis"].lower()
        if obj["direction"] in ("neutral", "unclear"):
            assert not any(w in low for w in ("bullish", "bearish")), (over, obj["thesis"])


def test_every_server_authored_thesis_is_clean_of_numbers_and_banned_phrases():
    """The server line is prose the product renders, so it is held to the rules the model's is:
    invariant #3 (the server does not author a level into narrative either) and §9.19."""
    for direction in analyst.DIRECTIONS:
        for branch in analyst.DIRECTION_BRANCHES:
            if branch == "model":
                continue
            line = analyst._server_thesis(direction, branch, "5 trading days")
            assert not analyst.banned_analyst_hits({"thesis": line}), (direction, branch, line)
            assert not any(ch.isdigit() and ch != "5" for ch in line), (direction, branch, line)
            assert line.endswith("."), (direction, branch, line)
            # Rendered as PLAIN TEXT in the Attestel view — no markup may travel in it.
            assert not any(ch in line for ch in "`*_<>#[]"), (direction, branch, line)


def test_a_banned_phrase_never_reaches_the_audit_copy_either():
    """§9.56 introduced a SECOND rendered-prose key, so the question "can advice hide in it?" has to
    be answered rather than assumed. It cannot, and not because §9.56 scrubs it: `_run_step`
    replaces the whole object with the deterministic stub on any `banned_analyst_hits`, so the
    banned sentence is gone before the override loop ever sees it — and the flag survives in
    `warnings` so the evidence that it was produced is not erased (§9.19)."""
    ctx = {"ticker": "NVDA", "news": [{"headline": "x"}]}          # forces the `unclear` override
    res = analyst.run_analyst("NVDA", ctx,
                              fake_model(_reply(thesis="I recommend buying this dip.")),
                              safe_json, as_of=None, horizons=["1d"])
    obj = res["theses"]["1d"]
    assert "buying" not in obj["thesis"]
    assert "buying" not in obj["modelThesis"]                  # holds the stub's line, not the model's
    assert "no interpretive thesis was generated" in obj["modelThesis"]
    assert any("i recommend buying" in w.lower() for w in res["warnings"]), res["warnings"]
    assert not analyst.banned_analyst_hits({"thesis": obj["thesis"],
                                            "modelThesis": obj["modelThesis"]})


# ══════════════════════════ 13. §9.56 bindings 7–9 — invalidation, numbers, every path

MODEL_INVALIDATION = "daily close below 173.80 on above-average volume"
"""The fixture's falsification condition, and the string binding 7 was written about. It names a
price, which is what makes it worse than the thesis case rather than merely parallel to it."""


def test_an_override_replaces_the_priced_invalidation_and_keeps_it_for_audit():
    """Binding 7 on the `missing-layer` branch — §9.56 binding 6's open item, closed.

    The measured defect: a run resolved to `unclear` still carried a *bullish* thesis's
    falsification condition. Nothing in invariant #2 is breached by the letter — no verdict word
    appears — but a specific price paired with a specific trigger, rendered beside an abstention,
    reads as a level to act on."""
    ctx = {"ticker": "NVDA", "news": [{"headline": "x", "sentiment": 0.2}]}   # no technical layer
    res = analyst.run_analyst("NVDA", ctx,
                              fake_model(_reply(thesis=MODEL_PROSE,
                                                invalidation=MODEL_INVALIDATION)),
                              safe_json, as_of=None)

    for key, obj in res["theses"].items():
        assert obj["direction"] == "unclear", (key, obj["directionReason"])
        assert obj["directionBranch"] == "missing-layer"
        assert obj["modelInvalidation"] == MODEL_INVALIDATION      # kept verbatim, for audit
        assert obj["invalidation"] != MODEL_INVALIDATION           # not rendered
        assert "no falsification condition is stated" in obj["invalidation"]
        assert "173" not in obj["invalidation"], obj["invalidation"]
    # §5's forecast block reads `theses[keys[0]]`, so the price is gone from there too.
    assert "173" not in res["forecast"]["invalidation"]


def test_a_model_owned_abstention_also_loses_the_priced_invalidation():
    """Bindings 7 and 9 together, on the branch neither one alone watches.

    The MODEL owns the direction and abstains in it, so no override fires — and before binding 9
    that branch rendered both the model's confident prose and its priced falsification condition
    beside the abstention. Same user-visible defect, different door."""
    res = analyst.run_analyst("NVDA", FULL_CTX,
                              fake_model(_reply(direction="neutral", thesis=MODEL_PROSE,
                                                invalidation=MODEL_INVALIDATION)),
                              safe_json, as_of=None, horizons=["5d"])
    obj = res["theses"]["5d"]

    assert obj["direction"] == "neutral"
    assert obj["directionBranch"] == "model"          # nothing was overridden
    assert obj["thesis"] != MODEL_PROSE               # and the prose is still the server's
    assert obj["modelThesis"] == MODEL_PROSE
    assert obj["modelInvalidation"] == MODEL_INVALIDATION
    assert "no falsification condition is stated" in obj["invalidation"]


def test_both_prose_fields_survive_when_the_model_owns_a_directional_read():
    """The other half: nothing is replaced, so NEITHER audit key exists. Their presence is the
    audit signal, so they must be absent exactly when nothing was replaced."""
    res = analyst.run_analyst("NVDA", FULL_CTX,
                              fake_model(_reply(thesis=MODEL_PROSE,
                                                invalidation=MODEL_INVALIDATION)),
                              safe_json, as_of=None, horizons=["5d"])
    obj = res["theses"]["5d"]

    assert obj["direction"] == "bullish" and obj["directionBranch"] == "model"
    assert obj["thesis"] == MODEL_PROSE
    assert obj["invalidation"] == MODEL_INVALIDATION
    assert "modelThesis" not in obj and "modelInvalidation" not in obj


def test_a_server_resolved_directional_read_never_claims_no_direction_was_resolved():
    """Binding 7 says the server owns `invalidation` on ANY override branch, including the ones
    that resolve to a DIRECTION. The abstention line would be false there, and a false server line
    beside a served field is the very defect §9.56 exists to prevent — so the directional override
    gets its own sentence.

    Asserted at unit level because the branch is unreachable through `run_analyst` today:
    `validate_thesis` rejects an out-of-enum `direction` before `_resolve_direction` ever sees it,
    so `_run_step` substitutes the stub and the run arrives on the `model` branch carrying
    `unclear`. The clause still binds the branch, and the line still has to be right when a future
    validator change lets it through."""
    assert analyst._resolve_direction("sideways-ish", {"technical": True},
                                      {"label": "bullish-lean"}) == (
        "bullish",
        "model direction 'sideways-ish' outside the enum; fell back to consensus 'bullish-lean'",
        "out-of-enum")

    line = analyst._server_invalidation("bullish")
    assert "no falsification condition is stated" in line
    assert "no directional read was resolved" not in line.lower()
    for direction in ("neutral", "unclear"):
        assert "No directional read was resolved" in analyst._server_invalidation(direction)


def test_no_abstaining_run_renders_model_prose_by_any_branch():
    """Binding 9's guarantee stated as one property over every route to an abstention, which is
    what "every path, not only override paths" means. The detector is the served `direction`, not
    a reading of the prose — so there is no sentence that can slip past it."""
    routes = [
        ({"ticker": "NVDA", "news": [{"headline": "x"}]}, {}),                       # missing-layer
        (FULL_CTX, {"direction": "neutral"}),                                        # model, abstains
        (FULL_CTX, {"direction": "unclear"}),                                        # model, abstains
        ({"ticker": "NVDA", "regime": FULL_CTX["regime"],
          "news": [{"headline": "x", "sentiment": -0.9}]}, {"stance": "bearish"}),   # consensus route
    ]
    for ctx, over in routes:
        res = analyst.run_analyst("NVDA", ctx,
                                  fake_model(_reply(thesis=MODEL_PROSE,
                                                    invalidation=MODEL_INVALIDATION, **over)),
                                  safe_json, as_of=None, horizons=["5d"])
        obj = res["theses"]["5d"]
        if obj["direction"] not in ("neutral", "unclear"):
            continue
        assert obj["thesis"] != MODEL_PROSE, (ctx, over)
        assert obj["invalidation"] != MODEL_INVALIDATION, (ctx, over)
        assert "173" not in obj["invalidation"], (ctx, over, obj["invalidation"])


def test_the_contradiction_warning_survives_as_an_audit_signal():
    """Binding 9 kept binding 5's warning after taking its job away: that the model produced prose
    contradicting its own abstention is evidence ABOUT THE MODEL, and §9.20's ablation wants it.
    It now describes `modelThesis`, which is where that prose lives."""
    res = analyst.run_analyst("NVDA", FULL_CTX,
                              fake_model(_reply(direction="neutral",
                                                thesis="The setup looks bullish into the print.")),
                              safe_json, as_of=None, horizons=["5d"])
    assert any("asserts 'bullish'" in w for w in res["warnings"]), res["warnings"]
    assert res["theses"]["5d"]["modelThesis"] == "The setup looks bullish into the print."


# ────────────────────────────── binding 8 — a rendered number is a served number

def test_the_measured_number_is_served_on_one_run_and_unchecked_on_the_other():
    """Binding 8's REQUIRED MEASUREMENT, encoded so it cannot rot.

    The finding, both directions: `173.80` **is** a member of the served support set on a
    full-context run (`[181.0, 173.8]`), and is **not** a member on the `unclear` run §9.56 binding
    6 measured — that context carries no `regime`, so the served set is empty. The first is a
    coincidence of the fixture, which was written by copying the contract's own example; nothing in
    the pipeline compared the two. `_reimpose_levels` overwrites the `support`/`resistance` ARRAYS
    and has never looked at prose."""
    full = analyst._levels_from_context(FULL_CTX)
    assert full == {"support": [181.0, 173.8], "resistance": [188.4]}
    assert analyst.unserved_numbers(MODEL_INVALIDATION, full) == []        # served — by luck

    bare = analyst._levels_from_context({"ticker": "NVDA", "news": [{"headline": "x"}]})
    assert bare == {"support": [], "resistance": []}
    assert analyst.unserved_numbers(MODEL_INVALIDATION, bare) == ["173.80"]


def test_an_unserved_number_is_suppressed_not_merely_warned_about():
    """§9.58 binding 1. `150.00` is not in `[181.0, 173.8, 188.4]`, and the model branch renders the
    model's own `invalidation`, so this is the live path — not a hypothetical.

    This test asserted the OPPOSITE under binding 8 alone: that the sentence was rendered and a
    warning recorded beside it. A warning does not stop rendering, and what reached the screen here
    was a price. Invariant #3 is not "the model's numbers are audited"; it is that the model never
    invents a number."""
    res = analyst.run_analyst("NVDA", FULL_CTX,
                              fake_model(_reply(invalidation="daily close below 150.00 on volume")),
                              safe_json, as_of=None, horizons=["5d"])
    obj = res["theses"]["5d"]

    assert "150.00" not in obj["invalidation"]                       # off the screen
    assert obj["modelInvalidation"] == "daily close below 150.00 on volume"   # still on the record
    assert any("150.00" in w and "suppressed" in w for w in res["warnings"]), res["warnings"]
    assert res["proseSuppressions"] == [
        {"horizon": "5_trading_days", "field": "invalidation", "literals": ["150.00"],
         "directionBranch": "model"}]


def test_a_withdrawn_rationale_states_the_direction_it_is_withdrawing_it_for():
    """§9.58 binding 2, the served-directional-read case. The direction STANDS — only the prose is
    withdrawn — so a line reading like an abstention would contradict the served field, which is
    §9.56's whole subject. And the withdrawal is stated plainly rather than dropped silently:
    a silent drop is an uncountable defect."""
    res = analyst.run_analyst("NVDA", FULL_CTX,
                              fake_model(_reply(thesis="A close over 999.99 confirms the trend.")),
                              safe_json, as_of=None, horizons=["5d"])
    obj = res["theses"]["5d"]

    assert obj["direction"] == "bullish"                       # untouched by the suppression
    assert "999.99" not in obj["thesis"]
    assert "resolves to a bullish read" in obj["thesis"]       # says WHICH direction
    assert "rationale is not shown" in obj["thesis"]           # says the rationale was withheld
    assert "did not compute" in obj["thesis"]                  # says why
    assert obj["modelThesis"] == "A close over 999.99 confirms the trend."
    assert analyst.unserved_numbers(obj["thesis"], analyst._levels_from_context(FULL_CTX)) == []


def test_an_abstention_reuses_the_abstention_line_rather_than_the_withheld_one():
    """§9.58 binding 2's other half. Unit-level: an abstaining run is already server-owned, so its
    prose carries no unserved number and the branch cannot fire end-to-end. The two lines must
    still be distinct, because saying "the rationale was withheld" about a run that resolved
    nothing would invent a rationale that never existed."""
    for direction in ("neutral", "unclear"):
        line = analyst._server_thesis(direction, "missing-layer", "5 trading days")
        assert "rationale is not shown" not in line
        assert "No directional read was resolved" in analyst._server_invalidation(direction)
    withheld = analyst._withheld_thesis("bullish", "5 trading days")
    assert "resolves to a bullish read" in withheld and "rationale is not shown" in withheld


def test_the_suppression_count_is_itemised_per_horizon_and_field():
    """§9.58 binding 4. Rare is expected; common means the defect is upstream — in the prompt or in
    what the specialists hand the model — and a rendering-layer replacement is hiding it. A bare
    count could not tell those apart, so each entry names the horizon, the field and the literal."""
    res = analyst.run_analyst("NVDA", FULL_CTX,
                              fake_model(_reply(thesis="Over 999.99 it runs.",
                                                invalidation="Under 12.34 it fails.")),
                              safe_json, as_of=None)

    assert len(res["proseSuppressions"]) == 2 * len(res["horizons"])
    assert {e["field"] for e in res["proseSuppressions"]} == {"thesis", "invalidation"}
    assert {e["horizon"] for e in res["proseSuppressions"]} == {
        analyst.HORIZON_FIELD[k] for k in res["horizons"]}
    assert all(e["literals"] for e in res["proseSuppressions"])
    # A clean run counts nothing — otherwise "common" could never be distinguished from "always".
    clean = analyst.run_analyst("NVDA", FULL_CTX, fake_model(_reply()), safe_json, as_of=None)
    assert clean["proseSuppressions"] == []


def test_the_exemption_is_a_quantity_of_periods_and_nothing_else():
    """§9.58 binding 3 — narrowed and named. The exemption exists for ONE reason: the server's own
    line carries `HORIZON_FIELD`'s period count. Anything wider is a hole a price can walk through
    wearing a unit."""
    levels = {"support": [181.0], "resistance": []}
    for exempt in ("Resolves over the next 5 trading days.", "Earnings in 6 sessions.",
                   "3 weeks of range.", "2 quarters of margin expansion."):
        assert analyst.unserved_numbers(exempt, levels) == [], exempt
    for not_exempt in ("Closes above 173 in 30 minutes.", "A 250 share position.",
                       "Up 12 percent.", "Covered by 4 analysts at 195.50."):
        assert analyst.unserved_numbers(not_exempt, levels), not_exempt


def test_a_time_quantity_is_not_read_as_a_price():
    """A numeric literal that quantifies a unit of TIME is a horizon, not a price claim. The
    server's own thesis line carries one by construction — `HORIZON_FIELD` is `5_trading_days` —
    so a rule reading every digit as a level would fail the server's own prose first."""
    levels = {"support": [181.0], "resistance": []}
    assert analyst.unserved_numbers("Resolves over the next 5 trading days.", levels) == []
    assert analyst.unserved_numbers("Earnings in 6 sessions; 3 weeks of range.", levels) == []
    assert analyst.unserved_numbers("A close below 181.00 ends it.", levels) == []
    assert analyst.unserved_numbers("A close below 181.4 ends it.", levels) == ["181.4"]


def test_no_server_authored_prose_carries_an_unserved_number():
    """Every server line, on every branch and direction, against an EMPTY served set — the hardest
    case, because with no levels served any number at all is unserved. The thesis line's horizon
    phrase is the only numeric literal it may contain, and it is exempt as a time quantity."""
    empty = {"support": [], "resistance": []}
    for direction in analyst.DIRECTIONS:
        assert analyst.unserved_numbers(analyst._server_invalidation(direction), empty) == []
        for branch in analyst.DIRECTION_BRANCHES:
            line = analyst._server_thesis(direction, branch, "5 trading days")
            assert analyst.unserved_numbers(line, empty) == [], (direction, branch, line)
            assert not analyst.banned_analyst_hits({"thesis": line}), (direction, branch)


def test_the_audit_copies_are_never_swept_for_numbers():
    """`modelThesis` and `modelInvalidation` are records of what the model said. Sanitising them
    would destroy their only purpose, so binding 8 exempts them by name — asserted here because an
    over-eager future sweep would look like a safety improvement."""
    ctx = {"ticker": "NVDA", "news": [{"headline": "x"}]}
    res = analyst.run_analyst("NVDA", ctx,
                              fake_model(_reply(invalidation=MODEL_INVALIDATION)),
                              safe_json, as_of=None, horizons=["5d"])
    obj = res["theses"]["5d"]

    assert obj["modelInvalidation"] == MODEL_INVALIDATION       # the price is still on the record
    assert not any("173.80" in w and "served keyLevels" in w for w in res["warnings"])
