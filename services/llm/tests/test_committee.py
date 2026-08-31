"""committee.py — deterministic tests (no model, no network): stubs, validation, consensus math,
and the invariant that no committee output can carry buy/sell advice.

Run:  cd services/llm && python -m pytest -q tests/test_committee.py
"""
import json

from app.committee import (
    STANCES,
    compute_consensus,
    feature_vector,
    needs_agent_retry,
    run_committee,
    stub_analyst,
    stub_debater,
    stub_judge,
    validate_analyst,
    validate_debater,
    validate_judge,
)

CTX = {
    "regime": {
        "trend": "uptrend", "momentum": "overbought", "macdState": "bullish cross",
        "volume": "above average", "keyLevels": {"support": [900.0], "resistance": [1000.0]},
    },
    "earnings": [{"surprise": 0.12}, {"surprise": 0.08}, {"surprise": -0.02}, {"surprise": 0.05}],
    "news": [{"headline": "x", "sentiment": 0.4}, {"headline": "y", "sentiment": 0.2}],
}


def test_stub_analysts_are_valid_and_deterministic():
    for agent in ("technical", "fundamentals", "news"):
        a = stub_analyst(agent, CTX)
        b = stub_analyst(agent, CTX)
        assert a == b  # deterministic
        assert a["stance"] in STANCES
        assert 0.0 <= a["confidence"] <= 1.0
        assert validate_analyst(a) == []


def test_stub_analysts_lean_neutral_on_empty_context():
    for agent in ("technical", "fundamentals", "news"):
        a = stub_analyst(agent, {})
        assert a["stance"] == "neutral"
        assert a["confidence"] <= 0.2


def test_stub_debate_and_judge_validate():
    analysts = {a: stub_analyst(a, CTX) for a in ("technical", "fundamentals", "news")}
    debate = {s: stub_debater(s, analysts) for s in ("bull", "bear")}
    assert validate_debater(debate["bull"]) == []
    assert validate_debater(debate["bear"]) == []
    assert validate_judge(stub_judge(analysts, debate)) == []


def test_consensus_is_a_reproducible_weighted_vote():
    analysts = {
        "technical": {"stance": "bullish", "confidence": 0.8},
        "fundamentals": {"stance": "bullish", "confidence": 0.4},
        "news": {"stance": "bearish", "confidence": 0.6},
    }
    c = compute_consensus(analysts)
    assert c["score"] == round((0.8 + 0.4 - 0.6) / 3, 4)
    assert c["agreement"] == round(2 / 3, 4)
    assert c["label"] == "bullish-lean"
    fv = feature_vector(analysts, c)
    assert fv["technical"] == 0.8 and fv["news"] == -0.6
    assert fv["consensus"] == c["score"]


def test_consensus_handles_no_stances():
    c = compute_consensus({})
    assert c == {"score": 0.0, "label": "no-stance", "agreement": 0.0, "meanConfidence": 0.0, "n": 0}


def test_validation_flags_advice_and_structure():
    bad = {"stance": "buy", "confidence": 2.0, "reasoning": [], "watchFor": "strong buy now"}
    w = validate_analyst(bad)
    assert any("invalid stance" in x for x in w)
    assert any("invalid confidence" in x for x in w)
    assert any("advice phrase" in x for x in w)
    assert needs_agent_retry(w)  # structural -> retry
    assert not needs_agent_retry(["technical: contains advice phrase: 'strong buy'"])


def test_run_committee_offline_is_fully_stubbed_and_advice_free():
    """The zero-network path: every provider down -> every agent stubs, nothing errors, and the
    full transcript contains no banned advice phrase (invariant #2)."""
    def offline_call(_messages):
        return "", "stub:offline"

    def sj(raw):
        try:
            return json.loads(raw)
        except Exception:  # noqa: BLE001
            return None

    out = run_committee("NVDA", CTX, offline_call, sj)
    assert set(out["analysts"]) == {"technical", "fundamentals", "news"}
    assert all(v["_stub"] for v in out["analysts"].values())
    assert out["debate"]["bull"]["_stub"] and out["judge"]["_stub"]
    assert out["consensus"]["n"] == 3
    assert set(out["featureVector"]) == {
        "technical", "fundamentals", "news", "consensus", "agreement", "meanConfidence"
    }
    blob = json.dumps(out).lower()
    from app.prompt import BANNED
    assert not any(b in blob for b in BANNED)


def test_run_committee_replaces_advice_with_stub():
    """A model that sneaks advice through gets swapped for the deterministic stub."""
    def bad_model(_messages):
        return json.dumps({
            "stance": "bullish", "confidence": 0.9,
            "reasoning": ["strong buy, you should buy this dip"],
            "watchFor": "nothing", "arguments": ["strong buy"], "rebuttals": [],
            "strongestBull": ["strong buy"], "strongestBear": [], "unresolved": [], "summary": "buy",
        }), "qwen3-14b"

    out = run_committee("NVDA", CTX, bad_model, lambda r: json.loads(r))
    blob = json.dumps(out).lower()
    from app.prompt import BANNED
    assert not any(b in blob for b in BANNED)
    assert all(v.get("_stub") for v in out["analysts"].values())
