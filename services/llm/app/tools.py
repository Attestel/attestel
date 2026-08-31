"""Typed tool surface for the analyst pipeline — contract `EVENT_CONTRACTS.md` §6 (Wave 3, Lane 3A).

This module is the ONLY place the eight domain tools are declared, and the only place a tool call is
turned into an upstream read. It ships no prompts, no model calls and no pipeline; Lane 3B calls it.

Three rules shape every line here, and none of them is a matter of taste.

1.  **`as_of` is injected by the runtime and is NEVER a model-supplied argument** (§1, §6). It does
    not appear in a tool name, in a `parameters.properties` block, in an enum, or in a description.
    The executor takes it from `ctx["asOf"]` and appends it to every upstream query itself. This is
    the one rule that cannot be softened: every leakage test in this programme patches the outbound
    HTTP client, so a *model-chosen* cutoff is invisible to all of them — it is a perfectly
    well-formed call that quietly reads the future. A model that supplies `as_of`/`asOf` therefore
    gets a validation VIOLATION, not a silent overwrite, because the attempt is itself a signal.

2.  **No vendor name reaches the model** (§6, doc §13.2). Provider routing lives behind the data
    layer. `provider` and `sourceTier` *inside a returned payload* are §3.12 provenance and are
    forwarded verbatim — but no tool takes a `provider` argument, so the model can never select one.

3.  **The llm service never talks to `journal` and never composes cross-service context.** Journal is
    session-cookie authenticated and this service holds no cookie. `get_user_subscriptions`,
    `get_ticker_context` and `get_market_context` are served from the CONTEXT ENVELOPE the gateway
    (`gateway/context.go`) posts in the request body. The other five are executed here over plain
    HTTP against `ANALYSIS_URL` and `EVENTS_URL` (§9.31), both keyless and both `as_of`-aware.

Public surface (what Lane 3B depends on):

    TOOL_SCHEMA_VERSION   -- "tools@1", the §5 `identity.toolSchemaVersion` literal
    TOOLS                 -- the full OpenAI-compatible definition list
    registry_for(spec)    -- per-specialist slice: technical | news | macro | orchestrator | final
    validate_args(t, a)   -- list of human-readable violations; empty means valid
    execute(t, a, ctx)    -- validate + execute, returns the result envelope
    tool_call_trace(ctx)  -- [{"tool", "args"}, ...], straight into §5 `evidence.toolCalls`
    tool_results(ctx)     -- the full envelopes, for a caller that wants the failures too

Result envelope, identical for every tool and every outcome:

    {"tool", "ok", "asOf", "result"|None, "error"|None, "degraded": [], "attempts"}

`degraded` reuses the house `subject:reason` token convention (`marketaux:budget` in §3.10,
`stub:offline` in `llm._stub_label`) — a short token, never a sentence.

Coverage is a RESULT, not an error (§1.3, §9.10). When the store cannot serve a cutoff the upstream
answers `coverage: "insufficient"` with `coverageFrom`/`coverageTo`; that reaches the model
unchanged. No retry, no fetch, no substitute.
"""
from __future__ import annotations

import json
import os
import re
from datetime import datetime
from typing import Any

import requests

# ─────────────────────────────────────────────────────────────────────────── constants

#: §5 `identity.toolSchemaVersion`. Bumping this is a contract event, not a refactor.
TOOL_SCHEMA_VERSION = "tools@1"

#: Upstream read timeout. `llm.py` uses 120 for MODEL calls; a data read that takes anywhere near
#: that has already failed the interaction, so it is bounded far tighter.
UPSTREAM_TIMEOUT = 20

#: Page size for the events list. Deliberately NOT a model argument: the news specialist's job is to
#: read what happened, not to tune pagination, and an unbounded page is how a tool result becomes a
#: blob. `services/events` caps `limit` server-side as well.
EVENTS_PAGE_LIMIT = 25

#: Page size for the macro list, same reasoning.
MACRO_PAGE_LIMIT = 25

