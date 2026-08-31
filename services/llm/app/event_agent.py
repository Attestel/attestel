"""Wave 2 Lane 2B — the Qwen event agent: an already-formed event's analytical layer.

Lane 2A clusters documents into events **deterministically**. This module takes one RAW event and
asks Qwen3-14B, in non-thinking mode, for the things only a reader can supply: why it matters,
whether it is genuinely new information, likely direction, expected horizon, affected dimensions,
and a one-sentence transmission rationale per ticker.

WHAT THE MODEL MAY NOT DECIDE (contract §9.15, §9.16, locked decision 3)
------------------------------------------------------------------------
Cluster membership, `importance`, `novelty`, `relevance`, `event_type` and every timestamp are
code's. `severity` and `isNovel` are not columns and never reach the POST body: `severity`
duplicates the deterministic `importance`, and `isNovel` stays LOCAL to the worker's skip decision.
`why_it_matters` and every `potential_*` are hypotheses, stored in separate columns from facts on
purpose (doc §15.7) so a later backtest can tell evidence from interpretation.

POINT-IN-TIME (contract §9.12)
------------------------------
The prior events shown to the model for the novelty judgement are read at
`as_of = event.publishedAt + ENRICH_LAG_HOURS`, and that same value is posted back as
`enrichmentAsOf`. Never "the last 30 days from now". `enrich_worker.py` computes the cutoff; this
module only ever receives it, so there is one place the rule can be got wrong.

WHY `keyFacts` IS ALMOST ALWAYS EMPTY — measured, not chosen
-------------------------------------------------------------
§9.14 has `services/events` verify every proposed `keyFacts` string by normalised n-gram
containment against the supporting documents' `title | excerpt | body`, and **one unverified string
rejects the whole payload with 400** — taking `whyItMatters` and every `potential_*` down with it.
But no endpoint on `services/events` hands this worker those documents' text: `GET /events/{id}`'s
`documents` array is `_sources_json` (`documentId`, `provider`, `sourceTier`, `url`, `publishedAt`
— no title, no excerpt, no body), and `GET /documents` filters only by ticker / provider / since /
as_of, with no way to ask for one event's documents.

So the model is shown the event's `title`, `summary` and code-extracted `keyFacts` — which Lane 2A
extracts VERBATIM from the documents (`_extract_summary`, `_extract_key_facts`) — and a proposed
fact is kept only when it is n-gram-contained in ONE of those supplied strings. Anything contained
in one of them is contained in the events service's corpus by construction, so a payload built this
way cannot trigger §9.14's 400. Everything else is DROPPED rather than posted: losing a fact is
survivable, losing the hypotheses is not. See the handoff for the endpoint change that would
actually let the model read source text.
"""
from __future__ import annotations

import hashlib
import json
import re
import unicodedata
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone

from . import llm
from .modes import mode_for
from .prompt import BANNED  # imported, NEVER copied (invariant #3)
from .versioning import identity_stamp

__all__ = [
    "PROMPT_VERSION", "SCHEMA_VERSION", "TASK", "SCHEMA",
    "DIRECTIONS", "HORIZONS", "AFFECTED_DIMENSIONS",
    "EnrichmentResult", "enrich_event", "build_messages", "validate", "needs_retry",
    "iso", "cutoff_for", "prompt_fingerprint", "PINNED_PROMPT_FINGERPRINT",
]

# --- versions (contract §3.12's audit block uses exactly these two strings) ----------------------
PROMPT_VERSION = "event-agent@1"
SCHEMA_VERSION = "event@1"

#: doc §8's routing row this task belongs to: sentiment / relevance tagging — a fast structured
#: transformation under a strict schema with low variance. `mode_for` RAISES on an unknown key, so
#: the mode is never silently defaulted; the Wave 5A ablation depends on that.
TASK = "sentiment_tagging"

# --- the contract's enums (§3.4 is code's; these three are the model's vocabulary) ---------------
# Declared here rather than imported: `services/events` is a different service in a different
# container. `test_event_agent.py` pins each tuple against the contract text.
DIRECTIONS = ("positive", "negative", "neutral", "unclear")
HORIZONS = ("intraday", "short_term", "medium_term", "long_term")
AFFECTED_DIMENSIONS = (
    "revenue_exposure", "margin", "growth", "regulation", "management",
    "litigation", "valuation", "demand", "supply", "rates", "currency",
)

