"""The agentic analyst pipeline — Wave 3 Lane 3B.

    Orchestrator (non-thinking, tool routing)
        ├── Technical specialist    (thinking)
        ├── News/Event specialist   (non-thinking extraction, thinking on conflict)
        └── Macro/Market specialist (thinking)
                        ↓
            Final Analyst (thinking) → one EVENT_CONTRACTS.md §7 object PER HORIZON
                        ↓
       BULLISH / NEUTRAL / BEARISH / UNCLEAR + evidence + risks + invalidation

Contract: `docs/research-os/EVENT_CONTRACTS.md` §7 (the output), §6 (the tool surface), §5 (the
record Lane 3C persists), §1 (`as_of`), §9.11 (the horizon vocabulary), §9.17 (the computed score),
§9.18 (the disclosure string), §9.19 (`BANNED_ANALYST`), §9.21 (the cross-process model lease).

WHY THIS IS A NEW FILE AND NOT AN EDIT TO `committee.py`
--------------------------------------------------------
`services/prediction/app/committee_features.py` reads the LLM-owned PostgreSQL committee snapshots
and their `featureVector`; the historical READS_DIR path remains only as local fallback. Every
deterministic primitive this pipeline needs already exists there and is IMPORTED, never copied:
`STANCE_NUM` and `compute_consensus`. Importing is not editing — `git diff committee.py` is empty.

THE MODEL LEASE IS THE POINT OF THIS FILE (§9.21)
--------------------------------------------------
Wave 2 proved `lease.acquire_interactive()` excludes across a real OS process boundary and then
shipped with NO production caller: an `/read` and the separately-invoked enrichment worker could generate
concurrently, so the 16 GB one-model-at-a-time budget was unenforced. This module is the first
caller. `_generate()` is the ONLY place a model call is made here, and every one of its call sites —
orchestrator, all three specialists, the final analyst, and both firmer retries — goes through it,
inside the lease. There is no second, in-process lock: a `threading` lock gives zero exclusion
against a different address space, which is the whole failure mode.

The lease is taken PER GENERATION rather than once per run, deliberately. A run makes up to eight
sequential calls and can span minutes; holding the flock across all of them would starve the
enrichment worker for the whole run for no benefit, since only one call is ever in flight anyway
(decision 7: strictly sequential, one model at a time). Per-generation also keeps the acquisitions
non-nested — `fcntl.flock` on a second fd of the same file blocks *within* one process too, so a
nested acquire would self-deadlock.

INVARIANT #4: this module makes a model call only from `run_analyst()`, which is reached only from
`POST /analyst`, which the gateway reaches only from `POST /api/analyst/{ticker}`. No timer, poll,
scheduler tick or page load may call it.

INVARIANT #3: the model never invents a number. `support` / `resistance` are re-imposed from
`services/analysis` AFTER the model replies (`_reimpose_levels`), exactly as `main.py::read` already
overwrites `keyLevels`; and the one float the benchmark rests on — §5's `forecast.horizons[].score`
— is computed in code from the model's ORDINAL outputs (`_score`), never emitted by the model. Any
`score` the model volunteers is stripped and recorded as a warning.
"""
from __future__ import annotations

import json
import os
import re
from datetime import datetime, timedelta, timezone
from typing import Any, Callable, Iterable

from pydantic import BaseModel

from . import ablation_verdict
from . import lease
from . import llm as _llm
from . import modes
from .analyst_router import analyst_router
from .committee import STANCE_NUM, compute_consensus
from .prompt import BANNED
from .versioning import TOOL_SCHEMA_VERSION, identity_stamp

__all__ = [
    "ANALYST_DISCLAIMER", "BANNED_ANALYST", "CONFIDENCE_WEIGHT", "HORIZON_FIELD", "HORIZON_KEYS",
    "DIRECTIONS", "CONFIDENCE_BUCKETS", "EVIDENCE_LAYERS", "REQUIRED_EVIDENCE_LAYERS",
    "PROMPT_VERSION_ORCHESTRATOR", "PROMPT_VERSION_TECHNICAL", "PROMPT_VERSION_NEWS",
    "PROMPT_VERSION_MACRO", "PROMPT_VERSION_FINAL", "SCHEMA_VERSION_THESIS",
    "MAX_TOOL_CALLS_PER_SPECIALIST", "MAX_TOOL_CALLS_PER_RUN", "ANALYST_RULES",
    "DIRECTION_BRANCHES",
    "build_final_prompt", "validate_thesis", "needs_thesis_retry", "banned_analyst_hits",
    "compute_score", "run_analyst", "AnalystRequest", "unserved_numbers",
]


# ═══════════════════════════════════════════════════════════════ contract constants

#: §9.11's mapping — THE ONLY PERMITTED TABLE, implemented once, here. §4.4/§5 speak the request
#: tokens on the left; §7's `horizon` field speaks the spelling on the right. Never a third
#: vocabulary. (§9.11 also exempts §3.8's `expected_horizon`, which is an impact-duration label on a
#: different axis and is not touched by this map.)
HORIZON_FIELD: dict[str, str] = {
    "1d": "1_trading_day",
    "5d": "5_trading_days",
    "20d": "20_trading_days",
    "60d": "60_trading_days",
}
HORIZON_KEYS = tuple(HORIZON_FIELD)

#: §7's enums, spelled exactly as the contract spells them.
DIRECTIONS = ("bullish", "neutral", "bearish", "unclear")
CONFIDENCE_BUCKETS = ("low", "medium", "high")

#: §7's `evidence` object covers exactly these five layers — no more, no fewer.
EVIDENCE_LAYERS = ("technical", "companyNews", "macro", "marketContext", "fundamentals")

#: The layers whose absence at the cutoff forces `direction: "unclear"` for the whole run. Only
#: `technical` qualifies: a price thesis with no price evidence is not a low-confidence thesis, it
#: is an absent one, and §7's binding rule is that the honest answer there is `unclear` rather than
#: a fabricated reading. A missing news / macro / market / fundamentals layer degrades ITS OWN
#: evidence value to "unclear" and the run continues.
REQUIRED_EVIDENCE_LAYERS = ("technical",)

#: §9.17. `abstain` is `direction in ("neutral", "unclear")`.
CONFIDENCE_WEIGHT: dict[str, float] = {"low": 0.25, "medium": 0.55, "high": 0.85}

#: §9.18. A server-supplied CONSTANT — the model never generates it — stamped onto every response
#: adjacent to the output it qualifies (`docs/DISCLOSURE_CLASSIFICATION.md` rule 3), the same
#: posture as `COMMITTEE_DISCLAIMER` / `LENS_DISCLAIMER` / `DIGEST_DISCLAIMER`.
#:
#: BYTE-FOR-BYTE RECONCILED with `gateway/explore.go`'s `analystDisclaimer` (Lane 2C), which mirrors
#: this same string into every Explore item carrying a `possibleReadThrough`. It must stay a Go
#: literal there — the gateway renders Explore with no network and no model (invariants #1 and #4),
#: and a disclosure that can fail to load is not a disclosure. Changing one without the other is a
#: bug; `tests/test_analyst.py::test_analyst_disclaimer_matches_the_gateway_literal` reads
#: `gateway/explore.go` and asserts the two agree.
ANALYST_DISCLAIMER = (
    "Analytical context, not investment advice and not a recommendation. "
    "Read-throughs are hypotheses about how an event might reach a company, never a verdict on it."
)

#: §9.19. ADDITIVE to `prompt.BANNED`, which stays EXACTLY as it is — extending `BANNED` would
#: change `/read` for every existing user. The eight additions are position-management verbs: they
#: are not caught by `BANNED`'s "i recommend buying"-style phrases, but "trim" or "add on weakness"
#: in a thesis is a trade instruction wearing analysis clothing.
BANNED_ANALYST = BANNED + (
    "accumulate", "take profits", "add on weakness", "trim",
    "overweight", "underweight", "load up", "average down",
)

#: §9.56. The branches `_resolve_direction` can take. Everything except `"model"` is a SERVER
#: OVERRIDE: the direction shown was resolved by this module, not asserted by the model.
DIRECTION_BRANCHES = ("model", "missing-layer", "mixed", "no-stance", "out-of-enum")

#: §9.56. Direction and prose come from the SAME authority. When the server resolves the direction
#: it also writes the sentence, because the model wrote its narrative to support a direction the
#: server then discarded — Wave 3 integration measured a run resolved to `unclear` still reading
#: "Structure and flow both point the same way over this horizon." `BANNED_ANALYST` cannot catch
#: that: the sentence contains no banned phrase. It is a badge contradicted by the paragraph next
#: to it, which teaches the reader that the badge is decoration.
#:
#: The rule is deliberately "server resolved it" and NOT "the word changed". A model that happens
#: to land on the same word by a different route did not reason its way there, and a prose/badge
#: agreement reached by luck is not a guarantee. The cost is that some usable model prose is
#: discarded; `modelThesis` keeps every discarded sentence for audit and for Wave 5A's ablation.
_SERVER_THESIS_DIRECTION = {
    "bullish": "The evidence available at this cutoff resolves to a bullish read over the "
               "next {horizon}.",
    "bearish": "The evidence available at this cutoff resolves to a bearish read over the "
               "next {horizon}.",
    "neutral": "The evidence available at this cutoff does not resolve to a directional lean "
               "over the next {horizon}.",
    "unclear": "The evidence available at this cutoff does not support a directional read over "
               "the next {horizon}.",
}

_SERVER_THESIS_REASON = {
    "missing-layer": "A required evidence layer was unavailable, so there was no price evidence "
                     "to reason over; the evidence section lists what the run did reach.",
    "mixed": "The specialists disagree and their stances net out mixed, so the disagreement is "
             "itself the finding; both sides are in the specialist sections.",
    "no-stance": "No specialist produced a usable stance, so there was nothing to combine.",
    "out-of-enum": "The model returned a direction outside the permitted set, so the direction "
                   "shown was resolved from the specialist consensus instead.",
    # §9.56 binding 9. The model kept the direction and abstained in it. The server is not picking
    # a direction here — the served field already says `neutral`/`unclear`, and a line asserting NO
    # direction is exactly what that field means, so writing it invents nothing.
    "model": "The model's own reading did not resolve a lean, so none is asserted; the evidence "
             "section lists what the run reached.",
}

