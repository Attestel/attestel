"""Scenario mode — structured what-if reasoning ("what if the US bans H20 exports to China?").

The model builds a qualitative cause-and-effect chain grounded in the supplied context (the
supply-chain tracker, roadmap, risks, regime, news). This is the feature most tempted to slide
into fortune-telling, so the guardrails are explicit:

  * HYPOTHETICAL, not prediction — the output opens with assumptions and is labelled throughout;
  * qualitative only — no price targets, no return numbers, no probabilities dressed as facts;
  * grounded — effects should trace to supplied facts (suppliers, roadmap, risks) where possible;
  * BANNED-list validated; deterministic stub offline.
"""
from __future__ import annotations

import json

from .prompt import BANNED

SCENARIO_DISCLAIMER = (
    "Hypothetical scenario reasoning — a structured thought experiment over the supplied facts, "
    "NOT a prediction, forecast, or investment advice."
)

_SYSTEM = (
    "You are a scenario analyst running structured thought experiments, NOT a financial advisor "
    "and NOT a forecaster. You reason qualitatively about how a hypothetical event could propagate "
    "through a company's supply chain, product roadmap, and market position. You never give "
    "buy/sell recommendations, price targets, return estimates, or numeric probabilities. Where "
    "the supplied facts name specific suppliers, products, or risks, anchor your reasoning to "
    "them. Uncertainty language is mandatory: could, might, historically, one path is. Reply with "
    "a single JSON object and nothing else."
)

SCHEMA = {
    "scenario": "string — restate the hypothetical in one line",
    "assumptions": ["string (2-4 assumptions this thought experiment makes)"],
    "firstOrder": ["string (direct effects, anchored to supplied facts where possible)"],
    "secondOrder": ["string (knock-on effects: supply chain, competitors, customers)"],
    "mitigants": ["string (what could soften the impact — alternatives, existing plans)"],
    "horizons": {"shortTerm": "string (weeks-months, qualitative)",
                 "longTerm": "string (quarters-years, qualitative)"},
    "watchFor": ["string (2-4 observable events that would confirm or kill this scenario)"],
    "summary": "string (2-3 sentences, hypothetical framing throughout)",
}

KEYS = ["scenario", "assumptions", "firstOrder", "secondOrder", "mitigants", "horizons", "watchFor", "summary"]


def build_scenario_prompt(ticker: str, question: str, ctx: dict, firmer: bool = False) -> list[dict]:
    firm = ""
    if firmer:
        firm = ("\n\nIMPORTANT: Return ONLY a JSON object with EXACTLY these keys: scenario, "
                "assumptions, firstOrder, secondOrder, mitigants, horizons, watchFor, summary.")
    user = (
        f"TICKER: {ticker}\n\n"
        f"USER'S WHAT-IF QUESTION:\n\"\"\"\n{question.strip()[:1500]}\n\"\"\"\n\n"
        f"GROUNDING FACTS (supply chain, roadmap, risks, current regime, news):\n"
        f"{json.dumps(ctx, indent=2)}\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n"
        f"{json.dumps(SCHEMA, indent=2)}\n\n"
        "Qualitative reasoning only — no price levels, no return numbers, no percent probabilities. "
        "This is a thought experiment, and it must read like one." + firm
    )
    return [{"role": "system", "content": _SYSTEM}, {"role": "user", "content": user}]


def validate_scenario(obj: dict | None) -> list[str]:
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    warnings = [f"missing key: {k}" for k in KEYS if k not in obj]
    if not isinstance(obj.get("horizons"), dict):
        warnings.append("horizons is not an object")
    blob = json.dumps(obj).lower()
    warnings.extend(f"contains advice phrase: '{b}'" for b in BANNED if b in blob)
    return warnings


def needs_scenario_retry(warnings: list[str]) -> bool:
    return any(w.startswith(("missing key", "output is not")) or w == "horizons is not an object"
               for w in warnings)


def stub_scenario(ticker: str, question: str, ctx: dict) -> dict:
    suppliers = [s.get("name") for s in (ctx.get("suppliers") or []) if isinstance(s, dict) and s.get("name")]
    risks = [r.get("risk") or r.get("name") or str(r) for r in (ctx.get("risks") or [])][:3]
    return {
        "scenario": question.strip()[:200] or "unspecified scenario",
        "assumptions": ["Model offline — no reasoning was performed; this is a fact listing."],
        "firstOrder": [f"Known supply-chain dependencies on record: {', '.join(suppliers[:5]) or 'none in context'}."],
        "secondOrder": [],
        "mitigants": [],
        "horizons": {"shortTerm": "Not analyzed (model offline).",
                     "longTerm": "Not analyzed (model offline)."},
        "watchFor": risks or ["Reconnect the model to run the scenario."],
        "summary": f"The scenario for {ticker} was not analyzed — the model is offline. The tracked "
                   "supply-chain facts are listed for reference only.",
        "_stub": True,
    }


def run_scenario(ticker: str, question: str, ctx: dict, call_model, safe_json) -> dict:
    raw, model_used = call_model(build_scenario_prompt(ticker, question, ctx))
    obj = safe_json(raw)
    warnings = validate_scenario(obj)
    retried = False
    if not model_used.startswith("stub:") and needs_scenario_retry(warnings):
        retried = True
        raw, model_used = call_model(build_scenario_prompt(ticker, question, ctx, firmer=True))
        obj = safe_json(raw)
        warnings = validate_scenario(obj)
    if obj is None or needs_scenario_retry(warnings) or any("advice phrase" in w for w in warnings):
        obj = stub_scenario(ticker, question, ctx)
        if not model_used.startswith("stub:"):
            model_used = f"{model_used} (fell back to stub)"
        warnings = validate_scenario(obj)
    return {
        "ticker": ticker, "question": question, "structured": obj,
        "modelUsed": model_used, "warnings": warnings, "retried": retried,
        "disclaimer": SCENARIO_DISCLAIMER,
    }
