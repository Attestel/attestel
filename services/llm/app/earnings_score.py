"""AI earnings scorer — prompt + validation + deterministic stub.

Scores the TEXT available at/before an earnings event (press release / headline / filing excerpt)
into a small structured signal: tone, guidance direction, confidence, risk flags. It reads only
event-time text — it is NOT given price, future returns, or any number to predict. Same discipline as
the read/brief: structured JSON, validated, one firmer retry, deterministic stub fallback, and NO
buy/sell/verdict language (reuses the read's BANNED list).

Used by the offline event-study harness's Test 2 (does AI tone add marginal value on top of the
SUE-based PEAD rule?). It is honest about data limits: historical event-time text is scarce, so the
harness scores only the retrievable subset and accumulates forward.
"""
from __future__ import annotations

import json
import re

from .prompt import BANNED

SCORE_SYSTEM = (
    "You are an earnings-report reader. Given the TEXT of a company's earnings release / headline / "
    "filing excerpt that was public AT OR BEFORE a given date, extract a compact, structured read of "
    "its TONE and forward GUIDANCE. You judge ONLY the text you are given — you have no price data and "
    "you must not predict returns or give any buy/sell recommendation, price target, or verdict. If "
    "the text is thin or ambiguous, say so with low confidence. Reply with a single JSON object only."
)

SCORE_SCHEMA = {
    "tone": "number in [-1, 1] — overall sentiment of the text (negative..positive)",
    "guidanceDirection": "one of: up | down | flat — the direction of forward guidance, or flat if none",
    "confidence": "number in [0, 1] — how strongly the text supports the read",
    "riskFlags": ["string — 0-4 concrete risks/uncertainties mentioned in the text"],
}

SCORE_REQUIRED_KEYS = ["tone", "guidanceDirection", "confidence", "riskFlags"]
_DIRECTIONS = {"up", "down", "flat"}


def build_score_prompt(ticker: str, as_of: str, text: str, firmer: bool = False) -> list[dict]:
    firm = ""
    if firmer:
        firm = (
            "\n\nIMPORTANT: Your previous reply was invalid. Return ONLY a JSON object with EXACTLY "
            "these keys: tone (number -1..1), guidanceDirection (up|down|flat), confidence (0..1), "
            "riskFlags (array of strings). No prose, no buy/sell language."
        )
    user = (
        f"Company: {ticker.upper()}. Text was public as of {as_of}. Score ONLY this text:\n"
        f"\"\"\"\n{text.strip()}\n\"\"\"\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n{json.dumps(SCORE_SCHEMA, indent=2)}\n\n"
        "Do not reference price, returns, or any information beyond the text. No buy/sell/hold "
        "recommendation, price target, or verdict." + firm
    )
    return [{"role": "system", "content": SCORE_SYSTEM}, {"role": "user", "content": user}]


def validate_score(obj: dict | None) -> list[str]:
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    warnings = [f"missing key: {k}" for k in SCORE_REQUIRED_KEYS if k not in obj]
    tone = obj.get("tone")
    if not isinstance(tone, (int, float)) or not (-1.0 <= float(tone) <= 1.0):
        warnings.append("tone not a number in [-1,1]")
    if str(obj.get("guidanceDirection", "")).lower() not in _DIRECTIONS:
        warnings.append("guidanceDirection not in {up,down,flat}")
    conf = obj.get("confidence")
    if not isinstance(conf, (int, float)) or not (0.0 <= float(conf) <= 1.0):
        warnings.append("confidence not a number in [0,1]")
    if "riskFlags" in obj and not isinstance(obj["riskFlags"], list):
        warnings.append("riskFlags is not a list")
    blob = json.dumps(obj, default=str).lower()
    for b in BANNED:
        if b in blob:
            warnings.append(f"contains advice phrase: '{b}'")
    return warnings


def needs_score_retry(warnings: list[str]) -> bool:
    return any(
        w.startswith(("missing key", "output is not"))
        or "tone not" in w
        or "guidanceDirection not" in w
        for w in warnings
    )


# --------------------------------------------------------------------------- deterministic stub

_POS = ("beat", "beats", "record", "strong", "growth", "grew", "raised", "raise", "above",
        "exceeded", "exceed", "upside", "momentum", "robust", "accelerat", "outperform", "surge",
        "higher", "increase", "expansion", "demand")
_NEG = ("miss", "missed", "decline", "declined", "weak", "cut", "lowered", "lower", "below",
        "softness", "soft", "headwind", "disappoint", "warn", "warning", "slowdown", "slowing",
        "shortfall", "pressure", "impairment", "loss", "reduce", "reduced")
_RISK = {
    "litigation": "litigation mentioned", "investigation": "regulatory/legal investigation",
    "recall": "product recall", "guidance cut": "guidance cut", "lawsuit": "lawsuit",
    "restructuring": "restructuring", "layoff": "layoffs", "impairment": "asset impairment",
    "dilution": "share dilution", "going concern": "going-concern doubt",
}
_UP_GUIDE = ("raised guidance", "raises guidance", "raise guidance", "raised its outlook",
             "raised outlook", "guides above", "raised full-year")
_DOWN_GUIDE = ("cut guidance", "lowered guidance", "lowers guidance", "cut its outlook",
               "lowered outlook", "guides below", "reduced guidance", "lowered full-year")


def _count(text: str, words) -> int:
    return sum(len(re.findall(r"\b" + re.escape(w), text)) for w in words)


def stub_score(ticker: str, text: str) -> dict:
    """Deterministic keyword-based earnings read (no model). Descriptive tone only — never advice."""
    t = (text or "").lower()
    pos, neg = _count(t, _POS), _count(t, _NEG)
    total = pos + neg
    tone = round((pos - neg) / total, 3) if total else 0.0

    guidance = "flat"
    if any(g in t for g in _UP_GUIDE):
        guidance = "up"
    elif any(g in t for g in _DOWN_GUIDE):
        guidance = "down"
    elif tone > 0.25:
        guidance = "up"
    elif tone < -0.25:
        guidance = "down"

    # confidence grows with evidence volume, capped; ~0 when the text is empty/neutral
    confidence = round(min(total / 8.0, 1.0) * (0.4 + 0.6 * min(abs(tone) + 0.0, 1.0)), 3) if total else 0.0

    risk_flags = [label for key, label in _RISK.items() if key in t][:4]

    return {
        "tone": tone,
        "guidanceDirection": guidance,
        "confidence": confidence,
        "riskFlags": risk_flags,
        "_stub": True,
    }