#: The exact object the model must emit. `isNovel` is LOCAL — §9.16 keeps it out of the POST body,
#: and `_to_payload` strips it. There is no `severity` and no `tickers[].relevance`.
SCHEMA = {
    "whyItMatters": "string — one or two sentences. A HYPOTHESIS about why this matters. No advice.",
    "keyFacts": ["string — quoted VERBATIM from the supplied EVENT text, or omit entirely"],
    "isNovel": "boolean — does this add information beyond the PRIOR EVENTS shown? (not stored)",
    "tickers": [{
        "ticker": "string — MUST be one of the tickers already linked to this event",
        "potentialDirection": " | ".join(DIRECTIONS),
        "potentialStrength": "number in [0,1]",
        "expectedHorizon": " | ".join(HORIZONS),
        "affectedDimensions": [" | ".join(AFFECTED_DIMENSIONS)],
        "transmission": "string — ONE sentence: why this event reaches this ticker",
    }],
}

SYSTEM = (
    "You are an event analyst. You read one already-identified market event and describe what it "
    "could mean. You are NOT a financial advisor: you NEVER give a buy, sell or hold "
    "recommendation, a price target, or a verdict on a stock. "
    "You NEVER state a number that is not already in the supplied event text — no prices, no "
    "levels, no percentages, no currency amounts, no targets. Your strength scores are ordinal "
    "judgements in [0,1], not measurements. "
    "'unclear' is a real and useful answer: use it whenever the direction genuinely is unclear, "
    "and never round it up to 'neutral' to sound decisive. "
    "You reply with a single JSON object and nothing else."
)

#: A number that would be a price, a level, a target or a return. Plain integers survive (a
#: quarter, a year, a count); anything shaped like a measurement does not. Invariant #3.
_NUMERIC_CLAIM = re.compile(
    r"""[$€£¥₩₹]\s*\d
      | \d+(?:[.,]\d+)?\s*%
      | \d+(?:[.,]\d+)?\s*(?:percent|pct|bps|basis\s+points?)\b
      | \d+\.\d+
      | \b\d[\d,]*\s*(?:usd|eur|gbp|jpy|dollars?|euros?|cents?)\b
      | \b\d[\d,]*\s*(?:billion|million|trillion|bn|mn|tn)\b
    """,
    re.IGNORECASE | re.VERBOSE,
)

# --- §9.14's verification, mirrored locally so nothing unverifiable is ever posted ----------------
KEY_FACT_NGRAM_N = 5        # services/events/app/events.py: KEY_FACT_NGRAM_N
KEY_FACT_MIN_CHARS = 20     # services/events/app/events.py: KEY_FACT_MIN_CHARS
_VERIFY_PUNCT = re.compile(r"[^\w\s]", re.UNICODE)
_VERIFY_WS = re.compile(r"\s+")

#: How many of the ticker's prior events the novelty judgement is shown. Bounded on purpose: this
#: is a context budget, and an unbounded list is how a 14B loses the schema.
PRIOR_EVENT_LIMIT = 8


def normalise_for_verification(text: str) -> str:
    """Byte-for-byte the same normalisation `services/events` applies (§9.14)."""
    folded = unicodedata.normalize("NFKC", text or "").casefold()
    return _VERIFY_WS.sub(" ", _VERIFY_PUNCT.sub(" ", folded)).strip()


def fact_is_supported(fact: str, supplied: list[str]) -> bool:
    """True when `fact`'s 5-grams are all contained in ONE of the supplied strings.

    Per-string, never against the concatenation: joining `title` and `summary` with a space mints
    n-grams that span the join and exist in no document, which would pass here and 400 there.
    """
    normalised = normalise_for_verification(fact)
    if len(normalised) < KEY_FACT_MIN_CHARS:
        return False
    words = normalised.split()
    for source in supplied:
        haystack = f" {normalise_for_verification(source)} "
        if len(words) <= KEY_FACT_NGRAM_N:
            if f" {normalised} " in haystack:
                return True
            continue
        if all(f" {' '.join(words[i:i + KEY_FACT_NGRAM_N])} " in haystack
               for i in range(len(words) - KEY_FACT_NGRAM_N + 1)):
            return True
    return False


