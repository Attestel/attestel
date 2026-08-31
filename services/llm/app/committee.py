"""Analyst committee — the ai-hedge-fund-style multi-agent pipeline, adapted to this repo's rules.

Pipeline (SEQUENTIAL, one model call at a time — the one-model-at-a-time memory rule):

    technical analyst ─┐
    fundamentals analyst ├─► bull researcher ─► bear researcher ─► judge
    news/sentiment analyst ┘

Each ANALYST reads only its slice of the gateway-assembled facts and returns a structured STANCE
(bullish / neutral / bearish + confidence + reasoning). The bull/bear RESEARCHERS then argue the
strongest case for each side given all three stances; the JUDGE summarizes the strongest points and
what remains unresolved.

Invariant #2 is preserved: a stance is an ANALYTICAL LEAN, never a buy/sell/hold verdict, price
target, or recommendation — the prompts forbid it and every output is scanned against the shared
BANNED list. The only actionable signal in this platform remains the backtest-gated
services/prediction output. The committee CONSENSUS is computed DETERMINISTICALLY here from the
analyst stances (a weighted vote), never by the model, so the number is reproducible.

Hybrid path: each persisted snapshot carries a numeric `featureVector`. The prediction service can
consume that history as candidate features ONCE enough out-of-sample snapshots exist (it has none
on day one — LLM stances cannot be backfilled without hindsight bias). See
services/prediction/app/committee_features.py.

Every agent falls back to a deterministic stub built from the same facts, so the whole committee
runs with zero keys / zero network (invariant #1).
"""
from __future__ import annotations

import json

from .prompt import BANNED

STANCES = ("bullish", "neutral", "bearish")
STANCE_NUM = {"bullish": 1.0, "neutral": 0.0, "bearish": -1.0}

COMMITTEE_DISCLAIMER = (
    "Analytical stances from AI personas — descriptive leans, NOT trade recommendations or "
    "targets. The only actionable signal in this app is the backtest-gated quant signal."
)

_SHARED_RULES = (
    "You are one analyst persona on a research committee, NOT a financial advisor. You NEVER give "
    "a buy, sell, or hold recommendation, price target, or position advice. A 'stance' is an "
    "analytical lean over the supplied evidence, nothing more. You only quote numbers that appear "
    "in the FACTS — never invent or recompute one. Reply with a single JSON object and nothing else."
)

ANALYST_SCHEMA = {
    "stance": "one of: bullish | neutral | bearish (an analytical lean, not advice)",
    "confidence": "number 0..1 — how strongly the evidence supports the stance",
    "reasoning": ["string (2-4 items, each grounded in a specific fact)"],
    "watchFor": "string — the single fact that would most change this read",
}
ANALYST_KEYS = ["stance", "confidence", "reasoning", "watchFor"]

DEBATER_SCHEMA = {
    "arguments": ["string (3-5 strongest points for your side, grounded in the analyst stances/facts)"],
    "rebuttals": ["string (1-3 direct counters to the other side's likely points)"],
}
DEBATER_KEYS = ["arguments", "rebuttals"]

JUDGE_SCHEMA = {
    "strongestBull": ["string (1-3 items)"],
    "strongestBear": ["string (1-3 items)"],
    "unresolved": ["string (0-3 open questions the evidence cannot settle)"],
    "summary": "string (2-3 sentences describing where the committee agrees/disagrees — no advice)",
}
JUDGE_KEYS = ["strongestBull", "strongestBear", "unresolved", "summary"]

PERSONAS = {
    "technical": (
        "You are the TECHNICAL analyst. You read only price-derived evidence: regime, trend, "
        "momentum, volatility, volume, key levels, and multi-timeframe confluence."
    ),
    "fundamentals": (
        "You are the FUNDAMENTALS analyst. You read only earnings evidence: EPS beat/miss history, "
        "surprise magnitude, and the recency of the last report."
    ),
    "news": (
        "You are the NEWS & SENTIMENT analyst. You read only headlines, SEC filings, and crowd "
        "sentiment. You weigh recency and source over volume, and you note when the flow is thin."
    ),
}


# ------------------------------------------------------------------------ context slicing

def _slice_context(agent: str, ctx: dict) -> dict:
    """Each analyst sees ONLY its slice — that is what makes the stances independent."""
    if agent == "technical":
        return {k: ctx.get(k) for k in ("regime", "confluence", "expectedClose") if ctx.get(k) is not None}
    if agent == "fundamentals":
        return {k: ctx.get(k) for k in ("earnings", "fiscalNote") if ctx.get(k) is not None}
    if agent == "news":
        return {k: ctx.get(k) for k in ("news", "filingsToday", "sentiment") if ctx.get(k) is not None}
    return ctx


# ------------------------------------------------------------------------ prompt builders

