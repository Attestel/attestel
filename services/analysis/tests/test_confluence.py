"""Multi-timeframe confluence tests — deterministic, offline. Confluence is descriptive only;
these also assert it never emits advice language."""
from app.confluence import build_confluence


def _reg(trend, momentum="neutral (RSI 50)", strength="strong (ADX 30)"):
    return {
        "trend": trend,
        "trendStrength": strength,
        "momentum": momentum,
        "stochastic": "neutral (%K 50, %D 50)",
    }


def test_aligned_up():
    c = build_confluence({"1D": _reg("uptrend"), "1H": _reg("uptrend"), "15m": _reg("uptrend")})
    assert c["trendAlignment"] == "aligned up"
    assert c["alignmentScore"] == 3
    assert c["conflicts"] == []
    assert c["timeframes"] == ["1D", "1H", "15m"]  # higher timeframe first


def test_aligned_down():
    c = build_confluence({"1D": _reg("downtrend"), "1H": _reg("downtrend")})
    assert c["trendAlignment"] == "aligned down"
    assert c["alignmentScore"] == -2


def test_trend_conflict_listed():
    c = build_confluence({"1D": _reg("uptrend"), "1H": _reg("uptrend"), "15m": _reg("downtrend")})
    assert c["trendAlignment"] == "mixed"
    assert c["alignmentScore"] == 1  # 2 up, 1 down
    assert any("1D" in x and "15m" in x for x in c["conflicts"])


def test_momentum_conflict_and_flags():
    c = build_confluence({
        "1D": _reg("uptrend", momentum="neutral (RSI 55)"),
        "15m": _reg("uptrend", momentum="overbought (RSI 82)"),
    })
    assert "15m overbought" in c["momentumFlags"]
    assert any("overbought" in x for x in c["conflicts"])


def test_note_is_descriptive_not_advice():
    c = build_confluence({"1D": _reg("uptrend"), "15m": _reg("uptrend", momentum="overbought (RSI 82)")})
    note = c["note"].lower()
    for banned in ("buy", "sell", "recommend", "you should", "price target"):
        assert banned not in note
    assert "uptrend" in note and "overbought" in note


def test_insufficient_history_is_neutral():
    # a timeframe with no clear trend doesn't create a false conflict
    c = build_confluence({"1D": _reg("uptrend"), "15m": _reg("insufficient history for SMA200")})
    assert c["conflicts"] == []
    assert c["trendAlignment"] == "aligned up"


def test_empty_regimes():
    c = build_confluence({})
    assert c["timeframes"] == []
    assert c["trendAlignment"] == "no clear trend"
    assert "No timeframe data" in c["note"]