# --- timestamps -----------------------------------------------------------------------------------
# `services/events` compares every `as_of` LEXICOGRAPHICALLY in SQL, so the width and the suffix are
# load-bearing: it emits `%Y-%m-%dT%H:%M:%SZ` (cluster.py::iso). A `+00:00` spelling of the same
# instant sorts BEFORE the `Z` spelling ('+' < 'Z'), which would make an enrichment visible at a
# cutoff it was not computed at — a leak, not a fail-safe. Mirrored here deliberately, and pinned by
# `test_event_agent.py::test_the_iso_format_matches_the_events_service_byte_for_byte`.

def parse_iso(value) -> datetime:
    if isinstance(value, datetime):
        parsed = value
    else:
        text = str(value or "").strip()
        if text.endswith("Z"):
            text = text[:-1] + "+00:00"
        parsed = datetime.fromisoformat(text)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def iso(value) -> str:
    return parse_iso(value).strftime("%Y-%m-%dT%H:%M:%SZ")


def cutoff_for(published_at, lag_hours: float) -> str:
    """§9.12's `as_of = event.published_at + ENRICH_LAG`. The ONE place the cutoff is computed."""
    return iso(parse_iso(published_at) + timedelta(hours=lag_hours))


# --- the prompt -----------------------------------------------------------------------------------

def _event_block(event: dict) -> dict:
    """Exactly what the model is shown about the event. Nothing else — no raw article bodies beyond
    the bounded excerpts D-29 permits, and none are reachable today (see the module docstring)."""
    return {
        "id": event.get("id"),
        "eventType": event.get("eventType"),
        "title": event.get("title"),
        "summary": event.get("summary"),
        "keyFacts": event.get("keyFacts") or [],
        "occurredAt": event.get("occurredAt"),
        "publishedAt": event.get("publishedAt"),
        "sourceTier": event.get("sourceTier"),
        "officialConfirmed": event.get("officialConfirmed"),
        "documentCount": event.get("documentCount"),
        "linkedTickers": [
            {"ticker": t.get("ticker"), "isPrimary": bool(t.get("isPrimary"))}
            for t in (event.get("tickers") or [])
        ],
    }


def supplied_texts(event: dict) -> list[str]:
    """The strings a proposed `keyFacts` entry may be traced to."""
    texts = [event.get("title") or "", event.get("summary") or ""]
    texts.extend(f for f in (event.get("keyFacts") or []) if isinstance(f, str))
    return [t for t in texts if t.strip()]


def _prior_block(prior_events: list[dict]) -> list[dict]:
    out = []
    for prior in (prior_events or [])[:PRIOR_EVENT_LIMIT]:
        out.append({
            "eventType": prior.get("eventType"),
            "title": prior.get("title"),
            "publishedAt": prior.get("publishedAt"),
        })
    return out


def build_messages(event: dict, prior_events: list[dict], *,
                   firmer: list[str] | None = None) -> list[dict]:
    """The two-message request. `firmer` is the retry's instruction, naming the fields that failed."""
    linked = [t.get("ticker") for t in (event.get("tickers") or []) if t.get("ticker")]
    firm = ""
    if firmer:
        firm = (
            "\n\nYOUR PREVIOUS REPLY WAS REJECTED. Fix exactly these and return the SAME schema, "
            "nothing else:\n- " + "\n- ".join(firmer)
        )
    user = (
        "EVENT:\n" + json.dumps(_event_block(event), indent=2, ensure_ascii=False) + "\n\n"
        "PRIOR EVENTS for these tickers, as known at the cutoff (may be empty):\n"
        + json.dumps(_prior_block(prior_events), indent=2, ensure_ascii=False) + "\n\n"
        "Respond with a single JSON object matching this schema exactly:\n"
        + json.dumps(SCHEMA, indent=2, ensure_ascii=False) + "\n\n"
        f"`tickers[].ticker` must be drawn from {linked!r} — you may weaken, strengthen or annotate "
        "a link, but you may NOT introduce a company that is not in that list.\n"
        "`keyFacts` must be quoted VERBATIM from the EVENT text above; if you cannot quote one "
        "exactly, return an empty array.\n"
        "Set `isNovel` to false only if the PRIOR EVENTS already contain this information.\n"
        "No buy/sell/hold, no price target, no invented number." + firm
    )
    return [{"role": "system", "content": SYSTEM}, {"role": "user", "content": user}]