TICKER_PATTERN = r"^[A-Z.\-]{1,10}$"
EVENT_ID_PATTERN = r"^evt_[0-9a-f]{16}$"
#: ISO 8601 UTC, ending `Z` (Conventions: Python services speak ISO strings ending Z).
UTC_TIMESTAMP_PATTERN = r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$"

TIMEFRAMES = ["1W", "1D", "1H", "15m", "5m"]      # Conventions
HORIZONS = ["1d", "5d", "20d", "60d"]             # §4.4 / Conventions short form
CANDLE_LIMIT_MIN, CANDLE_LIMIT_MAX, CANDLE_LIMIT_DEFAULT = 1, 500, 60

SPECIALISTS = ("technical", "news", "macro", "orchestrator", "final")

#: §6, verbatim. These are tighter than the doc's 5–10 ceiling and these are what is enforced.
SPECIALIST_CAPS = {
    "technical": 3,
    "news": 3,
    "macro": 3,
    "orchestrator": 3,
    "final": 4,
}

#: Any of these appearing anywhere in the serialised registry is a defect, not a style problem.
#: `sec` is listed as a bare token rather than `sec-edgar`, which is stricter than it looks: it also
#: forbids "sector", "seconds" and "security" in a description. That is a deliberate trade — the
#: check can never miss a vendor, at the cost of some wording.
BANNED_VENDOR_TOKENS = (
    "alpaca", "marketaux", "twelvedata", "tiingo", "yfinance",
    "fred", "fmp", "alphavantage", "sec", "edgar", "google", "stocktwits",
)

_TICKER_RE = re.compile(TICKER_PATTERN)
_EVENT_ID_RE = re.compile(EVENT_ID_PATTERN)
_TS_RE = re.compile(UTC_TIMESTAMP_PATTERN)

# ─────────────────────────────────────────────────────────── the eight tool definitions

_TICKER_PROP = {
    "type": "string",
    "pattern": TICKER_PATTERN,
    "description": "Company symbol, uppercase, e.g. NVDA.",
}

_NO_ARGS = {"type": "object", "properties": {}, "required": [], "additionalProperties": False}


def _fn(name: str, description: str, parameters: dict) -> dict:
    """OpenAI-compatible function definition. `parameters` is JSON Schema."""
    return {"type": "function", "function": {
        "name": name, "description": description, "parameters": parameters,
    }}