#: §9.56 binding 7. `invalidation` follows `thesis`, and is the WORSE of the two: a thesis that
#: contradicts an abstention is an inconsistency, while a falsification condition attached to an
#: abstention is a category error — it states the condition under which the thesis stops holding,
#: and there is no thesis. It also names a price. "Daily close below 173.80 on above-average
#: volume" beside `unclear` reads as a level to act on; invariant #2's letter is not breached (no
#: verdict word appears) but a specific price paired with a specific trigger is the thing
#: invariant #2 exists to keep off the screen, and the abstention badge does not defuse it.
_SERVER_INVALIDATION_ABSTAIN = (
    "No directional read was resolved at this cutoff, so no falsification condition is stated."
)

#: The other half of "on any override branch": the server resolved a DIRECTIONAL read from the
#: consensus. Saying "no directional read was resolved" there would be false and would reproduce
#: §9.56's own defect in the opposite direction, so this line says what is true instead — the read
#: is not the model's, so the model's falsification condition does not belong to it either.
_SERVER_INVALIDATION_OVERRIDE = (
    "The direction shown was resolved from the specialist consensus rather than from the model's "
    "own reading, so no falsification condition is stated with it."
)

#: §9.56 binding 5, now an AUDIT SIGNAL ONLY — binding 9 took its former job away.
#:
#: It used to be the gate: on the model-owned branch the prose was rewritten only if one of these
#: words appeared in it. That made a two-word blacklist load-bearing, and `BANNED_ANALYST` is the
#: standing proof that a blacklist misses the sentence that matters — "Structure and flow both
#: point the same way over this horizon" asserts a lean and contains neither word.
#:
#: Binding 9 replaced the gate with the direction itself (see `_server_owns_prose`), so nothing
#: user-visible now depends on this tuple. What remains is the warning, which the amendment kept
#: deliberately: it records that the model produced contradictory prose, which is evidence about
#: the model. Its false negatives cost an audit note and nothing else.
_ABSTAIN_CONTRADICTIONS = ("bullish", "bearish")

#: Bounds on the orchestrator loop. A model that loops is a timeout, not a thesis.
MAX_TOOL_CALLS_PER_SPECIALIST = 3
MAX_TOOL_CALLS_PER_RUN = 12

#: `name@n`, §5's form. Bump on ANY wording change to the corresponding prompt — that is the whole
#: point of AD-4, and without it Wave 5A cannot attribute a result to a cause.
PROMPT_VERSION_ORCHESTRATOR = "orchestrator@1"
PROMPT_VERSION_TECHNICAL = "technical-specialist@1"
PROMPT_VERSION_NEWS = "news-specialist@1"
PROMPT_VERSION_MACRO = "macro-specialist@1"
PROMPT_VERSION_FINAL = "final-analyst@1"
#: Covers `THESIS_SCHEMA` + `THESIS_KEYS` — §5's `identity.schemaVersion`.
SCHEMA_VERSION_THESIS = "thesis@1"

#: doc §8 task keys, resolved through Lane 1D's `modes.mode_for` — the routing table lives THERE, as
#: data, and an unknown task raises rather than defaulting. Re-implementing mode control here is how
#: the Wave 5 mode ablation silently becomes meaningless, so this maps role -> task and stops.
ROLE_TASK = {
    "orchestrator": "tool_routing",                    # non_thinking
    "technical": "technical_interpretation",           # thinking
    "news": "headline_extraction",                     # non_thinking (extraction)
    "news_conflict": "news_vs_technical_conflict",     # thinking (conflict resolution)
    "macro": "macro_impact",                           # thinking
    "final": "final_thesis_synthesis",                 # thinking
}

#: §6's per-specialist registries, as a FALLBACK ONLY. Lane 3A owns `services/llm/app/tools.py` and
#: is the authority; this table exists so the pipeline is well-defined before that file lands, and
#: `_registry_for` prefers 3A's `registry_for` whenever it is importable.
TOOL_SLICES: dict[str, tuple[str, ...]] = {
    "orchestrator": ("get_user_subscriptions", "get_ticker_context"),
    "technical": ("get_multi_timeframe_context", "get_candles"),
    "news": ("get_ticker_events", "get_event_details"),
    "macro": ("get_macro_context", "get_market_context"),
    "final": ("get_ticker_context", "get_market_context"),
}


# ═══════════════════════════════════════════════════════════════ Lane 3A's tool surface

# tools.py is Lane 3A's file and may not exist on this base. The import is tolerant for exactly the
# reason `analyst_router.py` documents: an absent sibling module must not break the service. Any
# OTHER missing module is a real error and is re-raised.
#
# MEASURED INTERFACE (Wave 3 integration, after 3A landed): `registry_for(role)` returns the
# OpenAI-compatible tool DEFINITIONS — `[{"type": "function", "function": {"name": ..., ...}}, ...]`
# — not the bare names this lane assumed while coding against an absent tools.py. `execute(tool,
# args, ctx) -> dict` does take a name, as assumed. Both consumers here want names, so
# `_registry_for` normalises; see its body. `as_of` is injected by the runtime and is NOT
# model-supplied. When 3A's surface is absent the pipeline still runs: every layer's evidence then
# comes from the gateway-assembled envelope, and a layer with no envelope slice degrades to
# "unclear" rather than being fabricated.
try:  # pragma: no cover - depends on whether Lane 3A has landed
    from .tools import execute as _tools_execute  # type: ignore[attr-defined]
    from .tools import registry_for as _tools_registry_for  # type: ignore[attr-defined]
except ImportError:  # ModuleNotFoundError, or tools.py present without these names yet
    _tools_execute = None
    _tools_registry_for = None


def _tool_name(entry: Any) -> str | None:
    """One registry entry -> its tool name, accepting either shape.

    3A returns OpenAI-compatible definitions; a bare string is still accepted so this does not
    re-break if the registry is ever flattened. Anything else is skipped rather than stringified —
    a `str(dict)` reaching a prompt would name a tool that does not exist.
    """
    if isinstance(entry, str):
        return entry
    if isinstance(entry, dict):
        name = entry.get("function", entry).get("name")
        return name if isinstance(name, str) else None
    return None


def _registry_for(role: str) -> tuple[str, ...]:
    """The tool slice `role` may call, always as NAMES. Lane 3A's registry wins; §6's table is the
    fallback.

    Normalising here is deliberate: both consumers want names (`_tools_note` joins them into a
    prompt, and 3A's `execute()` is keyed by name), and 3A's registry hands back definition dicts.
    Without this, `', '.join(...)` raises TypeError on every generation — which is exactly what the
    3A+3B merge produced before this was added.
    """
    if _tools_registry_for is not None:
        try:
            names = tuple(n for n in (_tool_name(e) for e in _tools_registry_for(role)) if n)
            if names:
                return names
        except Exception:  # pragma: no cover - a 3A-side error is not this lane's to interpret
            pass
    return TOOL_SLICES.get(role, ())


# ═══════════════════════════════════════════════════════════════ the nine analyst rules

#: doc Appendix B, adapted to contract §7 field names, near-verbatim. These go into the final
#: analyst's system prompt and `tests/test_analyst.py` asserts each one's key phrase is present.
#: Rule 5 is the one most likely to be quietly eroded by prompt tuning; it has its own test.
ANALYST_RULES = (
    "1. Use only supplied evidence and tool outputs available at the cutoff `asOf`. "
    "Nothing outside the supplied evidence exists.",
    "2. Never invent prices, news, consensus expectations or event times.",
    "3. Do not calculate indicators manually if a deterministic tool value is provided.",
    "4. Separate observation from inference. Observations belong in `evidence`; interpretation "
    "belongs in the thesis text and in `riskFlags`.",
    "5. Return `direction: \"neutral\"` or `direction: \"unclear\"` when the evidence does not "
    "support an edge.",
    "6. Identify evidence that contradicts your conclusion, and name it in `riskFlags`.",
    "7. State a concrete thesis invalidation condition in `invalidation` where possible.",
    "8. Return the required JSON schema only — one JSON object, no prose, no markdown fence.",
    "9. Treat `confidenceBucket` as an ordinal score, never a probability, unless an external "
    "calibration layer converts it.",
)

#: Mirrors `committee.py::_SHARED_RULES` in posture. Not a financial advisor; no buy/sell/hold; no
#: price target; only numbers that appear in the supplied evidence; one JSON object and nothing else.
_SHARED_RULES = (
    "You are a market-analysis specialist on a research pipeline, NOT a financial advisor. You "
    "NEVER give a buy, sell, or hold recommendation, price target, position size, or any "
    "instruction to accumulate, trim, add, average down, overweight or underweight. A direction is "
    "an analytical lean over the supplied evidence, nothing more. You only quote numbers that "
    "appear in the supplied evidence — never invent or recompute one. You reply with a single JSON "
    "object and nothing else."
)


# ═══════════════════════════════════════════════════════════════ schemas

ORCHESTRATOR_SCHEMA = {
    "specialists": ["one or more of: technical | news | macro"],
    "why": "string — one sentence per specialist, why its evidence layer matters here",
}
ORCHESTRATOR_KEYS = ["specialists", "why"]

TECHNICAL_SCHEMA = {
    "stance": "one of: bullish | neutral | bearish (an analytical lean, not advice)",
    "confidence": "number 0..1",
    "trendRegime": "string — the regime as the supplied evidence describes it",
    "structure": "string — higher-highs / higher-lows reading, quoted from the evidence",
    "momentumState": "string",
    "volatilityState": "string",
    "levelsObserved": ["number — quoted from the supplied computed levels, never recomputed"],
    "priceActionReading": "string — what the recent candles show",
    "technicalRisks": ["string (1-3) — evidence that contradicts the stance"],
}
TECHNICAL_KEYS = ["stance", "confidence", "trendRegime", "momentumState", "technicalRisks"]

