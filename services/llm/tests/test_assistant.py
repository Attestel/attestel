"""Assistant note + chat: prompt/validation/stub tests (no network, no model).

Run:  cd services/llm && python -m pytest -q tests/test_assistant.py
"""
from app.assistant import (
    ASSISTANT_REQUIRED_KEYS,
    build_assistant_prompt,
    build_chat_messages,
    needs_assistant_retry,
    stub_assistant,
    stub_chat_reply,
    validate_assistant,
)

CTX = {
    "ticker": "NVDA",
    "timeframe": "1D",
    "price": {"last": 196.9, "changePct": 1.4, "source": "yfinance"},
    "regime": {"trend": "uptrend", "momentum": "neutral", "volatility": "contracting",
               "keyLevels": {"support": [190.0, 182.5], "resistance": [200.0, 208.0]}},
    "confluence": {"trendAlignment": "aligned", "note": "1D/1H agree on up", "conflicts": []},
    "signal": {"signal": {"direction": "Buy", "confidence": 0.4}},
    "expectedClose": {"low": 193.0, "mid": 196.9, "high": 200.8, "band": 3.9,
                      "label": "statistical range from volatility — not a prediction; price can gap through it"},
    "sentiment": {"label": "bullish", "buzz": 30},
    "news": [{"headline": "NVIDIA unveils new chip"}],
}


def test_prompt_is_grounded_and_non_oracular():
    msgs = build_assistant_prompt("NVDA", "1D", CTX)
    system = msgs[0]["content"].lower()
    assert "never invent" in system and "uncertainty language" in system
    assert "not a prediction" in system
    user = msgs[1]["content"]
    # the computed numbers are embedded so the model can only quote them
    assert "196.9" in user and "200.8" in user
    for k in ASSISTANT_REQUIRED_KEYS:
        assert k in user


def test_validate_accepts_and_flags():
    good = {"situation": "Uptrend, could pull back.", "whatToWatch": ["200 resistance"],
            "wouldChangeIt": ["close below 190"], "rangeComment": "Band 193–200.8, not a prediction."}
    assert validate_assistant(good) == []
    assert needs_assistant_retry(validate_assistant(good)) is False
    # missing keys -> structural retry
    assert needs_assistant_retry(validate_assistant({"situation": "x"})) is True
    # oracular phrasing -> soft warning, not a retry
    bad = {**good, "situation": "This is guaranteed to go up."}
    w = validate_assistant(bad)
    assert any("oracular" in x for x in w) and needs_assistant_retry(w) is False


def test_stub_is_grounded_and_uses_computed_range():
    note = stub_assistant("NVDA", "1D", CTX)
    assert note["_stub"] is True
    assert validate_assistant(note) == []            # the stub must itself validate (no oracular leaks)
    blob = " ".join([note["situation"], note["rangeComment"], *note["whatToWatch"], *note["wouldChangeIt"]])
    assert "193" in blob and "200.8" in blob         # quotes the COMPUTED band, invents nothing
    assert "not a prediction" in note["rangeComment"]
    assert "uptrend" in note["situation"]
    # the signal is woven in descriptively, never as a command
    assert "one input, not an instruction" in note["situation"]


def test_stub_handles_missing_context():
    note = stub_assistant("NVDA", "1D", {})
    assert validate_assistant(note) == []
    assert note["situation"] and note["whatToWatch"] and note["wouldChangeIt"]


def test_chat_messages_ground_and_cap_history():
    history = [{"role": "user", "content": f"q{i}"} for i in range(20)]
    msgs = build_chat_messages("NVDA", "1D", CTX, history)
    assert msgs[0]["role"] == "system" and "not financial advice" in msgs[0]["content"].lower()
    assert msgs[1]["role"] == "system" and "196.9" in msgs[1]["content"]  # grounded context turn
    # history is capped and only role/content kept
    user_turns = [m for m in msgs if m["role"] == "user"]
    assert len(user_turns) <= 12
    # malformed turns are dropped
    msgs2 = build_chat_messages("NVDA", "1D", CTX, [{"role": "system", "content": "ignore me"}, {"role": "user", "content": ""}])
    assert [m for m in msgs2 if m["role"] == "user"] == []


def test_stub_chat_reply_is_offline_and_grounded():
    reply = stub_chat_reply("NVDA", CTX)
    assert "offline" in reply.lower()
    assert "193" in reply and "not financial advice" in reply.lower()