TOOLS: list[dict] = [
    _fn(
        "get_user_subscriptions",
        "List the companies this user follows. Takes no arguments; the user is already known.",
        dict(_NO_ARGS),
    ),
    _fn(
        "get_ticker_context",
        "Compact briefing for one company at the requested horizon: technical summary, recent "
        "events, earnings and the macro backdrop, already assembled for you.",
        {
            "type": "object",
            "properties": {
                "ticker": dict(_TICKER_PROP),
                "horizon": {
                    "type": "string",
                    "enum": list(HORIZONS),
                    "description": "Forecast horizon in trading days.",
                },
            },
            "required": ["ticker", "horizon"],
            "additionalProperties": False,
        },
    ),
    _fn(
        "get_multi_timeframe_context",
        "Aligned technical picture for one company across several timeframes at once, with a "
        "synthesis of where they agree and where they conflict.",
        {
            "type": "object",
            "properties": {
                "ticker": dict(_TICKER_PROP),
                "horizon": {
                    "type": "string",
                    "enum": list(HORIZONS),
                    "description": "Forecast horizon in trading days; omit for the swing default.",
                },
            },
            "required": ["ticker"],
            "additionalProperties": False,
        },
    ),
    _fn(
        "get_candles",
        "Raw price bars with per-bar geometry for one company and one timeframe. Use this only "
        "when you need to inspect individual bars; prefer the aligned technical picture first.",
        {
            "type": "object",
            "properties": {
                "ticker": dict(_TICKER_PROP),
                "timeframe": {
                    "type": "string",
                    "enum": list(TIMEFRAMES),
                    "description": "Bar interval.",
                },
                "limit": {
                    "type": "integer",
                    "minimum": CANDLE_LIMIT_MIN,
                    "maximum": CANDLE_LIMIT_MAX,
                    "default": CANDLE_LIMIT_DEFAULT,
                    "description": "How many bars to return, newest last.",
                },
            },
            "required": ["ticker", "timeframe", "limit"],
            "additionalProperties": False,
        },
    ),
    _fn(
        "get_ticker_events",
        "Recent corporate and market events touching one company, newest first, with importance "
        "and novelty already scored.",
        {
            "type": "object",
            "properties": {
                "ticker": dict(_TICKER_PROP),
                "since": {
                    "type": "string",
                    "pattern": UTC_TIMESTAMP_PATTERN,
                    "description": "Earliest event time to include, ISO 8601 UTC ending Z, "
                                   "e.g. 2026-08-01T00:00:00Z.",
                },
            },
            "required": ["ticker", "since"],
            "additionalProperties": False,
        },
    ),
    _fn(
        "get_event_details",
        "Full record for one event: summary, key facts, the companies it touches, and its "
        "underlying documents.",
        {
            "type": "object",
            "properties": {
                "eventId": {
                    "type": "string",
                    "pattern": EVENT_ID_PATTERN,
                    "description": "Event identifier as returned by get_ticker_events, "
                                   "e.g. evt_1a2b3c4d5e6f7081.",
                },
            },
            "required": ["eventId"],
            "additionalProperties": False,
        },
    ),
    _fn(
        "get_macro_context",
        "Recent macroeconomic releases with actual, consensus and prior readings. Takes no "
        "arguments.",
        dict(_NO_ARGS),
    ),
    _fn(
        "get_market_context",
        "The broad market backdrop assembled for you: index trend, rates and volatility. Takes no "
        "arguments. A dimension this deployment cannot compute is reported as null rather than "
        "guessed.",
        dict(_NO_ARGS),
    ),
]

#: §6, verbatim. `get_ticker_context` and `get_market_context` each appear for two specialists.
_REGISTRY: dict[str, tuple[str, ...]] = {
    "technical":    ("get_multi_timeframe_context", "get_candles"),
    "news":         ("get_ticker_events", "get_event_details"),
    "macro":        ("get_macro_context", "get_market_context"),
    "orchestrator": ("get_user_subscriptions", "get_ticker_context"),
    "final":        ("get_ticker_context", "get_market_context"),
}

#: Which tools are answered from the gateway's context envelope and never open a socket.
ENVELOPE_TOOLS = frozenset({"get_user_subscriptions", "get_ticker_context", "get_market_context"})

_BY_NAME = {t["function"]["name"]: t for t in TOOLS}


def tool_names() -> list[str]:
    """Every declared tool name, in declaration order."""
    return [t["function"]["name"] for t in TOOLS]


def registry_for(specialist: str) -> list[dict]:
    """The tool slice one specialist may see.

    An unknown specialist RAISES rather than returning an empty list: a typo that silently hands a
    specialist zero tools produces a plausible-looking answer with no evidence behind it, which is
    exactly the failure this whole surface exists to prevent.
    """
    if specialist not in _REGISTRY:
        raise ValueError(
            f"unknown specialist {specialist!r}; expected one of {', '.join(SPECIALISTS)}"
        )
    return [_BY_NAME[n] for n in _REGISTRY[specialist]]


# ─────────────────────────────────────────────────────────────────────────── validation

