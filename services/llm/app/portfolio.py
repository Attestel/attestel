"""Portfolio review and scenario explanation over deterministic server context.

The model never performs portfolio math. The journal service supplies weights, exposures,
concentration, risk, target states, policy findings, and attached ticker-thesis context. This module
only turns those validated facts into a compact explanation after an explicit user action.
"""
from __future__ import annotations

import json
import re

from .prompt import BANNED


PORTFOLIO_REVIEW_DISCLAIMER = (
    "AI explanation of server-calculated portfolio research context. Not investment advice, an "
    "allocation recommendation, or a trade instruction. No order is created or executed."
)
PORTFOLIO_SCENARIO_DISCLAIMER = (
    "Hypothetical portfolio-exposure reasoning over current recorded holdings. Not a forecast, "
    "probability, allocation recommendation, or trade instruction."
)

_ADVICE_RE = re.compile(
    r"\b(?:buy|sell|hold)\b|\b(?:increase|decrease|reduce|add to|trim)\s+(?:the|your|this)?\s*position\b|"
    r"\brebalance\s+(?:into|out of|toward|away from)\b|\bposition\s+size\b",
    re.IGNORECASE,
)
_NUMBER_RE = re.compile(r"(?<![A-Za-z])[-+]?\d+(?:[.,]\d+)?%?")

REVIEW_KEYS = ["posture", "supports", "threats", "invalidations", "attention", "summary"]
REVIEW_SCHEMA = {
    "posture": "string — descriptive portfolio posture with no numeric values",
    "supports": ["string — fact-grounded support; no numeric values"],
    "threats": ["string — fact-grounded threat; no numeric values"],
    "invalidations": ["string — observable context change that would invalidate this review"],
    "attention": [{
        "subject": "string — a held ticker, sector, CASH, or PORTFOLIO",
        "reason": "string — why this supplied finding deserves review; no action instruction",
    }],
    "summary": "string — concise descriptive review, no recommendation or numeric values",
}

SCENARIO_KEYS = [
    "scenario", "overallExposure", "mostExposed", "secondaryEffects", "mitigants",
    "uncertainties", "invalidations", "summary",
]
SCENARIO_SCHEMA = {
    "scenario": "string — restate the user's hypothetical",
    "overallExposure": "one of: limited | moderate | broad | unclear",
    "mostExposed": [{
        "ticker": "string — MUST be a ticker in context.positions",
        "mechanism": "string — qualitative mechanism from supplied context; no numeric values",
    }],
    "secondaryEffects": ["string — qualitative knock-on effect; no numeric values"],
    "mitigants": ["string — supplied characteristic that could soften exposure"],
    "uncertainties": ["string — what the supplied context cannot establish"],
    "invalidations": ["string — observable condition that would break the scenario chain"],
    "summary": "string — hypothetical framing throughout; no forecast or recommendation",
}


_REVIEW_SYSTEM = (
    "You explain a portfolio state that was CALCULATED BY CODE. You are not a portfolio calculator, "
    "financial adviser, allocation engine, or trader. Never recompute or introduce a number. Never "
    "say buy, sell, hold, increase, reduce, trim, add to, position size, or recommend a rebalance. "
    "Describe what supports the recorded portfolio context, what threatens it, what would invalidate "
    "the current interpretation, and which supplied findings deserve the user's review. Use only the "
    "supplied context. Reply with one JSON object and nothing else."
)

_SCENARIO_SYSTEM = (
    "You run a qualitative thought experiment over a portfolio state CALCULATED BY CODE. This is not "
    "a forecast or advice. Never recompute or introduce a number, probability, price, target, position "
    "size, allocation, or transaction. Never say buy, sell, hold, increase, reduce, trim, add to, or "
    "recommend a rebalance. Every ticker in mostExposed must already appear in context.positions. "
    "Use uncertainty language and supplied facts only. Reply with one JSON object and nothing else."
)


def build_portfolio_review_prompt(context: dict, firmer: bool = False) -> list[dict]:
    firm = ""
    if firmer:
        firm = (
            "\n\nYour previous reply was unsafe or invalid. Return ONLY the exact schema. "
            "Use no digits and no allocation or trade language."
        )
    user = (
        "SERVER-CALCULATED PORTFOLIO CONTEXT (the only source of truth):\n"
        f"{json.dumps(context, indent=2)}\n\n"
        f"Return exactly this schema:\n{json.dumps(REVIEW_SCHEMA, indent=2)}\n\n"
        "Do not repeat exact weights or risk numbers in prose; the UI renders those server fields "
        "separately. Explain relationships and attention only." + firm
    )
    return [{"role": "system", "content": _REVIEW_SYSTEM}, {"role": "user", "content": user}]


