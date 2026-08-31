"""Brief prompt/validation/stub tests — grounded + non-prescriptive discipline (no network, no model).

Run:  cd services/llm && python -m pytest -q tests/test_brief.py
"""
from app.brief import (
    BRIEF_REQUIRED_KEYS,
    build_brief_prompt,
    needs_brief_retry,
    stub_brief,
    validate_brief,
)

NVDA_CONTEXT = {
    "ticker": "NVDA",
    "price": {"last": 196.9, "changePct": 1.4, "source": "yfinance"},
    "regime": {"trend": "up", "momentum": "positive", "volatility": "normal"},
    "confluence": {"trendAlignment": "aligned", "note": "1D/1H agree on up", "conflicts": []},
    "news": [{"headline": "NVIDIA unveils new chip", "source": "Reuters", "sentiment": "positive"}],
    "filingsToday": [{"date": "2026-07-08", "headline": "8-K filed — Results of Operations"}],
    "earnings": {"label": "Q1 FY26", "beat": True, "surprisePercentage": 5.2},
    "sentiment": {"available": True, "label": "bullish", "buzz": 30, "spike": False},
}


def test_prompt_is_grounded_and_forbids_advice():
    msgs = build_brief_prompt("NVDA", NVDA_CONTEXT)
    assert msgs[0]["role"] == "system" and msgs[1]["role"] == "user"
    system = msgs[0]["content"].lower()
    assert "never" in system and "buy or sell" in system
    assert "only reference numbers and events that appear in the facts" in system
    user = msgs[1]["content"]
    # the facts are embedded so the model can only quote what it's given
    assert "196.9" in user and "NVIDIA unveils new chip" in user
    # every schema key the model must emit is named in the prompt
    for k in BRIEF_REQUIRED_KEYS:
        assert k in user


def test_market_prompt_addresses_the_market():
    msgs = build_brief_prompt("MARKET", {"scope": "MARKET", "tickers": []})
    assert "the overall market" in msgs[1]["content"]


def test_validate_accepts_a_well_formed_brief():
    good = {
        "headline": "NVDA up on chip news",
        "whatHappened": ["Closed +1.4%"],
        "whyItMoved": ["New chip announced"],
        "calendar": [{"date": "2026-07-08", "event": "8-K filed"}],
        "watch": ["Follow-through above resistance"],
        "riskFlags": [],
        "summary": "A factual digest. Not advice.",
    }
    assert validate_brief(good) == []
    assert needs_brief_retry(validate_brief(good)) is False


def test_validate_flags_missing_keys_and_triggers_retry():
    warnings = validate_brief({"headline": "hi"})
    assert any(w.startswith("missing key") for w in warnings)
    assert needs_brief_retry(warnings) is True
    # not-an-object is also a structural (retryable) failure
    assert needs_brief_retry(validate_brief(None)) is True


def test_validate_catches_sneaked_in_advice():
    bad = {
        "headline": "h", "whatHappened": ["x"], "whyItMoved": [], "calendar": [],
        "watch": ["w"], "riskFlags": [], "summary": "You should buy this now — strong buy.",
    }
    warnings = validate_brief(bad)
    assert any("advice phrase" in w for w in warnings)
    # advice is a SOFT warning (flagged), not a structural retry
    assert needs_brief_retry(warnings) is False


def test_stub_is_grounded_valid_and_non_prescriptive():
    stub = stub_brief("NVDA", NVDA_CONTEXT)
    assert stub["_stub"] is True
    assert validate_brief(stub) == []  # the stub must itself pass validation (no advice, all keys)
    blob = " ".join(str(v) for v in stub.values()).lower()
    # grounded: it references supplied facts, and it does NOT invent a recommendation
    assert "1.4" in " ".join(stub["whatHappened"])
    assert "buy" not in blob and "sell" not in blob and "price target" not in blob
    # the today filing lands in the calendar
    assert stub["calendar"] and stub["calendar"][0]["date"] == "2026-07-08"


def test_stub_market_scope_lists_movers():
    ctx = {
        "scope": "MARKET",
        "indices": [{"ticker": "SPY", "changePct": 0.6}],
        "tickers": [{"ticker": "NVDA", "changePct": 1.4, "trend": "up", "sentiment": "bullish"}],
    }
    stub = stub_brief("MARKET", ctx)
    assert validate_brief(stub) == []
    joined = " ".join(stub["whatHappened"])
    assert "SPY" in joined and "NVDA" in joined


def test_stub_handles_empty_context_gracefully():
    stub = stub_brief("NVDA", {})
    assert validate_brief(stub) == []
    assert stub["whatHappened"]  # never empty (guarded)