# --- the versioned-prompt bump guard (1D's pattern, applied to this lane's surface) ---------------

def prompt_fingerprint() -> str:
    payload = json.dumps({"system": SYSTEM, "schema": SCHEMA}, sort_keys=True,
                         separators=(",", ":"), ensure_ascii=False)
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


#: Re-pin ONLY together with a bump of PROMPT_VERSION / SCHEMA_VERSION. `test_event_agent.py`
#: asserts the two match, so an unversioned prompt edit fails the suite.
PINNED_PROMPT_FINGERPRINT = "c38bce42eba28b5a91a8ccdadd2388c4b6e5f299b652c2829c290b82a29e7138"
PINNED_FOR = (PROMPT_VERSION, SCHEMA_VERSION)


# --- validation -----------------------------------------------------------------------------------
# Failure strings carry a prefix so `needs_retry` can mirror `prompt.needs_retry`'s semantics
# without a second vocabulary. STRUCTURAL prefixes are retryable; the rest are not.

_STRUCTURAL_PREFIXES = ("output is not", "missing key", "wrong type", "invalid enum",
                        "out of range", "empty ")


def needs_retry(failures: list[str]) -> bool:
    """Retry ONLY on structural failure, mirroring `prompt.needs_retry`.

    A BANNED phrase, an invented ticker or a numeric claim is NOT retried: those are not a model
    that misunderstood the shape, they are a model that produced something this pipeline must never
    post, and a second attempt at the same thing is not a safety mechanism.
    """
    return any(f.startswith(_STRUCTURAL_PREFIXES) for f in failures)


def validate(obj, *, linked: set[str], supplied: list[str]) -> list[str]:
    """Every rule, applied before anything is sent to :8004. Returns a list of failure strings."""
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]

    failures: list[str] = []

    why = obj.get("whyItMatters")
    if "whyItMatters" not in obj:
        failures.append("missing key: whyItMatters")
    elif not isinstance(why, str):
        failures.append("wrong type: whyItMatters must be a string")
    elif not why.strip():
        failures.append("empty whyItMatters")

    facts = obj.get("keyFacts", [])
    if not isinstance(facts, list) or any(not isinstance(f, str) for f in facts):
        failures.append("wrong type: keyFacts must be an array of strings")

    tickers = obj.get("tickers")
    if "tickers" not in obj:
        failures.append("missing key: tickers")
    elif not isinstance(tickers, list):
        failures.append("wrong type: tickers must be an array")
    elif not tickers:
        failures.append("empty tickers")
    else:
        for i, entry in enumerate(tickers):
            if not isinstance(entry, dict):
                failures.append(f"wrong type: tickers[{i}] must be an object")
                continue
            if "relevance" in entry:
                # §9.15 — code's, write-once. The endpoint would 400; refuse it here too, so the
                # reason is legible in this lane's own log rather than only in the events service's.
                failures.append(f"tickers[{i}].relevance is code's and may not be model-authored")
            symbol = str(entry.get("ticker", "")).strip().upper()
            if not symbol:
                failures.append(f"missing key: tickers[{i}].ticker")
            elif symbol not in linked:
                failures.append(
                    f"invented ticker: {symbol} is not linked to this event; the model may weaken "
                    f"or annotate a link but may not introduce a company"
                )
            for key, allowed in (("potentialDirection", DIRECTIONS),
                                 ("expectedHorizon", HORIZONS)):
                if key not in entry:
                    failures.append(f"missing key: tickers[{i}].{key}")
                elif entry[key] not in allowed:
                    failures.append(
                        f"invalid enum: tickers[{i}].{key} must be one of {list(allowed)}, "
                        f"got {entry[key]!r}"
                    )
            strength = entry.get("potentialStrength")
            if "potentialStrength" not in entry:
                failures.append(f"missing key: tickers[{i}].potentialStrength")
            elif isinstance(strength, bool) or not isinstance(strength, (int, float)):
                failures.append(f"wrong type: tickers[{i}].potentialStrength must be a number")
            elif not 0.0 <= float(strength) <= 1.0:
                failures.append(f"out of range: tickers[{i}].potentialStrength={strength!r} ∉ [0,1]")
            dimensions = entry.get("affectedDimensions", [])
            if not isinstance(dimensions, list):
                failures.append(f"wrong type: tickers[{i}].affectedDimensions must be an array")
            else:
                for dimension in dimensions:
                    if dimension not in AFFECTED_DIMENSIONS:
                        failures.append(
                            f"invalid enum: tickers[{i}].affectedDimensions member {dimension!r} "
                            f"is not in the §3.8 list"
                        )
            transmission = entry.get("transmission")
            if transmission is not None and not isinstance(transmission, str):
                failures.append(f"wrong type: tickers[{i}].transmission must be a string")
            elif isinstance(transmission, str) and _NUMERIC_CLAIM.search(transmission):
                failures.append(
                    f"numeric claim in tickers[{i}].transmission: the model never invents a number "
                    f"(CLAUDE.md invariant #3)"
                )

    if isinstance(why, str) and _NUMERIC_CLAIM.search(why):
        failures.append("numeric claim in whyItMatters: the model never invents a number "
                        "(CLAUDE.md invariant #3)")

    blob = json.dumps(obj, ensure_ascii=False).lower()
    for phrase in BANNED:
        if phrase in blob:
            failures.append(f"contains advice phrase: '{phrase}'")

    # An unsupported `keyFacts` entry is deliberately NOT a failure — `_to_payload` DROPS it.
    # §9.14 rejects the whole payload on one unverified string, and losing every hypothesis
    # because the model paraphrased a headline would be far worse than losing the fact.
    return failures