def build_analyst_prompt(agent: str, ticker: str, ctx: dict, firmer: bool = False) -> list[dict]:
    facts = _slice_context(agent, ctx)
    firm = ""
    if firmer:
        firm = (
            "\n\nIMPORTANT: Your previous reply was invalid. Return ONLY a JSON object with EXACTLY "
            "these keys: stance, confidence, reasoning, watchFor. stance must be bullish, neutral, "
            "or bearish. No prose, no markdown, no buy/sell language."
        )
    user = (
        f"TICKER: {ticker}\n\n"
        f"FACTS (pre-computed / pre-collected, treat as ground truth — your ONLY evidence):\n"
        f"{json.dumps(facts, indent=2)}\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n"
        f"{json.dumps(ANALYST_SCHEMA, indent=2)}\n\n"
        "If your slice of evidence is thin or missing, say so in reasoning and lean neutral with "
        "low confidence. Never give a recommendation or target." + firm
    )
    return [
        {"role": "system", "content": _SHARED_RULES + " " + PERSONAS[agent]},
        {"role": "user", "content": user},
    ]


def build_debater_prompt(side: str, ticker: str, analysts: dict, ctx: dict, firmer: bool = False) -> list[dict]:
    other = "bear" if side == "bull" else "bull"
    firm = ""
    if firmer:
        firm = (
            "\n\nIMPORTANT: Your previous reply was invalid. Return ONLY a JSON object with EXACTLY "
            "these keys: arguments, rebuttals. No prose, no buy/sell language."
        )
    system = (
        _SHARED_RULES + f" You are the {side.upper()} researcher: you construct the strongest "
        f"evidence-based {side} case and anticipate the {other} side's counters. Arguing a case is "
        "NOT recommending a trade — you present evidence, the human decides."
    )
    user = (
        f"TICKER: {ticker}\n\n"
        f"ANALYST STANCES (your raw material):\n{json.dumps(analysts, indent=2)}\n\n"
        f"KEY COMPUTED LEVELS (quote, never invent):\n"
        f"{json.dumps((ctx.get('regime') or {}).get('keyLevels'), indent=2)}\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n"
        f"{json.dumps(DEBATER_SCHEMA, indent=2)}" + firm
    )
    return [{"role": "system", "content": system}, {"role": "user", "content": user}]


def build_judge_prompt(ticker: str, analysts: dict, debate: dict, firmer: bool = False) -> list[dict]:
    firm = ""
    if firmer:
        firm = (
            "\n\nIMPORTANT: Your previous reply was invalid. Return ONLY a JSON object with EXACTLY "
            "these keys: strongestBull, strongestBear, unresolved, summary. No advice language."
        )
    system = (
        _SHARED_RULES + " You are the JUDGE. You weigh the debate and report where the evidence is "
        "strong, weak, or contradictory. You NEVER pick a winner as a trade call — you describe the "
        "state of the argument."
    )
    user = (
        f"TICKER: {ticker}\n\n"
        f"ANALYST STANCES:\n{json.dumps(analysts, indent=2)}\n\n"
        f"DEBATE:\n{json.dumps(debate, indent=2)}\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n"
        f"{json.dumps(JUDGE_SCHEMA, indent=2)}" + firm
    )
    return [{"role": "system", "content": system}, {"role": "user", "content": user}]


# ------------------------------------------------------------------------ validation

def _banned_hits(obj: dict) -> list[str]:
    blob = json.dumps(obj).lower()
    return [f"contains advice phrase: '{b}'" for b in BANNED if b in blob]


def validate_analyst(obj: dict | None) -> list[str]:
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    warnings = [f"missing key: {k}" for k in ANALYST_KEYS if k not in obj]
    if obj.get("stance") not in STANCES:
        warnings.append(f"invalid stance: {obj.get('stance')!r}")
    conf = obj.get("confidence")
    if not isinstance(conf, (int, float)) or not (0.0 <= float(conf) <= 1.0):
        warnings.append(f"invalid confidence: {conf!r}")
    if not (isinstance(obj.get("reasoning"), list) and obj["reasoning"]):
        warnings.append("empty reasoning")
    warnings.extend(_banned_hits(obj))
    return warnings


def validate_debater(obj: dict | None) -> list[str]:
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    warnings = [f"missing key: {k}" for k in DEBATER_KEYS if k not in obj]
    if not (isinstance(obj.get("arguments"), list) and obj["arguments"]):
        warnings.append("empty arguments")
    warnings.extend(_banned_hits(obj))
    return warnings


def validate_judge(obj: dict | None) -> list[str]:
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    warnings = [f"missing key: {k}" for k in JUDGE_KEYS if k not in obj]
    warnings.extend(_banned_hits(obj))
    return warnings