NEWS_SCHEMA = {
    "stance": "one of: bullish | neutral | bearish",
    "confidence": "number 0..1",
    "events": [{
        "eventId": "string — copied verbatim from the supplied event",
        "relevance": "one of: high | medium | low",
        "novelty": "one of: new | developing | repeat",
        "severity": "one of: high | medium | low",
        "direction": "one of: bullish | neutral | bearish",
        "expectedHorizon": "one of: intraday | short_term | medium_term | long_term",
        "affectedDimensions": ["string"],
        "contradicts": "string — what in this event argues against the stance, or ''",
    }],
    "newsRisks": ["string (0-3)"],
}
NEWS_KEYS = ["stance", "confidence", "events", "newsRisks"]

MACRO_SCHEMA = {
    "stance": "one of: bullish | neutral | bearish",
    "confidence": "number 0..1",
    "surprises": [{
        "release": "string", "actual": "string — quoted from the evidence",
        "expected": "string — quoted from the evidence", "previous": "string — quoted",
        "reading": "string — surprise vs expectation, in words",
    }],
    "sensitivity": "string — how this ticker/sector is exposed to the above",
    "marketBackdrop": "string — the broad-market picture the evidence shows",
    "macroRisks": ["string (0-3)"],
}
MACRO_KEYS = ["stance", "confidence", "sensitivity", "marketBackdrop", "macroRisks"]

#: §7, minus the fields the SERVER owns. `support` / `resistance` are re-imposed after the reply
#: (invariant #3) and `disclaimer` is a constant (§9.18), so the model is never asked for them; and
#: there is deliberately NO `score` key — §9.17 computes it, the model never emits a float.
THESIS_SCHEMA = {
    "direction": "one of: bullish | neutral | bearish | unclear",
    "confidenceBucket": "one of: low | medium | high — an ORDINAL score, never a probability",
    "regime": "string — the regime label the supplied evidence carries",
    "evidence": {
        "technical": "bullish | neutral | bearish | unclear",
        "companyNews": "positive | neutral | negative | unclear",
        "macro": "positive | neutral | negative | unclear",
        "marketContext": "positive | neutral | negative | unclear",
        "fundamentals": "positive | neutral | negative | unclear",
    },
    "thesis": "string (2-4 sentences) — the horizon-specific read, descriptive, never advice",
    "invalidation": "string — a concrete condition that would falsify the thesis",
    "riskFlags": ["string (1-4) — including evidence that CONTRADICTS the direction"],
    "whatChanged": [{"at": "ISO-8601", "eventId": "string", "effect": "string"}],
    "sources": [{"eventId": "string", "documentId": "string", "url": "string"}],
}
#: The §7 keys the MODEL must supply. The server adds `ticker`, `asOf`, `horizon`, `support`,
#: `resistance`, `audit` and `disclaimer`.
THESIS_KEYS = ["direction", "confidenceBucket", "regime", "evidence", "thesis",
               "invalidation", "riskFlags", "whatChanged", "sources"]

#: The full §7 key set, asserted by the conformance test.
SECTION_7_KEYS = ("ticker", "asOf", "horizon", "direction", "confidenceBucket", "regime",
                  "evidence", "support", "resistance", "invalidation", "riskFlags",
                  "whatChanged", "sources", "audit", "disclaimer")


# ═══════════════════════════════════════════════════════════════ context slicing

def _normalize_context(ctx: dict) -> dict:
    """Expose the gateway's nested context envelope under the analyst's canonical slice keys.

    `gateway/context.go` owns the transport shape (`tickerContext.technical`, `recentEvents`, ...),
    while this module historically consumed the older flat committee shape. Keep both shapes
    accepted during migration, with explicit flat values winning if a caller supplied them.
    """
    out = dict(ctx or {})
    ticker_context = out.get("tickerContext")
    if not isinstance(ticker_context, dict):
        return out

    aliases = {
        "technical": "technicalContext",
        "recentEvents": "events",
        "earnings": "earnings",
        "fundamentals": "fundamentals",
        "macro": "macro",
    }
    for nested_key, canonical_key in aliases.items():
        value = ticker_context.get(nested_key)
        if canonical_key not in out and value not in (None, [], {}):
            out[canonical_key] = value
    return out


def _has_layer_evidence(key: str, value: Any) -> bool:
    """Distinguish a real evidence payload from a labelled but empty wrapper."""
    if value in (None, [], {}):
        return False
    if not isinstance(value, dict):
        return True
    if value.get("coverage") == "insufficient":
        return False
    if key in ("events", "news"):
        return bool(value.get("events") or value.get("seedCatalysts") or value.get("items"))
    if key in ("macro", "macroEvents"):
        return bool(value.get("macro") or value.get("events") or value.get("releases"))
    if key == "earnings":
        return bool(value.get("points") or value.get("earnings") or value.get("results"))
    if key == "marketContext":
        return any(value.get(k) not in (None, [], {}) for k in
                   ("benchmark", "sector", "rates", "volatility", "breadth"))
    if key == "technicalContext":
        return any(value.get(k) not in (None, [], {}) for k in
                   ("price", "indicators", "structure", "regime"))
    return True

def _slice(role: str, ctx: dict) -> dict:
    """Each specialist sees ONLY its slice — that is what makes the readings independent, and it is
    why the final analyst is handed the specialist OUTPUTS rather than the raw union of everything.
    """
    if role == "technical":
        keys = ("regime", "confluence", "expectedClose", "technicalContext",
                "multiTimeframeContext", "candles")
    elif role == "news":
        # Earnings/fundamentals are company-specific evidence. There is no fourth specialist, so
        # the news/event specialist is the bounded company-evidence reader that carries them into
        # final synthesis; otherwise a layer could be marked available without any specialist ever
        # seeing it.
        keys = ("news", "events", "filingsToday", "sentiment", "earnings", "fundamentals")
    elif role == "macro":
        keys = ("macro", "macroEvents", "marketContext", "calendar")
    elif role == "orchestrator":
        keys = ("subscriptions", "regime", "news", "macro")
    else:
        keys = ()
    return {k: ctx.get(k) for k in keys if ctx.get(k) not in (None, [], {})}


#: Which envelope keys evidence each §7 layer. A layer with no slice is UNAVAILABLE at the cutoff —
#: which is a fact about the evidence, not a licence to guess.
_LAYER_SOURCES: dict[str, tuple[str, ...]] = {
    "technical": ("regime", "technicalContext", "multiTimeframeContext", "confluence", "candles"),
    "companyNews": ("news", "events", "filingsToday"),
    "macro": ("macro", "macroEvents", "calendar"),
    "marketContext": ("marketContext", "benchmark", "sector"),
    "fundamentals": ("earnings", "fundamentals", "fiscalNote"),
}


def _available_layers(ctx: dict) -> dict[str, bool]:
    return {
        layer: any(_has_layer_evidence(k, ctx.get(k)) for k in sources)
        for layer, sources in _LAYER_SOURCES.items()
    }


# ═══════════════════════════════════════════════════════════════ prompt builders

def _firm(keys: Iterable[str]) -> str:
    return (
        "\n\nIMPORTANT: Your previous reply was invalid. Return ONLY a JSON object with EXACTLY "
        f"these keys: {', '.join(keys)}. No prose, no markdown fence, no buy/sell language."
    )


def _tools_note(role: str) -> str:
    reg = _registry_for(role)
    if not reg:
        return ""
    return (
        f"\n\nTOOLS AVAILABLE TO YOU: {', '.join(reg)}. The cutoff `asOf` is injected by the "
        "runtime and is NOT an argument you supply or may change."
    )


def build_orchestrator_prompt(ticker: str, ctx: dict, as_of: str | None,
                              firmer: bool = False,
                              tool_results: list | None = None) -> list[dict]:
    system = (
        _SHARED_RULES + " You are the ORCHESTRATOR. You do not analyse; you decide which evidence "
        "specialists are worth running for this ticker at this cutoff, and say why in one sentence "
        "each. Keep it cheap and predictable."
    )
    tools_block = ""
    if tool_results:
        tools_block = f"\n\nTOOL RESULTS:\n{json.dumps(tool_results, indent=2, default=str)}"
    user = (
        f"TICKER: {ticker}\nCUTOFF asOf: {as_of}\n\n"
        f"EVIDENCE AVAILABLE (summary only):\n{json.dumps(_slice('orchestrator', ctx), indent=2)}"
        f"{tools_block}\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n"
        f"{json.dumps(ORCHESTRATOR_SCHEMA, indent=2)}"
        + _tools_note("orchestrator")
        + (_firm(ORCHESTRATOR_KEYS) if firmer else "")
    )
    return [{"role": "system", "content": system}, {"role": "user", "content": user}]


_SPECIALIST_SYSTEM = {
    "technical": (
        "You are the TECHNICAL specialist. You read only price-derived evidence: multi-timeframe "
        "context, market structure, momentum, volatility, volume and the SERVER-COMPUTED levels. "
        "Quote levels; never recompute one. Combine the signals and resolve their contradictions "
        "explicitly."
    ),
    "news": (
        "You are the NEWS & EVENT specialist. You extract structured tags from the supplied "
        "canonical events — relevance, novelty, severity, direction, expected horizon, affected "
        "dimensions — and you note, per event, what in it argues AGAINST your stance. Copy every "
        "`eventId` verbatim from the supplied evidence; an event not supplied does not exist."
    ),
    "macro": (
        "You are the MACRO & MARKET specialist. You read macro releases as actual vs expected vs "
        "previous, map the surprise to this ticker's and sector's sensitivity, and describe the "
        "broad-market backdrop. Quote every figure from the supplied evidence."
    ),
}
_SPECIALIST_SCHEMA = {"technical": TECHNICAL_SCHEMA, "news": NEWS_SCHEMA, "macro": MACRO_SCHEMA}
_SPECIALIST_KEYS = {"technical": TECHNICAL_KEYS, "news": NEWS_KEYS, "macro": MACRO_KEYS}