# --- the result ------------------------------------------------------------------------------------

ENRICHED = "enriched"
FAILED = "failed"
SKIPPED = "skipped"


@dataclass(frozen=True)
class EnrichmentResult:
    """What one enrichment attempt produced.

    `payload` is the exact body to POST to `/events/{id}/enrichment`, or `None` when there is
    nothing honest to post. `state` is the enrichment state the event SHOULD carry; see the handoff
    for why `failed` cannot currently be persisted.
    """

    event_id: str
    state: str
    as_of: str
    model_used: str
    reasoning_mode: str | None
    attempts: int
    payload: dict | None = None
    identity: dict = field(default_factory=dict)
    failures: tuple[str, ...] = ()
    skip_reason: str | None = None

    @property
    def audit(self) -> dict:
        return dict((self.payload or {}).get("audit") or {})


def _stamp_and_audit(model_used: str, meta: dict, *, now: str) -> dict:
    """`(identity, audit)`: 1D's seven-key `identity` block (contract §5) and the five-key `audit`
    block contract §3.12 puts on an event. Derived from `identity_stamp` so there is exactly one
    home for the version strings and for the mode that actually ran."""
    stamp = identity_stamp(
        model_used,
        reasoning_mode=meta.get("reasoningModeUsed"),
        # llm.py sends 0.2 under json_mode. A stub made no request, so it carries no setting.
        temperature=None if str(model_used).startswith("stub:") else 0.2,
        prompt_version=PROMPT_VERSION,
        schema_version=SCHEMA_VERSION,
    )
    return stamp, {
        "modelUsed": stamp["modelUsed"],
        "promptVersion": stamp["promptVersion"],
        "schemaVersion": stamp["schemaVersion"],
        # The mode ACTUALLY USED. If the server refused the flag and thinking stayed on, this says
        # `thinking` — a silent fallback recorded as `non_thinking` would attribute a Wave 5A
        # ablation result to a cause it did not have.
        "reasoningMode": stamp["reasoningMode"],
        "enrichedAt": now,
    }


def _to_payload(obj: dict, event: dict, *, as_of: str, audit: dict,
                supplied: list[str]) -> dict:
    """The POST body. `isNovel`, `severity` and `tickers[].relevance` cannot appear here — the
    fields are constructed one by one rather than copied, so a new model field cannot leak in."""
    facts = [f for f in (obj.get("keyFacts") or [])
             if isinstance(f, str) and f.strip() and fact_is_supported(f, supplied)]
    tickers = []
    for entry in obj.get("tickers") or []:
        tickers.append({
            "ticker": str(entry.get("ticker", "")).strip().upper(),
            # No coercion ANYWHERE: `unclear` and `neutral` reach the store exactly as the model
            # said them. Rounding `unclear` up to `neutral` to make a card look decisive is a
            # contract violation, not a UX choice.
            "potentialDirection": entry.get("potentialDirection"),
            "potentialStrength": float(entry.get("potentialStrength")),
            "expectedHorizon": entry.get("expectedHorizon"),
            "affectedDimensions": list(entry.get("affectedDimensions") or []),
            "transmission": entry.get("transmission"),
        })
    return {
        "whyItMatters": obj.get("whyItMatters"),
        "keyFacts": facts,
        "tickers": tickers,
        "audit": audit,
        # §9.12. The CUTOFF, not the wall clock. `audit.enrichedAt` is the wall clock; substituting
        # one for the other is exactly how future-informed hypotheses leak into a historical read.
        "enrichmentAsOf": as_of,
    }


