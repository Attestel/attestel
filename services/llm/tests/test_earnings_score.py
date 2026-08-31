"""AI earnings-scorer prompt/validation/stub tests (no network, no model).

Run:  cd services/llm && python -m pytest -q tests/test_earnings_score.py
"""
from app.earnings_score import (
    build_score_prompt,
    needs_score_retry,
    stub_score,
    validate_score,
)


def test_prompt_scopes_to_text_and_forbids_advice():
    msgs = build_score_prompt("NVDA", "2026-05-28", "Revenue beat; company raised guidance.")
    system = msgs[0]["content"].lower()
    assert "only the text" in system and "no price data" in system
    user = msgs[1]["content"]
    assert "raised guidance" in user and "2026-05-28" in user


def test_validate_accepts_well_formed_score():
    good = {"tone": 0.6, "guidanceDirection": "up", "confidence": 0.7, "riskFlags": []}
    assert validate_score(good) == []
    assert needs_score_retry(validate_score(good)) is False


def test_validate_flags_bad_ranges_and_direction():
    assert any("tone" in w for w in validate_score({"tone": 5, "guidanceDirection": "up", "confidence": 0.5, "riskFlags": []}))
    assert any("guidanceDirection" in w for w in validate_score({"tone": 0.1, "guidanceDirection": "maybe", "confidence": 0.5, "riskFlags": []}))
    assert needs_score_retry(validate_score({"tone": 0.1})) is True  # missing keys -> structural retry


def test_stub_reads_tone_and_guidance_from_text():
    up = stub_score("NVDA", "Record revenue, strong growth, beat estimates, raised guidance.")
    assert up["tone"] > 0 and up["guidanceDirection"] == "up"
    assert validate_score(up) == []  # the stub must itself validate (no advice, all keys, in range)

    down = stub_score("NVDA", "Revenue missed, weak demand, lowered guidance, softness ahead.")
    assert down["tone"] < 0 and down["guidanceDirection"] == "down"

    neutral = stub_score("NVDA", "")
    assert neutral["tone"] == 0.0 and neutral["confidence"] == 0.0


def test_stub_surfaces_risk_flags():
    s = stub_score("XYZ", "Company disclosed an SEC investigation and a product recall this quarter.")
    joined = " ".join(s["riskFlags"]).lower()
    assert "investigation" in joined and "recall" in joined
