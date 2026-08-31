"""Thesis tracker — evaluate the USER'S stated investment thesis against newly collected facts.

The verdict is about the THESIS, never the stock: "the evidence currently supports / is neutral
toward / challenges your stated hypothesis". The model must cite which supplied fact drives each
piece of evidence — it judges the user's reasoning against the record, it does not advise.
Deterministic stub offline; BANNED-list validated like every other model output.
"""
from __future__ import annotations

import json

from .prompt import BANNED

VERDICTS = ("confirming", "neutral", "challenged")

THESIS_DISCLAIMER = (
    "AI comparison of YOUR thesis against collected facts — an evidence check, not investment "
    "advice. Whether to act on it stays with you."
)

_SYSTEM = (
    "You evaluate an investor's OWN stated thesis against newly collected facts. You are NOT a "
    "financial advisor: you never say buy, sell, hold, or give targets. Your verdict describes "
    "whether the EVIDENCE supports the user's hypothesis, nothing more. Every evidence item must "
    "reference a fact from the supplied context — never invent events or numbers. Reply with a "
    "single JSON object and nothing else."
)

SCHEMA = {
    "verdict": "one of: confirming | neutral | challenged",
    "confidence": "number 0..1 — how clearly the evidence points either way",
    "evidence": [{
        "fact": "string — the specific supplied fact",
        "bearing": "one of: supports | contradicts | context",
        "note": "string — why it bears on the thesis",
    }],
    "watchFor": ["string (1-3 upcoming facts that would settle the question)"],
    "summary": "string (2-3 sentences on the state of the thesis — descriptive)",
}

KEYS = ["verdict", "confidence", "evidence", "watchFor", "summary"]


def build_thesis_prompt(ticker: str, thesis: str, ctx: dict, firmer: bool = False) -> list[dict]:
    firm = ""
    if firmer:
        firm = ("\n\nIMPORTANT: Return ONLY a JSON object with EXACTLY these keys: verdict, "
                "confidence, evidence, watchFor, summary. verdict must be confirming, neutral, "
                "or challenged.")
    user = (
        f"TICKER: {ticker}\n\n"
        f"THE USER'S THESIS (their own words):\n\"\"\"\n{thesis.strip()[:2000]}\n\"\"\"\n\n"
        f"NEWLY COLLECTED FACTS (your ONLY evidence):\n{json.dumps(ctx, indent=2)}\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n"
        f"{json.dumps(SCHEMA, indent=2)}\n\n"
        "If the facts simply don't touch the thesis, say verdict=neutral with low confidence and "
        "say so plainly. Judge the thesis, not the stock." + firm
    )
    return [{"role": "system", "content": _SYSTEM}, {"role": "user", "content": user}]


def validate_thesis_check(obj: dict | None) -> list[str]:
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    warnings = [f"missing key: {k}" for k in KEYS if k not in obj]
    if obj.get("verdict") not in VERDICTS:
        warnings.append(f"invalid verdict: {obj.get('verdict')!r}")
    conf = obj.get("confidence")
    if not isinstance(conf, (int, float)) or not (0.0 <= float(conf) <= 1.0):
        warnings.append("invalid confidence")
    blob = json.dumps(obj).lower()
    warnings.extend(f"contains advice phrase: '{b}'" for b in BANNED if b in blob)
    return warnings


def needs_thesis_retry(warnings: list[str]) -> bool:
    return any(w.startswith(("missing key", "output is not", "invalid verdict")) for w in warnings)


def stub_thesis_check(ticker: str, thesis: str, ctx: dict) -> dict:
    n_news = len(ctx.get("news") or [])
    return {
        "verdict": "neutral",
        "confidence": 0.0,
        "evidence": [{
            "fact": f"{n_news} headlines and the computed regime were collected, but the model is offline.",
            "bearing": "context",
            "note": "No language analysis was possible; nothing here supports or contradicts the thesis.",
        }],
        "watchFor": ["Re-run the check when the model is reachable."],
        "summary": f"Model offline — the {ticker} thesis was not evaluated. This neutral verdict "
                   "carries no information.",
        "_stub": True,
    }


def run_thesis_check(ticker: str, thesis: str, ctx: dict, call_model, safe_json) -> dict:
    raw, model_used = call_model(build_thesis_prompt(ticker, thesis, ctx))
    obj = safe_json(raw)
    warnings = validate_thesis_check(obj)
    retried = False
    if not model_used.startswith("stub:") and needs_thesis_retry(warnings):
        retried = True
        raw, model_used = call_model(build_thesis_prompt(ticker, thesis, ctx, firmer=True))
        obj = safe_json(raw)
        warnings = validate_thesis_check(obj)
    if obj is None or needs_thesis_retry(warnings) or any("advice phrase" in w for w in warnings):
        obj = stub_thesis_check(ticker, thesis, ctx)
        if not model_used.startswith("stub:"):
            model_used = f"{model_used} (fell back to stub)"
        warnings = validate_thesis_check(obj)
    if isinstance(obj.get("confidence"), (int, float)):
        obj["confidence"] = max(0.0, min(1.0, float(obj["confidence"])))
    return {
        "ticker": ticker, "structured": obj, "modelUsed": model_used,
        "warnings": warnings, "retried": retried, "disclaimer": THESIS_DISCLAIMER,
    }