def validate_args(tool_name: str, args: Any) -> list[str]:
    """Hand-rolled JSON Schema validation against the schema THIS module published.

    Returns human-readable violations, empty when valid. Every message names the offending field and
    the allowed values, because the message is what the model gets one chance to act on.

    No JSON-Schema dependency: `services/llm/requirements.txt` stays at fastapi / uvicorn / requests
    / pydantic, and the schema subset in use here (type, enum, pattern, minimum, maximum, required,
    additionalProperties) is small enough to read in one screen.
    """
    if tool_name not in _BY_NAME:
        return [f"unknown tool {tool_name!r}; available tools are {', '.join(tool_names())}"]
    if not isinstance(args, dict):
        return [f"arguments must be a JSON object, got {type(args).__name__}"]

    schema = _BY_NAME[tool_name]["function"]["parameters"]
    props: dict = schema.get("properties", {})
    required: list = schema.get("required", [])
    out: list[str] = []

    # The cutoff check comes FIRST and is its own message. `as_of` would otherwise be caught by the
    # generic unknown-property rule, and "unknown property" is the wrong thing to say about an
    # attempt to choose a point in time: it reads like a typo instead of like leakage.
    for forbidden in ("as_of", "asOf"):
        if forbidden in args:
            out.append(
                f"{forbidden!r} is not a tool argument and must not be supplied: the point in time "
                f"for this whole analysis is fixed by the runtime and applies to every tool call."
            )

    for key in args:
        if key in ("as_of", "asOf"):
            continue
        if key not in props:
            allowed = ", ".join(props) or "none"
            out.append(f"unknown argument {key!r} for {tool_name}; allowed arguments: {allowed}")

    for key in required:
        if key not in args:
            out.append(f"missing required argument {key!r} for {tool_name}")

    for key, spec in props.items():
        if key not in args:
            continue
        out.extend(_violations_for(tool_name, key, spec, args[key]))

    return out


def _violations_for(tool_name: str, key: str, spec: dict, value: Any) -> list[str]:
    out: list[str] = []
    want = spec.get("type")

    if want == "string":
        if not isinstance(value, str):
            return [f"{key!r} must be a string, got {type(value).__name__}"]
        if "enum" in spec and value not in spec["enum"]:
            return [f"{key!r} must be one of {' | '.join(spec['enum'])}, got {value!r}"]
        pattern = spec.get("pattern")
        if pattern and not re.match(pattern, value):
            out.append(_pattern_message(key, pattern, value))
        elif pattern == UTC_TIMESTAMP_PATTERN:
            # Shape is right; make sure it is a real instant. "2026-02-31T00:00:00Z" matches the
            # pattern and is not a date.
            try:
                datetime.strptime(value.split(".")[0], "%Y-%m-%dT%H:%M:%SZ")
            except ValueError:
                out.append(f"{key!r} is not a real UTC instant: {value!r}")

    elif want == "integer":
        # bool is an int in Python and must not slip through as one.
        if isinstance(value, bool) or not isinstance(value, int):
            return [f"{key!r} must be an integer, got {type(value).__name__}"]
        lo, hi = spec.get("minimum"), spec.get("maximum")
        if lo is not None and value < lo:
            out.append(f"{key!r} must be between {lo} and {hi}, got {value}")
        elif hi is not None and value > hi:
            out.append(f"{key!r} must be between {lo} and {hi}, got {value}")

    return out


def _pattern_message(key: str, pattern: str, value: Any) -> str:
    if pattern == TICKER_PATTERN:
        return (f"{key!r} must be an uppercase company symbol of 1-10 characters "
                f"(letters, '.', '-'), got {value!r}")
    if pattern == EVENT_ID_PATTERN:
        return (f"{key!r} must look like evt_ followed by 16 lowercase hex characters, "
                f"got {value!r}")
    if pattern == UTC_TIMESTAMP_PATTERN:
        return (f"{key!r} must be an ISO 8601 UTC timestamp ending in Z, e.g. "
                f"2026-08-01T00:00:00Z, got {value!r}")
    return f"{key!r} does not match {pattern}, got {value!r}"


# ─────────────────────────────────────────────────────────── per-turn state (ctx-carried)
#
# `ctx` is the per-turn dict Lane 3B builds from the gateway envelope. Attempt counting and the call
# trace live on it rather than in a module global, because a module global would leak one turn's
# retry budget into the next and would be wrong the moment two turns run concurrently.

_ATTEMPTS_KEY = "_toolAttempts"
_TRACE_KEY = "_toolTrace"


def _attempts(ctx: dict) -> dict:
    return ctx.setdefault(_ATTEMPTS_KEY, {})