def build_specialist_prompt(role: str, ticker: str, ctx: dict, as_of: str | None,
                            firmer: bool = False, tool_results: list | None = None) -> list[dict]:
    facts = _slice(role, ctx)
    tools_block = ""
    if tool_results:
        tools_block = f"\n\nTOOL RESULTS:\n{json.dumps(tool_results, indent=2, default=str)}"
    user = (
        f"TICKER: {ticker}\nCUTOFF asOf: {as_of}\n\n"
        f"EVIDENCE (pre-computed, treat as ground truth — your ONLY evidence):\n"
        f"{json.dumps(facts, indent=2, default=str)}{tools_block}\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n"
        f"{json.dumps(_SPECIALIST_SCHEMA[role], indent=2)}\n\n"
        "If your slice of evidence is thin or missing, say so and lean neutral with low confidence. "
        "Never give a recommendation, target, or position instruction."
        + _tools_note(role)
        + (_firm(_SPECIALIST_KEYS[role]) if firmer else "")
    )
    return [{"role": "system", "content": _SHARED_RULES + " " + _SPECIALIST_SYSTEM[role]},
            {"role": "user", "content": user}]


CONFLICT_SCHEMA = {
    "weighting": "one of: technical | news | unresolved — which layer the evidence weights harder",
    "resolution": "string (1-3 sentences) — the explicit evidence weighting, never a verdict",
}
CONFLICT_KEYS = ["weighting", "resolution"]


def build_conflict_prompt(ticker: str, specialists: dict, as_of: str | None,
                          firmer: bool = False) -> list[dict]:
    """doc §8's "news vs technical conflict" task — THINKING, because it needs explicit evidence
    weighting rather than a schema transformation."""
    system = (
        _SHARED_RULES + " You are resolving a CONFLICT between the technical and news readings. "
        "Weigh the evidence explicitly and say which layer the supplied evidence supports harder, "
        "or that it is unresolved. Naming a stronger layer is not a trade call."
    )
    user = (
        f"TICKER: {ticker}\nCUTOFF asOf: {as_of}\n\n"
        f"CONFLICTING SPECIALIST OUTPUTS:\n{json.dumps(specialists, indent=2, default=str)}\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n"
        f"{json.dumps(CONFLICT_SCHEMA, indent=2)}"
        + (_firm(CONFLICT_KEYS) if firmer else "")
    )
    return [{"role": "system", "content": system}, {"role": "user", "content": user}]


def build_final_prompt(ticker: str, horizon_key: str, specialists: dict, levels: dict,
                       as_of: str | None, layers: dict | None = None,
                       firmer: bool = False,
                       tool_results: list | None = None) -> list[dict]:
    """The final analyst's prompt. The nine rules go in NEAR-VERBATIM and are tested for."""
    system = (
        "ROLE: You are a market-analysis specialist.\n\n"
        + _SHARED_RULES
        + "\n\nRULES:\n"
        + "\n".join(f" {r}" for r in ANALYST_RULES)
    )
    unavailable = sorted(k for k, v in (layers or {}).items() if not v)
    gaps = ""
    if unavailable:
        gaps = (
            f"\n\nEVIDENCE LAYERS UNAVAILABLE AT THIS CUTOFF: {', '.join(unavailable)}. "
            "Mark each of them \"unclear\" in `evidence`. Do not infer a reading for a layer you "
            "were given no evidence for (rule 5)."
        )
    tools_block = ""
    if tool_results:
        tools_block = f"\n\nFINAL TOOL RESULTS:\n{json.dumps(tool_results, indent=2, default=str)}"
    user = (
        f"TICKER: {ticker}\nCUTOFF asOf: {as_of}\n"
        f"HORIZON: {HORIZON_FIELD[horizon_key]}\n\n"
        f"SPECIALIST OUTPUTS (your raw material):\n{json.dumps(specialists, indent=2, default=str)}\n\n"
        f"SERVER-COMPUTED LEVELS (quote these; they are re-imposed after your reply either way):\n"
        f"{json.dumps(levels, indent=2, default=str)}{gaps}{tools_block}\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n"
        f"{json.dumps(THESIS_SCHEMA, indent=2)}\n\n"
        "Do not emit a numeric score, probability or percentage anywhere: `confidenceBucket` is "
        "ordinal and the server computes every number."
        + _tools_note("final")
        + (_firm(THESIS_KEYS) if firmer else "")
    )
    return [{"role": "system", "content": system}, {"role": "user", "content": user}]


# ═══════════════════════════════════════════════════════════════ validation

def banned_analyst_hits(payload: Any) -> list[str]:
    """§9.19. Case-insensitive over the SERIALISED payload — the same matching posture
    `prompt.validate_structured` uses for `BANNED` on `/read`. Applied to the analyst output and,
    by the caller, to `transmission`, `possibleReadThrough` and the persisted `thesis` /
    `invalidation` text."""
    blob = json.dumps(payload, default=str).lower()
    return [f"contains advice phrase: '{b}'" for b in BANNED_ANALYST if b in blob]


def _enum_warn(obj: dict, key: str, allowed: tuple[str, ...]) -> list[str]:
    if obj.get(key) not in allowed:
        return [f"invalid {key}: {obj.get(key)!r}"]
    return []


def validate_thesis(obj: dict | None) -> list[str]:
    """Thesis-specific structural validation. It lives HERE and not in `prompt.py`: `REQUIRED_KEYS`
    belongs to `/read` and stays exactly as it is."""
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    w = [f"missing key: {k}" for k in THESIS_KEYS if k not in obj]
    w += _enum_warn(obj, "direction", DIRECTIONS)
    w += _enum_warn(obj, "confidenceBucket", CONFIDENCE_BUCKETS)

    ev = obj.get("evidence")
    if not isinstance(ev, dict):
        w.append("evidence missing layers")
    elif set(ev) != set(EVIDENCE_LAYERS):
        missing = sorted(set(EVIDENCE_LAYERS) - set(ev))
        extra = sorted(set(ev) - set(EVIDENCE_LAYERS))
        if missing:
            w.append(f"evidence missing layers: {', '.join(missing)}")
        if extra:
            w.append(f"evidence has unknown layers: {', '.join(extra)}")

    if not (isinstance(obj.get("riskFlags"), list) and obj["riskFlags"]):
        w.append("empty riskFlags")
    if not str(obj.get("invalidation") or "").strip():
        w.append("empty invalidation")

    for i, s in enumerate(obj.get("sources") or []):
        if not isinstance(s, dict) or not any(k in s for k in ("eventId", "documentId", "url")):
            w.append(f"source {i} carries no eventId/documentId/url")
    for i, c in enumerate(obj.get("whatChanged") or []):
        if not isinstance(c, dict) or not all(k in c for k in ("at", "eventId", "effect")):
            w.append(f"whatChanged {i} missing at/eventId/effect")

    if "score" in obj:
        # §9.17: the model never emits a float for `score`. Recorded, then stripped by the caller.
        w.append("model emitted 'score'; the server computes it (§9.17)")

    w.extend(banned_analyst_hits(obj))
    return w


def needs_thesis_retry(warnings: list[str]) -> bool:
    """Structural failure ONLY — never a soft advice warning. Same test `prompt.needs_retry`
    encodes for `/read`, extended to this schema's enum and evidence-shape failures."""
    return any(
        w.startswith(("missing key", "output is not", "invalid direction",
                      "invalid confidenceBucket", "evidence missing", "evidence has unknown"))
        for w in warnings
    )


def _analyst_validate(role: str, obj: dict | None) -> list[str]:
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    keys = ({"orchestrator": ORCHESTRATOR_KEYS, "news_conflict": CONFLICT_KEYS}.get(role)
            or _SPECIALIST_KEYS.get(role, []))
    w = [f"missing key: {k}" for k in keys if k not in obj]
    if role in _SPECIALIST_KEYS:
        if obj.get("stance") not in ("bullish", "neutral", "bearish"):
            w.append(f"invalid stance: {obj.get('stance')!r}")
        c = obj.get("confidence")
        if not isinstance(c, (int, float)) or not (0.0 <= float(c) <= 1.0):
            w.append(f"invalid confidence: {c!r}")
    w.extend(banned_analyst_hits(obj))
    return w


def _needs_agent_retry(warnings: list[str]) -> bool:
    return any(w.startswith(("missing key", "output is not", "invalid stance")) for w in warnings)


#: The keys each role's output may carry — its own schema's top level, and nothing else. Used by
#: `_run_step` to project every model reply down to its declared shape after validation.
_ROLE_ALLOWED: dict[str, frozenset[str]] = {
    "orchestrator": frozenset(ORCHESTRATOR_SCHEMA),
    "technical": frozenset(TECHNICAL_SCHEMA),
    "news": frozenset(NEWS_SCHEMA),
    "macro": frozenset(MACRO_SCHEMA),
    "news_conflict": frozenset(CONFLICT_SCHEMA),
    "final": frozenset(THESIS_KEYS),
}


# ═══════════════════════════════════════════════════════════════ deterministic stubs
# The zero-keys / zero-network floor (invariant #1). Reached ONLY after a model attempt and its one
# firmer retry have both failed structurally — `neutral` and `unclear` must NOT depend on this path,
# and `_resolve_direction` reaches both without it.

def stub_specialist(role: str, ctx: dict) -> dict:
    facts = _slice(role, ctx)
    thin = not facts
    base = {"stance": "neutral", "confidence": 0.1 if thin else 0.25, "_stub": True}
    if role == "technical":
        reg = (ctx.get("regime") or {})
        base.update({
            "trendRegime": str(reg.get("trend") or "unavailable"),
            "structure": str(reg.get("shortTermTrend") or "unavailable"),
            "momentumState": str(reg.get("momentum") or "unavailable"),
            "volatilityState": str(reg.get("volatility") or "unavailable"),
            "levelsObserved": [],
            "priceActionReading": "Model offline — deterministic digest of the computed regime.",
            "technicalRisks": ["Deterministic offline reading; no model interpretation applied."],
        })
    elif role == "news":
        base.update({
            "events": [],
            "newsRisks": ["Model offline — event flow was not interpreted."],
        })
    else:
        base.update({
            "surprises": [],
            "sensitivity": "unavailable",
            "marketBackdrop": "unavailable",
            "macroRisks": ["Model offline — macro backdrop was not interpreted."],
        })
    return base