def needs_agent_retry(warnings: list[str]) -> bool:
    """Retry only on structural failure (missing keys / bad enum / not-JSON) — an advice phrase is
    flagged, and the offending output is replaced by the stub below, never shown raw."""
    return any(
        w.startswith(("missing key", "output is not")) or w.startswith("invalid stance")
        for w in warnings
    )


# ------------------------------------------------------------------------ deterministic stubs
# The zero-keys / zero-network path: honest stances computed from the same facts.

def stub_analyst(agent: str, ctx: dict) -> dict:
    if agent == "technical":
        reg = ctx.get("regime") or {}
        trend = str(reg.get("trend", "")).lower()
        momentum = str(reg.get("momentum", "")).lower()
        score = 0.0
        if "up" in trend:
            score += 1.0
        elif "down" in trend:
            score -= 1.0
        if "overbought" in momentum:
            score -= 0.3
        elif "oversold" in momentum:
            score += 0.3
        stance = "bullish" if score > 0.3 else "bearish" if score < -0.3 else "neutral"
        return {
            "stance": stance,
            "confidence": min(0.6, abs(score) / 2 + 0.2) if reg else 0.1,
            "reasoning": [
                f"Computed regime: trend {reg.get('trend', 'n/a')}, momentum {reg.get('momentum', 'n/a')}.",
                f"MACD {reg.get('macdState', 'n/a')}; volume {reg.get('volume', 'n/a')}.",
            ],
            "watchFor": "A trend or MACD state change in the computed regime.",
            "_stub": True,
        }
    if agent == "fundamentals":
        earnings = ctx.get("earnings") or []
        if isinstance(earnings, dict):
            earnings = [earnings]
        recent = earnings[:4]
        beats = sum(1 for e in recent if isinstance(e, dict) and (e.get("surprise") or 0) > 0)
        n = len(recent)
        stance = "bullish" if n and beats / n >= 0.75 else "bearish" if n and beats / n <= 0.25 else "neutral"
        return {
            "stance": stance,
            "confidence": 0.4 if n else 0.1,
            "reasoning": [
                f"{beats} of the last {n} reported quarters beat EPS estimates." if n
                else "No earnings history available in this context.",
            ],
            "watchFor": "The next EPS report vs. estimate.",
            "_stub": True,
        }
    # news
    items = ctx.get("news") or []
    sent_vals = [i.get("sentiment") for i in items if isinstance(i, dict) and isinstance(i.get("sentiment"), (int, float))]
    avg = sum(sent_vals) / len(sent_vals) if sent_vals else 0.0
    stance = "bullish" if avg > 0.15 else "bearish" if avg < -0.15 else "neutral"
    return {
        "stance": stance,
        "confidence": min(0.5, 0.15 + 0.05 * len(sent_vals)) if items else 0.1,
        "reasoning": [
            f"{len(items)} recent headlines; mean provider sentiment {round(avg, 2)}." if items
            else "No news flow available in this context.",
        ],
        "watchFor": "A new 8-K filing or a sharp sentiment swing.",
        "_stub": True,
    }


def stub_debater(side: str, analysts: dict) -> dict:
    want = "bullish" if side == "bull" else "bearish"
    args, rebuttals = [], []
    for name, a in analysts.items():
        if not isinstance(a, dict):
            continue
        reasons = a.get("reasoning") or []
        if a.get("stance") == want and reasons:
            args.append(f"[{name}] {reasons[0]}")
        elif a.get("stance") in STANCES and a.get("stance") != want and reasons:
            rebuttals.append(f"[{name}] leans {a.get('stance')}: {reasons[0]}")
    if not args:
        args = [f"No analyst currently leans {want}; the {side} case rests on a change in the evidence."]
    return {"arguments": args, "rebuttals": rebuttals, "_stub": True}


def stub_judge(analysts: dict, debate: dict) -> dict:
    bull = (debate.get("bull") or {}).get("arguments") or []
    bear = (debate.get("bear") or {}).get("arguments") or []
    stances = [a.get("stance") for a in analysts.values() if isinstance(a, dict) and a.get("stance") in STANCES]
    split = ", ".join(f"{s}: {stances.count(s)}" for s in STANCES if stances.count(s))
    return {
        "strongestBull": bull[:2],
        "strongestBear": bear[:2],
        "unresolved": ["Model offline — this is a deterministic digest of the stub debate."],
        "summary": f"Committee split — {split or 'no stances available'}. Deterministic offline "
                   "summary; evidence description only, no advice.",
        "_stub": True,
    }


# ------------------------------------------------------------------------ deterministic consensus