def tool_results(ctx: dict) -> list[dict]:
    """Every result envelope produced this turn, successes and failures alike, in order."""
    return list(ctx.get(_TRACE_KEY, []))


def tool_call_trace(ctx: dict) -> list[dict]:
    """The §5 `evidence.toolCalls` projection: `[{"tool": ..., "args": {...}}, ...]`.

    Lane 3C stores it; this lane produces it. Failed calls are INCLUDED — a call the model got wrong
    twice is part of how the answer was reached, and dropping it would make the record flattering
    rather than true. The injected cutoff is deliberately absent from every `args` block: it was
    never a model argument, and writing it into the trace would invent a call that never happened.
    """
    return [{"tool": e["tool"], "args": e.get("args", {})} for e in ctx.get(_TRACE_KEY, [])]


def _record(ctx: dict, envelope: dict, args: Any) -> dict:
    entry = dict(envelope)
    entry["args"] = args if isinstance(args, dict) else {}
    ctx.setdefault(_TRACE_KEY, []).append(entry)
    return envelope


# ────────────────────────────────────────────────────────────────────────────── envelope

def _result(tool: str, *, ok: bool, as_of: Any, result: Any = None,
            error: Any = None, degraded: list[str] | None = None, attempts: int = 1) -> dict:
    """The one result shape. Seven keys, always the same seven, whatever happened."""
    return {
        "tool": tool,
        "ok": ok,
        "asOf": as_of,
        "result": result,
        "error": error,
        "degraded": list(degraded or []),
        "attempts": attempts,
    }


def _error(code: str, message: str, *, retryable: bool,
           violations: list[str] | None = None) -> dict:
    err = {"code": code, "message": message, "retryable": retryable}
    if violations:
        err["violations"] = list(violations)
    return err


# ────────────────────────────────────────────────────────────────────────────── execution

def _base_url(ctx: dict, ctx_key: str, env_key: str, default: str) -> str:
    """Upstream base URL. `ctx` wins over the environment so a caller (and a test) can redirect one
    turn without mutating process state; the environment is the production path (§9.31)."""
    raw = ctx.get(ctx_key) or os.getenv(env_key) or default
    return str(raw).rstrip("/")


def analysis_url(ctx: dict | None = None) -> str:
    return _base_url(ctx or {}, "analysisUrl", "ANALYSIS_URL", "http://localhost:8001")


def events_url(ctx: dict | None = None) -> str:
    return _base_url(ctx or {}, "eventsUrl", "EVENTS_URL", "http://localhost:8004")


def _degraded_token(subject: str, exc: Exception | None, status: int | None = None) -> str:
    """`subject:reason`, following `llm._stub_label`. Never a sentence."""
    if status == 429:
        return f"{subject}:quota"
    if status is not None:
        return f"{subject}:http_{status}"
    if isinstance(exc, requests.Timeout):
        return f"{subject}:timeout"
    return f"{subject}:offline"