def build_portfolio_scenario_prompt(question: str, context: dict, firmer: bool = False) -> list[dict]:
    firm = ""
    if firmer:
        firm = (
            "\n\nYour previous reply was unsafe or invalid. Return ONLY the exact schema. Use only "
            "held tickers in mostExposed, no digits, and no allocation or trade language."
        )
    user = (
        f"USER'S HYPOTHETICAL:\n\"\"\"\n{question.strip()[:1500]}\n\"\"\"\n\n"
        "SERVER-CALCULATED PORTFOLIO CONTEXT (the only source of truth):\n"
        f"{json.dumps(context, indent=2)}\n\n"
        f"Return exactly this schema:\n{json.dumps(SCENARIO_SCHEMA, indent=2)}\n\n"
        "The scenario field may repeat the user's wording. Every other prose field must avoid "
        "numeric claims because the interface renders deterministic numbers separately." + firm
    )
    return [{"role": "system", "content": _SCENARIO_SYSTEM}, {"role": "user", "content": user}]


def _prose_strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, list):
        for item in value:
            yield from _prose_strings(item)
    elif isinstance(value, dict):
        for key, item in value.items():
            # Identifiers can legitimately contain digits (for example, ticker symbols). The
            # no-number rule applies to model-authored explanatory prose, not server identities.
            if key in {"ticker", "subject"}:
                continue
            yield from _prose_strings(item)


def _unsafe_language(obj: dict, *, exclude: set[str] | None = None) -> list[str]:
    exclude = exclude or set()
    warnings: list[str] = []
    reduced = {key: value for key, value in obj.items() if key not in exclude}
    blob = json.dumps(reduced).lower()
    warnings.extend(f"contains advice phrase: '{phrase}'" for phrase in BANNED if phrase in blob)
    if _ADVICE_RE.search(blob):
        warnings.append("contains allocation or trade instruction")
    for text in _prose_strings(reduced):
        if _NUMBER_RE.search(text):
            warnings.append("contains a numeric prose claim")
            break
    return warnings


def _validate_string_array(obj: dict, key: str, warnings: list[str]) -> None:
    if key not in obj:
        return
    if not isinstance(obj[key], list):
        warnings.append(f"{key} is not an array")
        return
    if any(not isinstance(item, str) for item in obj[key]):
        warnings.append(f"{key} contains a non-string item")


def validate_portfolio_review(obj: dict | None) -> list[str]:
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    warnings = [f"missing key: {key}" for key in REVIEW_KEYS if key not in obj]
    for key in ("supports", "threats", "invalidations"):
        _validate_string_array(obj, key, warnings)
    if "attention" in obj and not isinstance(obj["attention"], list):
        warnings.append("attention is not an array")
    if "posture" in obj and not isinstance(obj["posture"], str):
        warnings.append("posture is not a string")
    if "summary" in obj and not isinstance(obj["summary"], str):
        warnings.append("summary is not a string")
    for item in obj.get("attention") or []:
        if not isinstance(item, dict) or not isinstance(item.get("subject"), str) or not isinstance(item.get("reason"), str):
            warnings.append("attention item is invalid")
            break
    warnings.extend(_unsafe_language(obj))
    return warnings


def _held_tickers(context: dict) -> set[str]:
    return {
        str(position.get("ticker") or "").upper()
        for position in (context.get("positions") or [])
        if isinstance(position, dict) and position.get("ticker")
    }


def validate_portfolio_scenario(obj: dict | None, context: dict) -> list[str]:
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    warnings = [f"missing key: {key}" for key in SCENARIO_KEYS if key not in obj]
    if obj.get("overallExposure") not in {"limited", "moderate", "broad", "unclear"}:
        warnings.append("invalid overallExposure")
    if "scenario" in obj and not isinstance(obj["scenario"], str):
        warnings.append("scenario is not a string")
    if "summary" in obj and not isinstance(obj["summary"], str):
        warnings.append("summary is not a string")
    if "mostExposed" in obj and not isinstance(obj["mostExposed"], list):
        warnings.append("mostExposed is not an array")
    for key in ("secondaryEffects", "mitigants", "uncertainties", "invalidations"):
        _validate_string_array(obj, key, warnings)
    held = _held_tickers(context)
    for item in obj.get("mostExposed") or []:
        if (
            not isinstance(item, dict)
            or not isinstance(item.get("ticker"), str)
            or not isinstance(item.get("mechanism"), str)
        ):
            warnings.append("mostExposed item is invalid")
            continue
        if str(item.get("ticker") or "").upper() not in held:
            warnings.append(f"mostExposed ticker is not held: {item.get('ticker')!r}")
    warnings.extend(_unsafe_language(obj, exclude={"scenario"}))
    return warnings


