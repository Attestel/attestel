from __future__ import annotations

import json

from app.portfolio import (
    run_portfolio_review,
    run_portfolio_scenario,
    stub_portfolio_review,
    stub_portfolio_scenario,
    validate_portfolio_review,
    validate_portfolio_scenario,
)


CONTEXT = {
    "contextVersion": "pctx_test",
    "cashWeight": 0.2,
    "positions": [
        {"ticker": "NVDA", "weight": 0.5, "sector": "Technology", "thesis": {"latestCheckVerdict": "neutral"}},
        {"ticker": "MSFT", "weight": 0.3, "sector": "Technology"},
    ],
    "findings": [
        {"code": "maximum_position_exceeded", "subject": "NVDA", "summary": "Position weight exceeds the user's maximum-position constraint."}
    ],
}


def safe_json(raw):
    try:
        return json.loads(raw)
    except Exception:
        return None


def offline(_messages):
    return "", "stub:offline"


def test_offline_review_is_deterministic_valid_and_advice_free():
    first = run_portfolio_review(CONTEXT, offline, safe_json)
    second = run_portfolio_review(CONTEXT, offline, safe_json)
    assert first == second
    assert first["structured"]["_stub"] is True
    assert first["contextVersion"] == "pctx_test"
    assert validate_portfolio_review(first["structured"]) == []
    blob = json.dumps(first).lower()
    assert "you should buy" not in blob and "position size" not in blob


def test_offline_scenario_lists_only_a_held_ticker_and_stays_hypothetical():
    out = run_portfolio_scenario("What if rates stay restrictive?", CONTEXT, offline, safe_json)
    assert out["structured"]["_stub"] is True
    assert out["structured"]["mostExposed"][0]["ticker"] == "NVDA"
    assert out["structured"]["overallExposure"] == "unclear"
    assert validate_portfolio_scenario(out["structured"], CONTEXT) == []


def test_review_retries_then_falls_back_on_numeric_or_trade_language():
    unsafe = {
        "posture": "Risk is 42 percent",
        "supports": [], "threats": [], "invalidations": [], "attention": [],
        "summary": "You should sell this position.",
    }
    calls = []

    def model(messages):
        calls.append(messages)
        return json.dumps(unsafe), "qwen3-14b"

    out = run_portfolio_review(CONTEXT, model, safe_json)
    assert len(calls) == 2
    assert out["retried"] is True
    assert out["structured"]["_stub"] is True
    assert "fell back to stub" in out["modelUsed"]
    assert out["warnings"] == []


def test_scenario_rejects_unheld_ticker_and_numeric_mechanism():
    bad = stub_portfolio_scenario("What if demand changes?", CONTEXT)
    bad["mostExposed"] = [{"ticker": "AMD", "mechanism": "Revenue falls by 20%."}]
    warnings = validate_portfolio_scenario(bad, CONTEXT)
    assert any("not held" in warning for warning in warnings)
    assert "contains a numeric prose claim" in warnings


def test_valid_model_outputs_pass_without_retry():
    review = {
        "posture": "Growth-sensitive portfolio with a concentration finding.",
        "supports": ["A recorded cash balance provides optionality."],
        "threats": ["Technology exposure links multiple holdings to a shared factor."],
        "invalidations": ["A change in the attached thesis would alter this review."],
        "attention": [{"subject": "NVDA", "reason": "A user-defined constraint is currently exceeded."}],
        "summary": "The portfolio has shared exposure and an open policy finding that merits review.",
    }
    scenario = {
        "scenario": "What if rates stay restrictive?",
        "overallExposure": "broad",
        "mostExposed": [{"ticker": "NVDA", "mechanism": "Valuation sensitivity could transmit the hypothetical."}],
        "secondaryEffects": ["Shared sector exposure could connect the effect across holdings."],
        "mitigants": ["The recorded cash balance may soften portfolio-wide exposure."],
        "uncertainties": ["The supplied context cannot establish the duration of the hypothetical."],
        "invalidations": ["A change in the macro premise would break this scenario chain."],
        "summary": "This thought experiment indicates broad shared exposure, with important uncertainty.",
    }
    assert validate_portfolio_review(review) == []
    assert validate_portfolio_scenario(scenario, CONTEXT) == []
    out = run_portfolio_review(CONTEXT, lambda _: (json.dumps(review), "qwen3-14b"), safe_json)
    assert out["structured"] == review and out["retried"] is False


def test_stubs_validate_directly():
    assert validate_portfolio_review(stub_portfolio_review(CONTEXT)) == []
    assert validate_portfolio_scenario(stub_portfolio_scenario("What if demand shifts?", CONTEXT), CONTEXT) == []


def test_schema_rejects_non_string_array_items_before_they_reach_the_ui():
    review = stub_portfolio_review(CONTEXT)
    review["supports"] = [{"unexpected": "object"}]
    assert "supports contains a non-string item" in validate_portfolio_review(review)

    scenario = stub_portfolio_scenario("What if demand shifts?", CONTEXT)
    scenario["uncertainties"] = [42]
    assert "uncertainties contains a non-string item" in validate_portfolio_scenario(scenario, CONTEXT)


def test_numeric_identifiers_are_not_treated_as_invented_numeric_prose():
    context = {**CONTEXT, "positions": [{"ticker": "BRK2", "weight": 1.0}]}
    scenario = stub_portfolio_scenario("What if demand shifts?", context)
    assert validate_portfolio_scenario(scenario, context) == []