def _get(subject: str, url: str, params: dict, as_of: Any, tool: str, attempts: int) -> dict:
    """One upstream read, fail-soft.

    A provider key that is absent is NOT this function's problem and must never raise here:
    `services/analysis` and `services/events` are both keyless-capable (invariant #1), analysis
    serves synthetic bars labelled `sourceIsSynthetic` and events serves an empty set with
    `coverage`. An unreachable upstream comes back as `ok: false` plus a `degraded` token, which the
    model can reason about, rather than as an exception, which it cannot.
    """
    try:
        resp = requests.get(url, params=params, timeout=UPSTREAM_TIMEOUT)
    except Exception as exc:  # requests raises a family; none of them may escape
        return _result(tool, ok=False, as_of=as_of, attempts=attempts,
                       degraded=[_degraded_token(subject, exc)],
                       error=_error("upstream_unreachable",
                                    f"{subject} is unreachable right now.", retryable=False))
    if resp.status_code >= 400:
        detail = (resp.text or "")[:200]
        return _result(tool, ok=False, as_of=as_of, attempts=attempts,
                       degraded=[_degraded_token(subject, None, resp.status_code)],
                       error=_error(f"upstream_{resp.status_code}",
                                    f"{subject} refused the request: {detail}", retryable=False))
    try:
        payload = resp.json()
    except Exception:
        return _result(tool, ok=False, as_of=as_of, attempts=attempts,
                       degraded=[f"{subject}:bad_payload"],
                       error=_error("upstream_bad_payload",
                                    f"{subject} returned something that is not JSON.",
                                    retryable=False))
    if not isinstance(payload, dict):
        return _result(tool, ok=False, as_of=as_of, attempts=attempts,
                       degraded=[f"{subject}:bad_payload"],
                       error=_error("upstream_bad_payload",
                                    f"{subject} returned a {type(payload).__name__}, not an object.",
                                    retryable=False))
    # §1.4: echo the cutoff the UPSTREAM actually resolved, in preference to the one we sent. The
    # whole payload is forwarded, so `coverage`, `coverageFrom`, `coverageTo`, `sourceIsSynthetic`,
    # `source`, `provider` and `sourceTier` survive untouched (§1.3, §3.12). Nothing is trimmed.
    return _result(tool, ok=True, as_of=payload.get("asOf", as_of), result=payload,
                   attempts=attempts)


def _from_envelope(tool: str, ctx: dict, key: str, as_of: Any, attempts: int) -> dict:
    """Serve a tool from the gateway context envelope. Opens no socket, ever."""
    if key not in ctx or ctx[key] is None:
        return _result(tool, ok=False, as_of=as_of, attempts=attempts,
                       degraded=[f"context:{key}_absent"],
                       error=_error("context_unavailable",
                                    f"The {key} section of this request's context was not supplied.",
                                    retryable=False))
    return _result(tool, ok=True, as_of=as_of, result=ctx[key], attempts=attempts)


def execute(tool_name: str, args: Any, ctx: dict) -> dict:
    """Validate, execute, return the result envelope. The single entry point.

    `ctx` keys this reads: `asOf` (the injected cutoff), `ticker`, `horizon`, `subscriptions`,
    `tickerContext`, `marketContext`, and optionally `analysisUrl` / `eventsUrl`.

    Retry policy (§10, and locked decision 4): a malformed call is answered ONCE with a retryable
    error that names which fields are wrong and what the allowed values are. The second malformed
    call for the SAME tool in the same turn is terminal. Nothing is ever coerced, defaulted, or
    quietly dropped — a tool call that ran with an argument the model did not supply is a fabricated
    piece of evidence.
    """
    as_of = ctx.get("asOf")
    counts = _attempts(ctx)
    attempts = counts.get(tool_name, 0) + 1
    counts[tool_name] = attempts

    violations = validate_args(tool_name, args)
    if violations:
        first = attempts == 1
        return _record(ctx, _result(
            tool_name, ok=False, as_of=as_of, attempts=attempts,
            degraded=["args:invalid"],
            error=_error(
                "invalid_arguments" if first else "invalid_arguments_terminal",
                ("Fix these and call again: " if first
                 else "Second malformed call for this tool; not retrying: ") + "; ".join(violations),
                retryable=first, violations=violations),
        ), args)

    handler = _HANDLERS[tool_name]
    return _record(ctx, handler(args, ctx, as_of, attempts), args)


# Each handler receives validated arguments. `as_of` is threaded in from ctx and appended to every
# upstream query below — it is never read out of `args`, which cannot contain it.

def _h_user_subscriptions(_args, ctx, as_of, attempts):
    return _from_envelope("get_user_subscriptions", ctx, "subscriptions", as_of, attempts)