def _stub_result(event: dict, *, as_of: str, model_used: str, meta: dict, now: str,
                 attempts: int, failures: tuple[str, ...]) -> EnrichmentResult:
    """Two failures, or the provider chain exhausted, ⇒ state and audit, never hypotheses.

    Invariant #1 is already satisfied without this module: Lane 2A's raw event carries title,
    summary, key facts, deterministic importance, novelty and ticker links, and the feed renders
    from it. A fabricated `whyItMatters` would be worse than an absent one.

    Deterministic: for a fixed `now`, the same input produces a byte-identical result.
    """
    stamp, audit = _stamp_and_audit(model_used, meta, now=now)
    return EnrichmentResult(
        event_id=event.get("id", ""),
        state=FAILED,
        as_of=as_of,
        model_used=model_used,
        reasoning_mode=audit["reasoningMode"],
        attempts=attempts,
        payload=None,
        identity=stamp,
        failures=failures,
    )


def enrich_event(event: dict, prior_events: list[dict], *, as_of: str, now: str,
                 call=None) -> EnrichmentResult:
    """One event in, one result out. Strict schema, ONE firmer retry, then the deterministic stub.

    `as_of` is §9.12's cutoff, computed once by the caller and used for BOTH the `prior_events` read
    and the posted `enrichmentAsOf`. `now` is the wall clock, and is a parameter so the stub path is
    reproducible.
    """
    call = call or llm.call_model_meta
    mode = mode_for(TASK)
    linked = {str(t.get("ticker", "")).strip().upper()
              for t in (event.get("tickers") or []) if t.get("ticker")}
    supplied = supplied_texts(event)

    attempts = 0
    failures: tuple[str, ...] = ()
    obj = None
    model_used, meta = "stub:offline", {}

    for firmer in (None, "retry"):
        attempts += 1
        messages = build_messages(event, prior_events,
                                  firmer=list(failures) if firmer else None)
        raw, model_used, meta = call(messages, json_mode=True, mode=mode)
        if str(model_used).startswith("stub:"):
            # `_stub_label` said offline/quota: no generation happened. Do not retry a provider
            # chain that has already been exhausted once.
            return _stub_result(event, as_of=as_of, model_used=model_used, meta=meta, now=now,
                                attempts=attempts, failures=("no provider answered",))
        obj = llm.safe_json(raw)
        failures = tuple(validate(obj, linked=linked, supplied=supplied))
        if not failures:
            break
        if not needs_retry(list(failures)):
            break   # semantic refusal — never retried
    if failures:
        return _stub_result(event, as_of=as_of, model_used=model_used, meta=meta, now=now,
                            attempts=attempts, failures=failures)

    stamp, audit = _stamp_and_audit(model_used, meta, now=now)

    # The novelty JUDGEMENT (locked decision 3): local to this worker's skip decision. It never
    # writes the `novelty` or `relevance` column, and §9.16 keeps it out of the POST body.
    if obj.get("isNovel") is False:
        return EnrichmentResult(
            event_id=event.get("id", ""), state=SKIPPED, as_of=as_of, model_used=model_used,
            reasoning_mode=audit["reasoningMode"], attempts=attempts, payload=None,
            identity=stamp, skip_reason="the model judged this adds nothing beyond the prior events",
        )

    return EnrichmentResult(
        event_id=event.get("id", ""),
        state=ENRICHED,
        as_of=as_of,
        model_used=model_used,
        reasoning_mode=audit["reasoningMode"],
        attempts=attempts,
        payload=_to_payload(obj, event, as_of=as_of, audit=audit, supplied=supplied),
        identity=stamp,
    )