def compute_consensus(analysts: dict) -> dict:
    """A reproducible weighted vote over the analyst stances. Computed HERE, never by the model.
    Descriptive lean labels on purpose — 'lean', not a verdict."""
    votes = []
    for a in analysts.values():
        if isinstance(a, dict) and a.get("stance") in STANCE_NUM:
            conf = a.get("confidence")
            conf = float(conf) if isinstance(conf, (int, float)) else 0.0
            votes.append((STANCE_NUM[a["stance"]], max(0.0, min(1.0, conf))))
    if not votes:
        return {"score": 0.0, "label": "no-stance", "agreement": 0.0, "meanConfidence": 0.0, "n": 0}
    score = sum(s * c for s, c in votes) / len(votes)
    stances = [s for s, _ in votes]
    agreement = max(stances.count(v) for v in set(stances)) / len(stances)
    label = "bullish-lean" if score > 0.15 else "bearish-lean" if score < -0.15 else "mixed"
    return {
        "score": round(score, 4),
        "label": label,
        "agreement": round(agreement, 4),
        "meanConfidence": round(sum(c for _, c in votes) / len(votes), 4),
        "n": len(votes),
    }


def feature_vector(analysts: dict, consensus: dict) -> dict:
    """The numeric footprint persisted with each snapshot — what the prediction service may consume
    as candidate features once enough history exists (committee_features.py gates on that)."""
    def signed(agent: str) -> float:
        a = analysts.get(agent) or {}
        s = STANCE_NUM.get(a.get("stance"), 0.0)
        c = a.get("confidence")
        c = float(c) if isinstance(c, (int, float)) else 0.0
        return round(s * max(0.0, min(1.0, c)), 4)

    return {
        "technical": signed("technical"),
        "fundamentals": signed("fundamentals"),
        "news": signed("news"),
        "consensus": consensus.get("score", 0.0),
        "agreement": consensus.get("agreement", 0.0),
        "meanConfidence": consensus.get("meanConfidence", 0.0),
    }


# ------------------------------------------------------------------------ orchestration

def _run_step(call_model, safe_json, build, validate, needs_retry, stub) -> tuple[dict, str, list[str]]:
    """One agent: structured attempt -> one firmer retry -> deterministic stub. Same contract as
    every other model call in this service."""
    raw, model_used = call_model(build(firmer=False))
    obj = safe_json(raw)
    warnings = validate(obj)
    if not model_used.startswith("stub:") and needs_retry(warnings):
        raw, model_used = call_model(build(firmer=True))
        obj = safe_json(raw)
        warnings = validate(obj)
    if obj is None or needs_retry(warnings) or _banned_hits(obj if isinstance(obj, dict) else {}):
        obj = stub()
        if not model_used.startswith("stub:"):
            model_used = f"{model_used} (fell back to stub)"
        warnings = validate(obj)
    # clamp confidence so downstream math is always sane
    if isinstance(obj.get("confidence"), (int, float)):
        obj["confidence"] = max(0.0, min(1.0, float(obj["confidence"])))
    return obj, model_used, warnings


def run_committee(ticker: str, ctx: dict, call_model, safe_json) -> dict:
    """Run the full pipeline SEQUENTIALLY (one model call in flight at a time). Returns the full
    transcript: analyst stances, debate, judge digest, deterministic consensus + featureVector."""
    warnings: list[str] = []
    models: dict[str, str] = {}

    analysts: dict[str, dict] = {}
    for agent in ("technical", "fundamentals", "news"):
        obj, used, w = _run_step(
            call_model, safe_json,
            build=lambda firmer, a=agent: build_analyst_prompt(a, ticker, ctx, firmer),
            validate=validate_analyst, needs_retry=needs_agent_retry,
            stub=lambda a=agent: stub_analyst(a, ctx),
        )
        analysts[agent] = obj
        models[agent] = used
        warnings.extend(f"{agent}: {x}" for x in w)

    debate: dict[str, dict] = {}
    for side in ("bull", "bear"):
        obj, used, w = _run_step(
            call_model, safe_json,
            build=lambda firmer, s=side: build_debater_prompt(s, ticker, analysts, ctx, firmer),
            validate=validate_debater, needs_retry=needs_agent_retry,
            stub=lambda s=side: stub_debater(s, analysts),
        )
        debate[side] = obj
        models[side] = used
        warnings.extend(f"{side}: {x}" for x in w)

    judge, used, w = _run_step(
        call_model, safe_json,
        build=lambda firmer: build_judge_prompt(ticker, analysts, debate, firmer),
        validate=validate_judge, needs_retry=needs_agent_retry,
        stub=lambda: stub_judge(analysts, debate),
    )
    models["judge"] = used
    warnings.extend(f"judge: {x}" for x in w)

    consensus = compute_consensus(analysts)
    return {
        "ticker": ticker,
        "analysts": analysts,
        "debate": debate,
        "judge": judge,
        "consensus": consensus,
        "featureVector": feature_vector(analysts, consensus),
        "modelsUsed": models,
        "warnings": warnings,
        "disclaimer": COMMITTEE_DISCLAIMER,
    }