def _h_ticker_context(args, ctx, as_of, attempts):
    tool = "get_ticker_context"
    envelope_ticker = str(ctx.get("ticker") or "").upper()
    if envelope_ticker and args["ticker"] != envelope_ticker:
        # The envelope is built for one company. Answering for a different one would mean handing
        # back NVDA's briefing under AMD's name.
        return _result(tool, ok=False, as_of=as_of, attempts=attempts,
                       degraded=["context:ticker_out_of_envelope"],
                       error=_error("context_unavailable",
                                    f"This request's context covers {envelope_ticker} only; "
                                    f"{args['ticker']} is not in it.", retryable=False))
    out = _from_envelope(tool, ctx, "tickerContext", as_of, attempts)
    if out["ok"]:
        # Say honestly which horizon the envelope was built for, so a mismatch is visible to the
        # model rather than silently implied to match.
        out["result"] = {
            "requestedHorizon": args["horizon"],
            "envelopeHorizon": ctx.get("horizon"),
            "context": out["result"],
        }
    return out


def _h_market_context(_args, ctx, as_of, attempts):
    return _from_envelope("get_market_context", ctx, "marketContext", as_of, attempts)


def _h_multi_timeframe_context(args, ctx, as_of, attempts):
    params: dict[str, Any] = {}
    if "horizon" in args:
        params["horizon"] = args["horizon"]
    if as_of:
        params["as_of"] = as_of
    return _get("analysis", f"{analysis_url(ctx)}/multi-timeframe-context/{args['ticker']}",
                params, as_of, "get_multi_timeframe_context", attempts)


def _h_candles(args, ctx, as_of, attempts):
    params: dict[str, Any] = {"timeframe": args["timeframe"], "limit": args["limit"]}
    if as_of:
        params["as_of"] = as_of
    return _get("analysis", f"{analysis_url(ctx)}/candles/{args['ticker']}",
                params, as_of, "get_candles", attempts)


def _h_ticker_events(args, ctx, as_of, attempts):
    # §3.11's parameter is `tickers` (a comma-separated set). The tool takes exactly one company,
    # because a specialist reasoning about two at once is how attribution gets lost.
    params: dict[str, Any] = {
        "tickers": args["ticker"], "since": args["since"], "limit": EVENTS_PAGE_LIMIT,
    }
    if as_of:
        params["as_of"] = as_of
    return _get("events", f"{events_url(ctx)}/events", params, as_of, "get_ticker_events", attempts)


def _h_event_details(args, ctx, as_of, attempts):
    params: dict[str, Any] = {}
    if as_of:
        params["as_of"] = as_of
    return _get("events", f"{events_url(ctx)}/events/{args['eventId']}",
                params, as_of, "get_event_details", attempts)


def _h_macro_context(_args, ctx, as_of, attempts):
    params: dict[str, Any] = {"limit": MACRO_PAGE_LIMIT}
    if as_of:
        params["as_of"] = as_of
    return _get("events", f"{events_url(ctx)}/macro", params, as_of, "get_macro_context", attempts)


_HANDLERS = {
    "get_user_subscriptions": _h_user_subscriptions,
    "get_ticker_context": _h_ticker_context,
    "get_multi_timeframe_context": _h_multi_timeframe_context,
    "get_candles": _h_candles,
    "get_ticker_events": _h_ticker_events,
    "get_event_details": _h_event_details,
    "get_macro_context": _h_macro_context,
    "get_market_context": _h_market_context,
}


# ───────────────────────────────────────────────────────────────────── self-check helpers
#
# These are exported so the gate can be run from anywhere, not only from the test file: a lane that
# imports this module can assert the two invariants for itself in one line each.

def serialised_registry() -> str:
    """The registry exactly as it would be sent to the model."""
    return json.dumps(TOOLS, sort_keys=True)


def vendor_leaks() -> list[str]:
    """Banned vendor tokens present in the serialised registry. Empty is the only passing answer."""
    blob = serialised_registry().lower()
    return [tok for tok in BANNED_VENDOR_TOKENS if tok in blob]


def schemas_declaring_as_of() -> list[str]:
    """Tools whose PUBLISHED schema mentions a cutoff argument. Empty is the only passing answer.

    This walks the registry rather than checking a hand-written list, so a tool added later is
    covered without anyone remembering to extend the test.
    """
    bad = []
    for t in TOOLS:
        blob = json.dumps(t["function"]).lower()
        if "as_of" in blob or "asof" in blob:
            bad.append(t["function"]["name"])
    return bad