def stub_thesis(ticker: str, layers: dict, consensus: dict) -> dict:
    return {
        "direction": "unclear",
        "confidenceBucket": "low",
        "regime": "unavailable",
        "evidence": {k: ("unclear" if not layers.get(k) else "neutral") for k in EVIDENCE_LAYERS},
        "thesis": (
            f"Model offline: this is a deterministic digest for {ticker}. The specialist consensus "
            f"reads {consensus.get('label', 'no-stance')}; no interpretive thesis was generated."
        ),
        "invalidation": "Not established — no model reading was produced at this cutoff.",
        "riskFlags": ["Deterministic offline output; treat every layer as uninterpreted."],
        "whatChanged": [],
        "sources": [],
        "_stub": True,
    }


# ═══════════════════════════════════════════════════════════════ the lease-wrapped generator

def _default_call_model(messages: list[dict], *, mode: str | None = None):
    return _llm.call_model_meta(messages, json_mode=True, mode=mode)


def _generate(call_model: Callable, messages: list[dict], mode: str | None) -> tuple[str, str, dict]:
    """THE ONLY MODEL CALL SITE IN THIS MODULE (contract §9.21).

    Every generation — orchestrator, each specialist, the final analyst, and both firmer retries —
    lands here and runs inside `lease.acquire_interactive()`, the cross-process `fcntl.flock` on
    `{READS_DIR}/.model.lock`. Two concurrent `POST /api/analyst/{ticker}` requests therefore
    serialise even when they are served by different OS processes, which is the failure mode a
    `threading` lock cannot touch, and the reason Wave 2 built the lease.

    The lease is taken UNCONDITIONALLY, before the call — including on the path where every provider
    is down and `llm.py` returns a deterministic stub. We cannot know in advance which it will be,
    and a lease taken only when a real generation happens is a lease with a race in it.

    Returns `(raw, model_used, meta)` where `meta` is `modes.ModeResult.as_dict()`-shaped. An
    injected `call_model` may return the 2-tuple `llm.call_model` returns; the mode metadata is then
    read from `llm.last_mode_meta()`, so the mode RECORDED is always the mode ACTUALLY USED.
    """
    with lease.acquire_interactive():
        out = call_model(messages, mode=mode)
    if isinstance(out, tuple) and len(out) == 3:
        raw, model_used, meta = out
        return raw, model_used, dict(meta or {})
    raw, model_used = out
    return raw, model_used, dict(_llm.last_mode_meta() or {})


def _run_step(role: str, call_model, safe_json, build, validate, needs_retry, stub) -> dict:
    """One agent: structured attempt → ONE firmer retry on structural failure → deterministic stub.
    The same contract as `main.py::read` and every other model call in this service."""
    mode = modes.mode_for(ROLE_TASK[role])
    raw, model_used, meta = _generate(call_model, build(firmer=False), mode)
    obj = safe_json(modes.strip_thinking(raw or ""))
    warnings = validate(obj)
    retried = False

    if not str(model_used).startswith("stub:") and needs_retry(warnings):
        retried = True
        raw, model_used, meta = _generate(call_model, build(firmer=True), mode)
        obj = safe_json(modes.strip_thinking(raw or ""))
        warnings = validate(obj)

    stubbed = False
    flagged = banned_analyst_hits(obj) if obj is not None else []
    if obj is None or needs_retry(warnings) or flagged:
        obj = stub()
        stubbed = True
        if not str(model_used).startswith("stub:"):
            # `_stub_label`'s convention, preserved: the record must show a stub was used.
            model_used = f"{model_used} (fell back to stub)"
        # The stub's own warnings, PLUS whatever got the model's reply thrown away. Recomputing
        # from the replacement alone would erase the evidence that a banned phrase was ever
        # produced, and §9.19 exists precisely so that is visible.
        warnings = validate(obj) + flagged

    if isinstance(obj.get("confidence"), (int, float)):
        obj["confidence"] = max(0.0, min(1.0, float(obj["confidence"])))

    # PROJECT to the keys this role's schema declares, and nothing else. Validation ran on the raw
    # reply above — so a volunteered `score` is still WARNED about (§9.17) — but it does not survive
    # into the record. A model that appends a field nobody asked for is not adding information, it
    # is adding an audit trail entry describing something the pipeline did not do.
    allowed = _ROLE_ALLOWED.get(role)
    if allowed:
        obj = {k: v for k, v in obj.items() if k in allowed or k == "_stub"}

    return {
        "role": role, "object": obj, "modelUsed": model_used, "warnings": warnings,
        "retried": retried, "stubbed": stubbed,
        "reasoningModeRequested": meta.get("reasoningModeRequested", mode),
        # THE ACTUAL MODE, never the requested one (invariant #8). A server that silently rejects
        # `enable_thinking: false` is visible here, and the ablation stays attributable.
        "reasoningMode": meta.get("reasoningModeUsed"),
        "reasoningModeDegraded": bool(meta.get("degraded", False)),
        "transport": meta.get("transport"),
    }


# ═══════════════════════════════════════════════════════════════ tool calls

def _event_since(as_of: str | None) -> str:
    """Return the deterministic thirty-day event window required by `get_ticker_events`."""
    base = datetime.now(timezone.utc)
    if as_of:
        try:
            base = datetime.fromisoformat(as_of.replace("Z", "+00:00")).astimezone(timezone.utc)
        except (TypeError, ValueError):
            pass
    return (base - timedelta(days=30)).strftime("%Y-%m-%dT%H:%M:%SZ")


def _first_event_id(ctx: dict, results: list[dict]) -> str | None:
    """Find one served event id for the dependent `get_event_details` call."""
    candidates: list[Any] = []
    for item in results:
        envelope = item.get("result") if isinstance(item, dict) else None
        if isinstance(envelope, dict):
            payload = envelope.get("result")
            if isinstance(payload, dict):
                candidates.extend(payload.get("events") or [])
    for key in ("events", "news"):
        value = ctx.get(key)
        if isinstance(value, dict):
            candidates.extend(value.get("events") or [])
        elif isinstance(value, list):
            candidates.extend(value)
    for event in candidates:
        if not isinstance(event, dict):
            continue
        event_id = event.get("eventId") or event.get("id")
        if isinstance(event_id, str) and re.fullmatch(r"evt_[0-9a-f]{16}", event_id):
            return event_id
    return None


def _tool_args(tool: str, ctx: dict, as_of: str | None,
               prior_results: list[dict]) -> dict | None:
    ticker = str(ctx.get("ticker") or "").upper()
    horizon = ctx.get("horizon") if ctx.get("horizon") in HORIZON_FIELD else "5d"
    if tool in ("get_user_subscriptions", "get_macro_context", "get_market_context"):
        return {}
    if tool == "get_ticker_context":
        return {"ticker": ticker, "horizon": horizon}
    if tool == "get_multi_timeframe_context":
        return {"ticker": ticker, "horizon": horizon}
    if tool == "get_candles":
        return {"ticker": ticker, "timeframe": "1D", "limit": 60}
    if tool == "get_ticker_events":
        return {"ticker": ticker, "since": _event_since(as_of)}
    if tool == "get_event_details":
        event_id = _first_event_id(ctx, prior_results)
        return {"eventId": event_id} if event_id else None
    return None

def _run_tools(role: str, ctx: dict, as_of: str | None, budget: list[int]) -> tuple[list, list, bool]:
    """Execute this role's tool slice through Lane 3A's `execute(tool, args, ctx)`.

    A terminal tool failure is EVIDENCE, not an abort (Phase 3): it is recorded in the trace, the
    affected layer is marked unclear by the caller, and the run continues. Returns
    `(trace, results, any_success)`; the trace is §5's `evidence.toolCalls` shape.
    """
    trace: list[dict] = []
    results: list[dict] = []
    ok = False
    if _tools_execute is None:
        return trace, results, ok
    for tool in _registry_for(role)[:MAX_TOOL_CALLS_PER_SPECIALIST]:
        if budget[0] <= 0:
            trace.append({"tool": tool, "args": {}, "status": "skipped: run budget exhausted"})
            break
        args = _tool_args(tool, ctx, as_of, results)
        if args is None:
            trace.append({"tool": tool, "args": {}, "status": "skipped: dependency unavailable"})
            continue
        budget[0] -= 1
        entry = {"tool": tool, "args": args}
        try:
            res = _tools_execute(tool, args, {**ctx, "asOf": as_of})
            results.append({"tool": tool, "result": res})
            if isinstance(res, dict) and res.get("ok") is True:
                entry["status"] = "ok"
                ok = True
            else:
                error = res.get("error") if isinstance(res, dict) else None
                code = error.get("code") if isinstance(error, dict) else "invalid_result"
                entry["status"] = f"failed: {code}"
        except Exception as exc:  # terminal failure after 3A's own second attempt
            entry["status"] = f"failed: {type(exc).__name__}"
        trace.append(entry)
    return trace, results, ok


# ═══════════════════════════════════════════════════════════════ §9.17 — the computed score

def compute_score(direction: str, bucket: str) -> float:
    """§9.17, verbatim: `score = STANCE_NUM[direction] * CONFIDENCE_WEIGHT[confidenceBucket]`, with
    `STANCE_NUM` IMPORTED from `committee.py` rather than re-derived.

    `unclear` is handled with `.get(direction, 0.0)` and this is a deliberate, recorded reading of a
    gap in the clause: §7's direction enum has four values, `STANCE_NUM` has three, and `unclear` is
    absent from it. Adding it there would edit `committee.py`, which invariant #6 forbids. 0.0 is
    the only defensible value — `unclear` is an abstention, and an abstention has no directional
    magnitude, exactly as `neutral` (whose `STANCE_NUM` IS 0.0) does not. The handoff carries
    proposed clause text.
    """
    return round(STANCE_NUM.get(direction, 0.0) * CONFIDENCE_WEIGHT.get(bucket, 0.0), 4)