def needs_portfolio_retry(warnings: list[str]) -> bool:
    return bool(warnings)


def stub_portfolio_review(context: dict) -> dict:
    positions = context.get("positions") or []
    findings = context.get("findings") or []
    active_theses = [p for p in positions if isinstance(p, dict) and p.get("thesis")]
    supports = []
    if active_theses:
        supports.append("At least one holding has active thesis context attached.")
    if context.get("cashWeight") is not None:
        supports.append("A recorded cash balance is included in the portfolio state.")
    if not supports:
        supports.append("The deterministic portfolio record is available for review.")
    threats = [str(f.get("summary")) for f in findings if isinstance(f, dict) and f.get("summary")]
    if not threats:
        threats = ["No user-policy or target-range finding is currently open."]
    attention = [
        {"subject": str(f.get("subject") or "PORTFOLIO"), "reason": str(f.get("summary"))}
        for f in findings if isinstance(f, dict) and f.get("summary")
    ]
    return {
        "posture": "Deterministic context is available; interpretive review was not run because the model is offline.",
        "supports": supports,
        "threats": threats,
        "invalidations": ["A change in holdings, policy, price context, risk coverage, or an attached thesis would invalidate this cached review."],
        "attention": attention,
        "summary": "Model offline. This is a deterministic fact summary, not an AI portfolio interpretation.",
        "_stub": True,
    }


def stub_portfolio_scenario(question: str, context: dict) -> dict:
    positions = [p for p in (context.get("positions") or []) if isinstance(p, dict) and p.get("ticker")]
    positions.sort(key=lambda p: (-float(p.get("weight") or 0), str(p.get("ticker"))))
    exposed = []
    if positions:
        exposed.append({
            "ticker": str(positions[0]["ticker"]).upper(),
            "mechanism": "This is the largest recorded holding; no qualitative model reasoning was available.",
        })
    return {
        "scenario": question.strip()[:300] or "Unspecified hypothetical",
        "overallExposure": "unclear",
        "mostExposed": exposed,
        "secondaryEffects": [],
        "mitigants": [],
        "uncertainties": ["The model is offline, so causal mechanisms were not evaluated."],
        "invalidations": ["Reconnect the model and rerun the explicit scenario review."],
        "summary": "Model offline. Recorded holdings are listed without a forecast or recommendation.",
        "_stub": True,
    }


def run_portfolio_review(context: dict, call_model, safe_json) -> dict:
    raw, model_used = call_model(build_portfolio_review_prompt(context))
    obj = safe_json(raw)
    warnings = validate_portfolio_review(obj)
    retried = False
    if not model_used.startswith("stub:") and needs_portfolio_retry(warnings):
        retried = True
        raw, model_used = call_model(build_portfolio_review_prompt(context, firmer=True))
        obj = safe_json(raw)
        warnings = validate_portfolio_review(obj)
    if obj is None or needs_portfolio_retry(warnings):
        obj = stub_portfolio_review(context)
        if not model_used.startswith("stub:"):
            model_used = f"{model_used} (fell back to stub)"
        warnings = validate_portfolio_review(obj)
    return {
        "contextVersion": context.get("contextVersion"),
        "structured": obj,
        "modelUsed": model_used,
        "warnings": warnings,
        "retried": retried,
        "disclaimer": PORTFOLIO_REVIEW_DISCLAIMER,
    }


def run_portfolio_scenario(question: str, context: dict, call_model, safe_json) -> dict:
    raw, model_used = call_model(build_portfolio_scenario_prompt(question, context))
    obj = safe_json(raw)
    warnings = validate_portfolio_scenario(obj, context)
    retried = False
    if not model_used.startswith("stub:") and needs_portfolio_retry(warnings):
        retried = True
        raw, model_used = call_model(build_portfolio_scenario_prompt(question, context, firmer=True))
        obj = safe_json(raw)
        warnings = validate_portfolio_scenario(obj, context)
    if obj is None or needs_portfolio_retry(warnings):
        obj = stub_portfolio_scenario(question, context)
        if not model_used.startswith("stub:"):
            model_used = f"{model_used} (fell back to stub)"
        warnings = validate_portfolio_scenario(obj, context)
    return {
        "contextVersion": context.get("contextVersion"),
        "question": question,
        "structured": obj,
        "modelUsed": model_used,
        "warnings": warnings,
        "retried": retried,
        "disclaimer": PORTFOLIO_SCENARIO_DISCLAIMER,
    }
