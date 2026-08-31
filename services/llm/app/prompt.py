"""Prompt builder (original doc §3.3) — now demands STRUCTURED JSON (Phase 3 / original GAP-4).

The model returns a fixed JSON object, not prose, so reads can be stored, diffed across days, and
rendered by the frontend without fragile markdown parsing. Still hands the model ONLY pre-computed
facts + levels (GAP-2) and forbids buy/sell recommendations.
"""
from __future__ import annotations

import json

SYSTEM = (
    "You are a technical-analysis assistant, NOT a financial advisor. You explain what the "
    "computed technical evidence shows. You NEVER give a buy or sell recommendation, price "
    "target, or verdict. You only quote numbers that appear in the FACTS — never invent or "
    "recompute a level. You reply with a single JSON object and nothing else."
)

# The exact schema the model must emit.
SCHEMA = {
    "bullCase": ["string (2-4 items, grounded in the facts)"],
    "bearCase": ["string (2-4 items, grounded in the facts)"],
    "keyLevels": {"support": ["number, quoted from FACTS"], "resistance": ["number, quoted from FACTS"]},
    "invalidation": {"bull": "string", "bear": "string"},
    "summary": "string (2-3 sentences of directional CONTEXT, not advice)",
}

REQUIRED_KEYS = ["bullCase", "bearCase", "keyLevels", "invalidation", "summary"]

BANNED = ("i recommend buying", "i recommend selling", "you should buy", "you should sell",
          "strong buy", "strong sell", "price target")

# --- versions (Wave 1 Lane 1D, additive) --------------------------------------------------------
# `name@n`, the form EVENT_CONTRACTS.md §5 uses ("final-analyst@3", "thesis@2", "tools@1").
# PROMPT_VERSION covers SYSTEM + build_prompt's user message; SCHEMA_VERSION covers SCHEMA +
# REQUIRED_KEYS. **Changing SYSTEM, SCHEMA or REQUIRED_KEYS requires bumping the matching version**
# — `versioning.py` pins a hash of all three and `tests/test_modes.py` fails an unversioned edit.
# They live here (not in versioning.py) so the import runs one way only: versioning -> prompt.
PROMPT_VERSION = "read@1"
SCHEMA_VERSION = "read-structured@1"


def _facts(ticker: str, regime: dict, confluence: dict | None = None) -> dict:
    facts = {
        "ticker": ticker,
        "timeframe": regime.get("timeframe"),
        "lastClose": regime.get("lastClose"),
        "trend": regime.get("trend"),
        "shortTermTrend": regime.get("shortTermTrend"),
        "momentum": regime.get("momentum"),
        "macdState": regime.get("macdState"),
        "volatility": regime.get("volatility"),
        "priceVsBands": regime.get("priceVsBands"),
        "volume": regime.get("volume"),
        "trendStrength": regime.get("trendStrength"),
        "stochastic": regime.get("stochastic"),
        "volumeTrend": regime.get("volumeTrend"),
        "vwapState": regime.get("vwapState"),
        "movingAverages": regime.get("movingAverages"),
        "keyLevels_COMPUTED": regime.get("keyLevels"),
    }
    if confluence:
        # Pre-computed multi-timeframe picture (deterministic). The model comments on it; it must
        # not invent it. Trim to the fields worth reasoning over.
        facts["confluence_MULTITIMEFRAME"] = {
            "trendAlignment": confluence.get("trendAlignment"),
            "alignmentScore": confluence.get("alignmentScore"),
            "trendByTf": confluence.get("trendByTf"),
            "momentumByTf": confluence.get("momentumByTf"),
            "conflicts": confluence.get("conflicts"),
            "note": confluence.get("note"),
        }
    return facts


def build_prompt(
    ticker: str,
    regime: dict,
    recent_bars: list[dict],
    firmer: bool = False,
    confluence: dict | None = None,
) -> list[dict]:
    facts = _facts(ticker, regime, confluence)
    firm = ""
    if firmer:
        firm = (
            "\n\nIMPORTANT: Your previous reply was invalid. Return ONLY a JSON object with EXACTLY "
            "these keys: bullCase, bearCase, keyLevels{support,resistance}, invalidation{bull,bear}, "
            "summary. No prose, no markdown, no buy/sell language."
        )
    confluence_instr = ""
    if confluence:
        confluence_instr = (
            "\n\nCONFLUENCE: The FACTS include confluence_MULTITIMEFRAME (1D/1H/15m). Explicitly note "
            "where the timeframes AGREE and where they CONFLICT in bullCase / bearCase and the summary "
            "(e.g. a daily uptrend with a 15m overbought pullback). Describe the alignment; do NOT turn "
            "it into a buy/sell call."
        )
    user = (
        f"FACTS (all pre-computed, treat as ground truth):\n{json.dumps(facts, indent=2)}\n\n"
        f"LAST {len(recent_bars)} BARS:\n{json.dumps(recent_bars, indent=2)}\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n{json.dumps(SCHEMA, indent=2)}\n\n"
        "Quote the support/resistance numbers from keyLevels_COMPUTED. Do not invent levels. "
        "Do not include any buy/sell recommendation." + confluence_instr + firm
    )
    return [{"role": "system", "content": SYSTEM}, {"role": "user", "content": user}]


def validate_structured(obj: dict | None) -> list[str]:
    """Loose validation (original GAP-3): missing keys, empty sections, sneaked-in advice."""
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    warnings = [f"missing key: {k}" for k in REQUIRED_KEYS if k not in obj]
    if isinstance(obj.get("bullCase"), list) and not obj["bullCase"]:
        warnings.append("empty bullCase")
    if isinstance(obj.get("bearCase"), list) and not obj["bearCase"]:
        warnings.append("empty bearCase")
    kl = obj.get("keyLevels")
    if not isinstance(kl, dict) or "support" not in kl or "resistance" not in kl:
        warnings.append("keyLevels missing support/resistance")
    blob = json.dumps(obj).lower()
    for b in BANNED:
        if b in blob:
            warnings.append(f"contains advice phrase: '{b}'")
    return warnings


def needs_retry(warnings: list[str]) -> bool:
    """Retry only on structural failure — not on a soft advice warning we can just flag."""
    return any(w.startswith(("missing key", "output is not")) or "keyLevels missing" in w for w in warnings)


def structured_to_markdown(obj: dict) -> str:
    """Render the structured read to markdown for display / logging."""
    def bullets(items):
        return "\n".join(f"- {x}" for x in (items or [])) or "- (none)"
    kl = obj.get("keyLevels", {}) or {}
    inv = obj.get("invalidation", {}) or {}
    sup = ", ".join(str(x) for x in (kl.get("support") or [])) or "n/a"
    res = ", ".join(str(x) for x in (kl.get("resistance") or [])) or "n/a"
    return (
        f"## Bull Case\n{bullets(obj.get('bullCase'))}\n\n"
        f"## Bear Case\n{bullets(obj.get('bearCase'))}\n\n"
        f"## Key Levels\n- Support: {sup}\n- Resistance: {res}\n\n"
        f"## What Would Invalidate Each\n- Bull invalidation: {inv.get('bull', 'n/a')}\n"
        f"- Bear invalidation: {inv.get('bear', 'n/a')}\n\n"
        f"## Summary\n{obj.get('summary', 'n/a')}"
    )