def _abstain(direction: str) -> bool:
    return direction in ("neutral", "unclear")


# ═══════════════════════════════════════════════════════════════ §7 assembly

def _reimpose_levels(obj: dict, levels: dict) -> None:
    """Invariant #3 / §7's binding rule, in code. UNCONDITIONAL, exactly as `main.py::read`
    overwrites `keyLevels`: whatever the model said about a level is discarded and the values
    `services/analysis` computed (`swing_levels` via `detect_regime`, or §4.3's
    `structure.supports/resistances`) are written over the top. Never trust a model level."""
    obj["support"] = list(levels.get("support") or [])
    obj["resistance"] = list(levels.get("resistance") or [])


def _levels_from_context(ctx: dict) -> dict:
    """The ONLY authorities on a price level are `services/analysis`'s outputs. Prefers §4.3's
    `structure` (Lane 3A's `/technical-context`), falls back to the `regime.keyLevels` shape that
    exists on this base today and that `prompt.py::_facts` already reads."""
    tc = ctx.get("technicalContext") or {}
    struct = tc.get("structure") if isinstance(tc, dict) else None
    if isinstance(struct, dict) and isinstance(struct.get("structure"), dict):
        struct = struct["structure"]
    if isinstance(struct, dict) and (struct.get("supports") or struct.get("resistances")):
        return {"support": struct.get("supports") or [],
                "resistance": struct.get("resistances") or []}
    kl = (ctx.get("regime") or {}).get("keyLevels")
    if isinstance(kl, dict):
        return {"support": kl.get("support") or [], "resistance": kl.get("resistance") or []}
    return {"support": [], "resistance": []}


def _resolve_direction(model_direction: Any, layers: dict,
                       consensus: dict) -> tuple[str, str, str]:
    """THE DETERMINISTIC ROUTE TO `neutral` AND `unclear` — reached WITHOUT the stub.

    Order matters, and it is the contract's:

    1. A REQUIRED evidence layer unavailable at the cutoff ⇒ `unclear`. §7's binding rule and
       Phase 3's: "a missing layer must be able to produce `unclear`, not a fabricated reading".
       The model may have produced a perfectly well-formed bullish object; it was reasoning over a
       hole, so the server overrides it.
    2. Specialist stances that CONFLICT ⇒ `neutral`. `compute_consensus`'s own `mixed` label is the
       mapping (|score| ≤ 0.15), imported rather than re-derived.
    3. Otherwise the model's direction stands, provided it is in the enum; a value outside the enum
       falls back to the consensus label, and a `no-stance` consensus is `unclear`.

    Returns `(direction, reason, branch)`. `branch` is one of `DIRECTION_BRANCHES` and is the
    MACHINE-READABLE form of `reason`: §9.56 keys the server-authored thesis line off it, and
    keying off the prose would make `directionReason`'s wording load-bearing. `reason` stays
    exactly as it was — tests and the handoff record read it.
    """
    missing = [k for k in REQUIRED_EVIDENCE_LAYERS if not layers.get(k)]
    if missing:
        return ("unclear",
                f"required evidence layer unavailable at the cutoff: {', '.join(missing)}",
                "missing-layer")

    label = consensus.get("label")
    if label == "mixed":
        return ("neutral", "specialist stances conflict (compute_consensus label 'mixed')",
                "mixed")
    if label == "no-stance":
        return "unclear", "no specialist produced a usable stance", "no-stance"

    if model_direction in DIRECTIONS:
        return (str(model_direction),
                "model direction, evidence complete and consensus not mixed", "model")
    return ({"bullish-lean": "bullish", "bearish-lean": "bearish"}.get(label, "unclear"),
            f"model direction {model_direction!r} outside the enum; fell back to consensus {label!r}",
            "out-of-enum")


def _server_thesis(direction: str, branch: str, horizon: str) -> str:
    """§9.56. The prose the SERVER writes when the SERVER resolved the direction.

    Two sentences: what the run resolved to, and why it resolved there. Neither contains a number
    (invariant #3 — the server does not author levels into narrative either) and neither contains
    a `BANNED_ANALYST` phrase; `test_analyst.py` asserts both against every branch/direction pair.
    """
    lead = _SERVER_THESIS_DIRECTION.get(direction, _SERVER_THESIS_DIRECTION["unclear"])
    return f"{lead.format(horizon=horizon)} {_SERVER_THESIS_REASON[branch]}".strip()


#: §9.58. What replaces prose that cited a level the server did not compute. The DIRECTION still
#: stands on this path — only the rationale is withdrawn — so the line says the served direction and
#: then says plainly that the reasoning was withheld and why. A visible withdrawal is the point: a
#: silent drop would make the defect uncountable, which is the failure mode §9.58 binding 4 exists
#: to watch. Neither line names a number; the horizon phrase is a quantity of PERIODS.
_WITHHELD_THESIS_REASON = (
    "The model's rationale is not shown for this run: it cited a price level this run did not "
    "compute, and only a computed level may be stated."
)

_WITHHELD_INVALIDATION = (
    "No falsification condition is stated for this run: the model's condition cited a price level "
    "this run did not compute, and only a computed level may be stated."
)


def _server_owns_prose(direction: str, branch: str) -> bool:
    """§9.56 bindings 1 and 9 in one predicate: may the server author `thesis` for this run?

    Two ways it may. The server RESOLVED the direction (any branch but `"model"`), which is
    binding 1. Or the served `direction` ABSTAINS, whoever resolved it — binding 9, which overruled
    the original "warn, do not rewrite" handling of the model-owned branch. The standing objection
    was that the server has no business authoring prose for a direction it did not resolve; the
    answer is that it is not authoring a direction. The served field says `neutral`, and a sentence
    asserting NO lean is precisely what `neutral` means, so writing it invents nothing.

    **This is the detection approach, and it detects nothing** — that is the point. The condition
    is the served `direction`, never a reading of the prose. A prose-reading detector was the
    original design and it is the thing binding 9 forbids: `BANNED_ANALYST` already demonstrated
    that a phrase list misses the sentence that matters, and the sentence that started all of this
    ("Structure and flow both point the same way over this horizon") contains no blacklisted word.

    False-negative shape: **none for the user-visible defect.** Every abstaining run is
    server-authored whether or not its prose leans, so no rendered `thesis` can contradict an
    abstaining badge by any route. The residual false negative moved to the audit trail — see
    `_ABSTAIN_CONTRADICTIONS`, where a lean stated without the words "bullish"/"bearish" produces
    no warning. It is no longer rendered, so it costs a note and never a defect.

    The price, paid knowingly and already accepted by binding 2: model prose that describes an
    abstention perfectly well is discarded on that branch too. `modelThesis` keeps every discarded
    sentence, so §9.20's ablation loses nothing.
    """
    return branch != "model" or _abstain(direction)


def _server_invalidation(direction: str) -> str:
    """§9.56 binding 7. The prose the SERVER writes for `invalidation` when it owns the prose.

    Keyed on the resolved DIRECTION rather than the branch, because the two cases differ in what
    is true: an abstention has no thesis to falsify, while a server-resolved directional read has
    a thesis the model did not write and whose falsification condition the model therefore did not
    author either. Both say no falsification condition is stated; only one of them may say no
    direction was resolved.
    """
    return _SERVER_INVALIDATION_ABSTAIN if _abstain(direction) else _SERVER_INVALIDATION_OVERRIDE


def _withheld_thesis(direction: str, horizon: str) -> str:
    """§9.58 binding 2, the served-directional-read case. The direction stands and the rationale is
    withdrawn, so the line states the direction it is withdrawing the rationale FOR — anything less
    would read as an abstention and contradict the served field, which is §9.56's whole subject."""
    lead = _SERVER_THESIS_DIRECTION.get(direction, _SERVER_THESIS_DIRECTION["unclear"])
    return f"{lead.format(horizon=horizon)} {_WITHHELD_THESIS_REASON}".strip()


#: §9.56 binding 8, scoped by §9.58 binding 3. The ONLY exempt numeric literal is **a quantity of
#: PERIODS** — "the next 5 trading days", "earnings in 6 sessions". Not a quantity of anything else:
#: not shares, not dollars, not percent, not a count of analysts or filings. The server's own thesis
#: line carries a period count by construction (`HORIZON_FIELD` is "5_trading_days"), which is the
#: entire reason the exemption exists, so it is drawn exactly that wide and no wider. Everything
#: else is a price claim and must be a served level.
#:
#: Sub-day units are deliberately absent: this pipeline's horizons are trading days and longer
#: (`HORIZON_FIELD`), and "closes above 173 in 30 minutes" is a price claim wearing a clock.
_TIME_QUANTITY = re.compile(
    r"\d+(?:\.\d+)?\s*(?:trading\s+)?"
    r"(?:session|day|week|month|quarter|year)s?\b",
    re.IGNORECASE,
)
_NUMERIC_LITERAL = re.compile(r"\d+(?:\.\d+)?")


def unserved_numbers(text: Any, levels: dict) -> list[str]:
    """§9.56 binding 8. Every numeric literal in RENDERED prose that is not a served level.

    Invariant #3 says the model never invents a number and the server re-imposes `keyLevels` — but
    `_reimpose_levels` only overwrites the `support`/`resistance` ARRAYS. Free prose was never
    inspected, so a level in a sentence has always been whatever the model said. Measured: the
    fixture's *"daily close below 173.80 on above-average volume"* IS a member of the served
    support set on a full-context run (`[181.0, 173.8]`) and is NOT on the `unclear` run §9.56
    measured, whose context carries no `regime` at all and whose served set is therefore empty.
    The first is a coincidence of the fixture, not a protection: nothing compared them.

    `modelThesis` and `modelInvalidation` are deliberately NOT swept — they are audit records of
    what the model said, and sanitising them would destroy their only purpose.
    """
    served = [float(v) for v in
              list(levels.get("support") or []) + list(levels.get("resistance") or [])
              if isinstance(v, (int, float)) and not isinstance(v, bool)]
    scrubbed = _TIME_QUANTITY.sub(" ", str(text or ""))
    out: list[str] = []
    for literal in _NUMERIC_LITERAL.findall(scrubbed):
        value = float(literal)
        if not any(abs(value - s) < 1e-9 for s in served):
            out.append(literal)
    return out


def _audit(model_used: str, reasoning_mode: str | None, prompt_version: str,
           temperature: float | None) -> dict:
    """§7's `audit`, as Lane 1D's SEVEN-key `identity` block (§9.40) — a superset of §7's example
    four, so invariant #7's stamps all travel with every artefact and `temperature` lives in exactly
    one place (`generationSettings`). Version control is 1D's; nothing is re-implemented here."""
    return identity_stamp(
        model_used,
        reasoning_mode=reasoning_mode,
        temperature=temperature,
        prompt_version=prompt_version,
        schema_version=SCHEMA_VERSION_THESIS,
        tool_schema_version=TOOL_SCHEMA_VERSION,
    )


# ═══════════════════════════════════════════════════════════════ the orchestrated run

def run_analyst(
    ticker: str,
    ctx: dict | None = None,
    call_model: Callable | None = None,
    safe_json: Callable | None = None,
    *,
    as_of: str | None = None,
    horizons: Iterable[str] | None = None,
) -> dict:
    """Run the pipeline SEQUENTIALLY — one model call in flight at a time, each inside the lease.

    `as_of` comes from the REQUEST and is injected by the runtime; the orchestrator never chooses it
    (§1, Phase 3). Every response echoes the resolved `asOf` it actually used (§1.4).
    """
    ctx = _normalize_context(dict(ctx or {}))
    ctx.setdefault("ticker", ticker)
    if as_of is None and isinstance(ctx.get("asOf"), str) and ctx.get("asOf"):
        # A live gateway request resolves its empty request cutoff to the timestamp actually used
        # by the data services. Echo that resolved instant throughout the run.
        as_of = ctx["asOf"]
    call_model = call_model or _default_call_model
    safe_json = safe_json or _llm.safe_json
    keys = [k for k in (horizons or HORIZON_KEYS) if k in HORIZON_FIELD] or list(HORIZON_KEYS)

    layers = _available_layers(ctx)
    budget = [MAX_TOOL_CALLS_PER_RUN]
    tool_calls: list[dict] = []
    warnings: list[str] = []
    stages: dict[str, dict] = {}

    # ── 1. Orchestrator (non-thinking) — routing only, and it never picks the cutoff.
    orch_trace, orch_results, _ = _run_tools("orchestrator", ctx, as_of, budget)
    tool_calls.extend(orch_trace)
    warnings.extend(
        f"orchestrator: tool {t['tool']} {t['status']}"
        for t in orch_trace if str(t.get("status", "")).startswith("failed")
    )
    orch = _run_step(
        "orchestrator", call_model, safe_json,
        build=lambda firmer: build_orchestrator_prompt(
            ticker, ctx, as_of, firmer, orch_results),
        validate=lambda o: _analyst_validate("orchestrator", o),
        needs_retry=_needs_agent_retry,
        stub=lambda: {"specialists": ["technical", "news", "macro"],
                      "why": "Model offline — all specialists run.", "_stub": True},
    )
    stages["orchestrator"] = orch
    warnings.extend(f"orchestrator: {w}" for w in orch["warnings"])

    chosen = [s for s in (orch["object"].get("specialists") or []) if s in ("technical", "news", "macro")]
    if not chosen:
        chosen = ["technical", "news", "macro"]

    # ── 2. Specialists, one at a time. A specialist that is not run has no stance, which is a fact
    #       the consensus and the layer map both carry honestly.
    specialists: dict[str, dict] = {}
    conflict: dict | None = None
    for role in ("technical", "news", "macro"):
        if role not in chosen:
            continue
        trace, results, _ = _run_tools(role, ctx, as_of, budget)
        tool_calls.extend(trace)
        failed = [t for t in trace if str(t.get("status", "")).startswith("failed")]
        if failed:
            warnings.extend(f"{role}: tool {t['tool']} {t['status']}" for t in failed)

        step = _run_step(
            role, call_model, safe_json,
            build=lambda firmer, r=role, res=results: build_specialist_prompt(
                r, ticker, ctx, as_of, firmer, res),
            validate=lambda o, r=role: _analyst_validate(r, o),
            needs_retry=_needs_agent_retry,
            stub=lambda r=role: stub_specialist(r, ctx),
        )
        stages[role] = step
        specialists[role] = step["object"]
        warnings.extend(f"{role}: {w}" for w in step["warnings"])

    # ── 2b. News vs technical CONFLICT (doc §8: non-thinking for extraction, THINKING for the
    #        conflict). Runs only when the two layers actually disagree — a thinking pass on
    #        agreeing evidence is variance for nothing, which is the whole point of the mode table.
    tech_stance = (specialists.get("technical") or {}).get("stance")
    news_stance = (specialists.get("news") or {}).get("stance")
    if (tech_stance in ("bullish", "bearish") and news_stance in ("bullish", "bearish")
            and tech_stance != news_stance):
        step = _run_step(
            "news_conflict", call_model, safe_json,
            build=lambda firmer: build_conflict_prompt(ticker, specialists, as_of, firmer),
            validate=lambda o: _analyst_validate("news_conflict", o),
            needs_retry=_needs_agent_retry,
            stub=lambda: {"weighting": "unresolved", "resolution":
                          "Model offline — the technical/news conflict was not weighed.",
                          "_stub": True},
        )
        stages["news_conflict"] = step
        conflict = step["object"]
        warnings.extend(f"news_conflict: {w}" for w in step["warnings"])

    # The deterministic combination is Python's, never the model's (decision 6). `compute_consensus`
    # is imported from committee.py; the arithmetic exists once in this repo.
    #
    # It runs over the THREE SPECIALISTS ONLY. The conflict note is a weighting, not a fourth vote —
    # letting it into the tally would let a `stance` key the conflict schema does not even define
    # tip a genuinely mixed committee off `mixed`, and `mixed` is the route to `neutral`.
    consensus = compute_consensus(specialists)
    final_inputs = dict(specialists)
    if conflict is not None:
        final_inputs["conflict"] = conflict

    levels = _levels_from_context(ctx)

    # The final analyst receives the two envelope-backed summary tools once per run, not once per
    # horizon. Repeating identical reads four times would add no evidence and would waste budget.
    final_trace, final_tool_results, _ = _run_tools("final", ctx, as_of, budget)
    tool_calls.extend(final_trace)
    warnings.extend(
        f"final: tool {t['tool']} {t['status']}"
        for t in final_trace if str(t.get("status", "")).startswith("failed")
    )

    # ── 3. Final analyst, once per horizon. `evidence`, `sources` and `audit` are SHARED across the
    #       four objects by construction; `direction`, `confidenceBucket`, `invalidation` and
    #       `riskFlags` are what differ.
    theses: dict[str, dict] = {}
    forecast_horizons: dict[str, dict] = {}
    #: §9.58 binding 4. Every suppression, itemised so it can be counted AND diagnosed. Rare is the
    #: expected state; common means the defect is upstream — in the final prompt, or in what the
    #: specialists hand the model — and a rendering-layer replacement is hiding it. That is a §9.17
    #: finding to RAISE, not to absorb. Diagnostic, like `warnings`: never rendered to a user.
    suppressions: list[dict] = []
    # ONE read of the verdict file per analyst run, not one per horizon (§9.61). Four reads would be
    # four chances for the file to change mid-run and for two theses in the same envelope to
    # disagree about whether the ladder has run.
    verdict_table = ablation_verdict.load_verdicts()
    for key in keys:
        step = _run_step(
            "final", call_model, safe_json,
            build=lambda firmer, k=key: build_final_prompt(
                ticker, k, final_inputs, levels, as_of, layers, firmer,
                final_tool_results),
            validate=validate_thesis,
            needs_retry=needs_thesis_retry,
            stub=lambda: stub_thesis(ticker, layers, consensus),
        )
        warnings.extend(f"final[{key}]: {w}" for w in step["warnings"])

        # PROJECT to the model-owned §7 keys and nothing else. A model that volunteers extra fields
        # — a `score`, a `probability`, a `priceTarget` — does not get them into the record: §9.17
        # says the float is the server's, and an audit field that describes something the pipeline
        # did not do is worse than a missing one. This is the same posture as `main.py::read`
        # overwriting `keyLevels`, applied to the whole object rather than one field.
        src = step["object"]
        stages["final"] = step
        obj = {k: src[k] for k in THESIS_KEYS if k in src}
        for k, default in (("regime", "unavailable"), ("thesis", ""), ("invalidation", ""),
                           ("riskFlags", []), ("whatChanged", []), ("sources", [])):
            obj.setdefault(k, default)

        direction, reason, branch = _resolve_direction(obj.get("direction"), layers, consensus)
        obj["direction"] = direction
        bucket = obj.get("confidenceBucket")
        if bucket not in CONFIDENCE_BUCKETS:
            bucket = "low"
        if direction == "unclear":
            # An abstention on absent evidence is not a high-confidence anything.
            bucket = "low"
        obj["confidenceBucket"] = bucket

        # Evidence layers with no evidence read "unclear", regardless of what the model wrote.
        ev = obj.get("evidence") if isinstance(obj.get("evidence"), dict) else {}
        obj["evidence"] = {k: ("unclear" if not layers.get(k) else ev.get(k, "neutral"))
                           for k in EVIDENCE_LAYERS}

        _reimpose_levels(obj, levels)  # invariant #3 — unconditional, after the model replies

        obj["ticker"] = ticker
        obj["asOf"] = as_of
        obj["horizon"] = HORIZON_FIELD[key]
        obj["directionReason"] = reason
        obj["directionBranch"] = branch

        # §9.61 — THE ABLATION VERDICT, SERVED. §9.20 gates the `direction` word in the BROWSER on
        # a verdict stored on the SERVER, so the server has to hand it over or the gate can never
        # open. Absent is the ordinary state and stays renderable as "not yet validated"; the key
        # is omitted entirely rather than set to null, because §9.61 binding 4 makes
        # `validated: false` a DIFFERENT answer from an absent verdict and a null would blur them.
        # `key` (`1d`/`5d`/…) is the horizon vocabulary the verdict file is written against, not
        # `HORIZON_FIELD[key]`, which is §7's long form — §9.11 keeps one mapping, in one place.
        served_verdict = ablation_verdict.verdict_for(key, verdicts=verdict_table)
        if served_verdict is not None:
            obj["ablationVerdict"] = served_verdict

        # §9.56 — the SAME AUTHORITY writes the direction and the prose describing it. This is
        # the exact posture of `_reimpose_levels` two lines above and of `main.py::read`
        # overwriting `keyLevels`: the replaced sentences are kept for audit, and the record
        # carries the server's. `modelThesis`/`modelInvalidation` are set ONLY when a replacement
        # happened, so their PRESENCE is the audit signal — an always-present duplicate would carry
        # no information and would invite a renderer into reading the wrong key. Contract: render
        # `thesis` and `invalidation`, never the `model*` pair. They hold whatever WOULD have been
        # rendered: usually the model's sentence, or the deterministic stub's when `_run_step` had
        # already discarded the model's reply.
        #
        # Binding 9 widened the condition from "the server overrode" to `_server_owns_prose`, which
        # also covers the branch where the MODEL owns the direction and abstains in it. That branch
        # reached the identical user-visible defect — confident prose beside an abstention — through
        # a door the override does not watch, and a `warnings` entry does not stop rendering.
        #
        # Binding 7 puts `invalidation` under the same rule, and it is the worse case: a
        # falsification condition attached to an abstention is a category error that also names a
        # price. It moves with `thesis` and by the same condition, never separately — a served
        # thesis beside a model's falsification condition is the same defect wearing the other coat.
        if _server_owns_prose(direction, branch):
            model_thesis = obj.get("thesis") or ""
            model_invalidation = obj.get("invalidation") or ""
            obj["thesis"] = _server_thesis(direction, branch,
                                           HORIZON_FIELD[key].replace("_", " "))
            obj["invalidation"] = _server_invalidation(direction)
            if model_thesis:
                obj["modelThesis"] = model_thesis
            if model_invalidation:
                obj["modelInvalidation"] = model_invalidation

        if branch == "model" and _abstain(direction):
            # Binding 5's warning, kept by binding 9 even though it no longer gates anything: that
            # the model produced prose contradicting its own abstention is evidence about the model,
            # and the ablation wants it. The prose it describes is in `modelThesis`, not rendered.
            low = str(obj.get("modelThesis") or "").lower()
            for word in _ABSTAIN_CONTRADICTIONS:
                if word in low:
                    warnings.append(
                        f"final[{key}]: model thesis asserts {word!r} while direction is "
                        f"{direction!r} — prose and direction disagree, and no server override "
                        f"fired to reconcile them")
        obj["audit"] = _audit(step["modelUsed"], step["reasoningMode"], PROMPT_VERSION_FINAL,
                              None if str(step["modelUsed"]).startswith("stub:") else 0.2)
        obj["disclaimer"] = ANALYST_DISCLAIMER  # §9.18 — adjacent to the output it qualifies

        # The `model*` audit copies are swept TOO. They cannot carry a banned phrase today —
        # `_run_step` replaces the WHOLE object with the stub on any `banned_analyst_hits`, so a
        # banned sentence never reaches this loop — but §9.56 introduced further prose keys, and a
        # sweep that names only some of them silently stops being complete the day that gate is
        # relaxed.
        hits = banned_analyst_hits({k: obj.get(k) for k in
                                    ("thesis", "modelThesis", "invalidation", "modelInvalidation",
                                     "riskFlags", "transmission", "possibleReadThrough")})
        if hits:
            warnings.extend(f"final[{key}]: {h}" for h in hits)

        # §9.56 binding 8, ENFORCED BY §9.58 — a rendered number must be a served number, and prose
        # that is not is SUPPRESSED rather than warned about. Binding 8 shipped as a warning plus a
        # test, and a warning does not stop rendering: the same gap binding 9 closed for prose, and
        # worse here, because what reaches the screen is a PRICE. Invariant #3 is not "the model's
        # numbers are audited", it is that the model never invents a number.
        #
        # This can only fire where the MODEL owns the prose — every server line is built from
        # `_SERVER_*` constants whose only numeric literal is a period count. It is therefore the
        # narrowest possible reach into model output: the direction is untouched, and on a served
        # directional read only the rationale is withdrawn.
        for field, replacement in (
            ("thesis", lambda: (_server_thesis(direction, branch,
                                               HORIZON_FIELD[key].replace("_", " "))
                                if _abstain(direction)
                                else _withheld_thesis(direction,
                                                      HORIZON_FIELD[key].replace("_", " ")))),
            ("invalidation", lambda: (_server_invalidation(direction) if _abstain(direction)
                                      else _WITHHELD_INVALIDATION)),
        ):
            stray = unserved_numbers(obj.get(field), levels)
            if not stray:
                continue
            # The warning is KEPT (§9.58 binding 1) — suppression removes the defect from the
            # screen, not the evidence that it happened, and binding 4 counts these.
            warnings.append(
                f"final[{key}]: {field} stated {', '.join(stray)}, which is not a member of the "
                f"served keyLevels for this run — §9.17-class, suppressed and replaced with a "
                f"server-authored line")
            # `setdefault`: on a server-owned run the audit key already holds the MODEL's original,
            # and overwriting it with the server line this replaces would destroy the record.
            obj.setdefault(f"model{field[0].upper()}{field[1:]}", obj.get(field) or "")
            obj[field] = replacement()
            suppressions.append({"horizon": HORIZON_FIELD[key], "field": field,
                                 "literals": stray, "directionBranch": branch})

        theses[key] = obj
        forecast_horizons[key] = {
            "direction": direction,
            "score": compute_score(direction, bucket),   # §9.17 — computed, never generated
            "abstain": _abstain(direction),
        }

    stage_models = {r: s["modelUsed"] for r, s in stages.items()}
    # Wave 5C. `retried` and `stubbed` were computed per stage and then thrown away, so
    # "how often does a real model fail the schema, and did the ONE firmer retry rescue it?" was
    # unanswerable from outside this function. That is precisely the measurement `app.smoke` exists
    # to bring back from the prod box, and it cannot be reconstructed from `modelsUsed` alone: a
    # stage that failed once and passed on the retry is byte-identical there to one that passed
    # first time. Additive, and read by nothing that renders.
    stage_outcomes = {r: {"retried": bool(s.get("retried")), "stubbed": bool(s.get("stubbed"))}
                      for r, s in stages.items()}
    modes_used = {r: {"requested": s["reasoningModeRequested"], "used": s["reasoningMode"],
                      "degraded": s["reasoningModeDegraded"], "transport": s["transport"]}
                  for r, s in stages.items()}

    return {
        "ticker": ticker,
        "asOf": as_of,                       # §1.4 — echo the cutoff actually used
        "horizons": list(keys),
        "theses": theses,                    # one §7 object per horizon
        "forecast": {                        # §5's shape, assembled by Lane 3C
            "horizons": forecast_horizons,
            "confidenceBucket": theses[keys[0]]["confidenceBucket"] if keys else "low",
            "thesis": theses[keys[0]]["thesis"] if keys else "",
            "invalidation": theses[keys[0]]["invalidation"] if keys else "",
        },
        "specialists": specialists,
        "consensus": consensus,
        "evidence": {"toolCalls": tool_calls, "layersAvailable": layers},
        "identity": _audit(stage_models.get("final", "unknown"),
                           (stages.get("final") or {}).get("reasoningMode"),
                           PROMPT_VERSION_FINAL, 0.2),
        # Wave 5C. WHICH RUNTIME PRODUCED THIS ENVELOPE, beside `identity` and never inside it
        # (§9.40 pins identity at seven keys). `runtime.served == "stub:offline"` means a FIXTURE
        # produced every field above, and no claim about model behaviour may cite it — a green test,
        # a passing gate and a rendered screenshot each carry the runtime they ran against.
        "runtime": _llm.runtime_block(stage_models.get("final")),
        "promptVersions": {
            "orchestrator": PROMPT_VERSION_ORCHESTRATOR,
            "technical": PROMPT_VERSION_TECHNICAL,
            "news": PROMPT_VERSION_NEWS,
            "macro": PROMPT_VERSION_MACRO,
            "final": PROMPT_VERSION_FINAL,
        },
        "modelsUsed": stage_models,
        "stageOutcomes": stage_outcomes,   # Wave 5C — the schema-failure / firmer-retry measurement
        "reasoningModes": modes_used,
        "warnings": warnings,
        "proseSuppressions": suppressions,   # §9.58 binding 4 — countable, never rendered
        "disclaimer": ANALYST_DISCLAIMER,
    }


# ═══════════════════════════════════════════════════════════════ the route (§9.28 seam)

class AnalystRequest(BaseModel):
    ticker: str
    context: dict | None = None
    asOf: str | None = None
    horizon: str | None = None


@analyst_router.post("/analyst")
def analyst(req: AnalystRequest) -> dict:
    """The agentic analyst pipeline: orchestrator -> 3 specialists -> final analyst, SEQUENTIALLY,
    every generation inside the cross-process model lease (§9.21).

    ON-DEMAND ONLY (invariant #4). Nothing may poll this: no page load, ticker change, quote poll,
    alert poll or scheduler tick. Registered through the Wave 0 `analyst_router` seam, so §9.28 is
    satisfied and no `include_router` line is handed to the integrator.
    """
    horizons = [req.horizon] if req.horizon in HORIZON_FIELD else None
    return run_analyst(req.ticker.upper(), req.context or {}, as_of=req.asOf, horizons=horizons)


# Nothing below module scope may call a model. `READS_DIR` is read here only so an import-time
# misconfiguration surfaces as a lease failure at request time rather than a silent unlocked run.
_ = os.getenv("READS_DIR")
