"""Wave 3 Lane 3A — the typed tool surface (`app/tools.py`, contract §6).

Two tests in here are load-bearing and the rest support them.

`test_no_schema_declares_a_cutoff_argument` walks the REGISTRY — not a hand-written list — and fails
if any published schema mentions `as_of`/`asOf`. It matters because every other leakage test in this
programme patches the outbound HTTP client, and a model that supplies its own cutoff makes a
perfectly well-formed call that no such test can see. The registry walk is the only thing standing
between the model and a self-chosen point in time.

`test_no_vendor_name_appears_in_the_serialised_registry` fails if a provider name reaches the model
in any name, argument, enum or description. A tool named for a vendor teaches the model that vendors
are selectable, and its choice would be unauditable.

Deterministic and offline: no test here opens a socket, and the two that must PROVE no socket was
opened do it by asserting on a recorder, never by grepping a log (§9.44).

Run:  cd services/llm && python -m pytest -q tests/test_tools.py
"""
from __future__ import annotations

import json

import pytest

from app import tools

AS_OF = "2026-08-15T14:32:00Z"
HISTORICAL = "2024-03-01T00:00:00Z"


# ───────────────────────────────────────────────────────────────────────────── fakes

class FakeResponse:
    def __init__(self, payload, status_code=200, text=""):
        self._payload, self.status_code = payload, status_code
        try:
            self.text = text or json.dumps(payload)
        except TypeError:
            self.text = "<not json>"
        self.headers = {"content-type": "application/json"}

    def json(self):
        if self._payload is _UNPARSEABLE:
            raise ValueError("not json")
        return self._payload


_UNPARSEABLE = object()


class Recorder:
    """Stands in for `requests.get`. Records every call so a test can assert on what happened
    rather than on what was logged."""

    def __init__(self, payload=None, status_code=200, raises=None):
        self.calls: list[tuple[str, dict]] = []
        self.payload = {"asOf": AS_OF} if payload is None else payload
        self.status_code, self.raises = status_code, raises

    def __call__(self, url, params=None, timeout=None):
        self.calls.append((url, dict(params or {})))
        assert timeout == tools.UPSTREAM_TIMEOUT, "every upstream read must be bounded"
        if self.raises is not None:
            raise self.raises
        return FakeResponse(self.payload, self.status_code)


@pytest.fixture
def rec(monkeypatch):
    r = Recorder()
    monkeypatch.setattr(tools.requests, "get", r)
    return r


def ctx(**over) -> dict:
    base = {
        "asOf": AS_OF,
        "ticker": "NVDA",
        "horizon": "5d",
        "subscriptions": [{"id": "sub_9f2c1a4b7e0d3856", "ticker": "NVDA"}],
        "tickerContext": {"technical": {"trend": "uptrend"}},
        "marketContext": {"index": None, "rates": None},
        "analysisUrl": "http://analysis:8001",
        "eventsUrl": "http://events:8004",
    }
    base.update(over)
    return base


# ──────────────────────────────────────────────────────────── §6: exactly eight tools

def test_exactly_the_eight_tools_of_section_six():
    assert tools.tool_names() == [
        "get_user_subscriptions", "get_ticker_context", "get_multi_timeframe_context",
        "get_candles", "get_ticker_events", "get_event_details", "get_macro_context",
        "get_market_context",
    ]
    assert tools.TOOL_SCHEMA_VERSION == "tools@1"


def test_every_definition_is_openai_compatible_and_closed():
    for t in tools.TOOLS:
        assert t["type"] == "function"
        fn = t["function"]
        assert set(fn) == {"name", "description", "parameters"}
        p = fn["parameters"]
        assert p["type"] == "object"
        assert p["additionalProperties"] is False, f"{fn['name']} accepts unknown arguments"
        assert isinstance(p["required"], list), f"{fn['name']} has no explicit required list"
        for name in p["required"]:
            assert name in p["properties"], f"{fn['name']} requires undeclared {name!r}"


@pytest.mark.parametrize("specialist,cap", sorted(tools.SPECIALIST_CAPS.items()))
def test_specialist_registry_slices_are_within_the_section_six_caps(specialist, cap):
    slice_ = tools.registry_for(specialist)
    assert 0 < len(slice_) <= cap
    for t in slice_:
        assert t in tools.TOOLS


def test_an_unknown_specialist_raises_rather_than_returning_an_empty_toolset():
    with pytest.raises(ValueError):
        tools.registry_for("technichal")


# ────────────────────────────────────── the two rules that cannot be softened

def test_no_schema_declares_a_cutoff_argument():
    """Walks the registry. A tool added later is covered without anyone extending this test."""
    assert tools.schemas_declaring_as_of() == []
    blob = tools.serialised_registry().lower()
    assert "as_of" not in blob
    assert "asof" not in blob


@pytest.mark.parametrize("spelling", ["as_of", "asOf"])
def test_supplying_a_cutoff_argument_is_a_violation_not_a_silent_overwrite(spelling, rec):
    args = {"ticker": "NVDA", "timeframe": "1D", "limit": 60, spelling: "2020-01-01T00:00:00Z"}
    violations = tools.validate_args("get_candles", args)
    assert any(spelling in v for v in violations), violations

    out = tools.execute("get_candles", args, ctx())
    assert out["ok"] is False
    assert out["error"]["code"] == "invalid_arguments"
    assert rec.calls == [], "a rejected call must never reach an upstream"


def test_no_vendor_name_appears_in_the_serialised_registry():
    assert tools.vendor_leaks() == []


def test_no_tool_takes_a_provider_argument():
    """Provenance INSIDE a payload is §3.12 and stays; selecting a provider never becomes an
    argument, or the model's routing choice would be unauditable."""
    for t in tools.TOOLS:
        props = t["function"]["parameters"]["properties"]
        assert "provider" not in props and "source" not in props and "sourceTier" not in props


def test_no_execution_or_verdict_surface_anywhere_in_the_registry():
    """CLAUDE.md #2: no buy/sell/hold, no price target, no order affordance."""
    blob = tools.serialised_registry().lower()
    for banned in ("buy", "sell", "hold", "price target", "order", "broker", "execute a trade",
                   "recommend"):
        assert banned not in blob, f"{banned!r} reached the tool surface"


# ─────────────────────────────────────────────────────────────────── validation modes

@pytest.mark.parametrize("tool,args,needle", [
    ("get_bars", {}, "unknown tool"),                                        # unknown tool
    ("get_candles", {"ticker": "NVDA", "timeframe": "1D", "limit": 60, "x": 1}, "unknown argument"),
    ("get_candles", {"ticker": "NVDA", "timeframe": "1D"}, "missing required argument"),
    ("get_candles", {"ticker": "NVDA", "timeframe": "1D", "limit": "60"}, "must be an integer"),
    ("get_candles", {"ticker": "nvda", "timeframe": "1D", "limit": 60}, "uppercase"),
    ("get_candles", {"ticker": "NVDA", "timeframe": "4H", "limit": 60}, "must be one of"),
    ("get_candles", {"ticker": "NVDA", "timeframe": "1D", "limit": 0}, "between 1 and 500"),
    ("get_candles", {"ticker": "NVDA", "timeframe": "1D", "limit": 501}, "between 1 and 500"),
    ("get_candles", {"ticker": "NVDA", "timeframe": "1D", "limit": True}, "must be an integer"),
    ("get_event_details", {"eventId": "evt_XYZ"}, "16 lowercase hex"),
    ("get_ticker_events", {"ticker": "NVDA", "since": "2026-08-01"}, "ISO 8601 UTC"),
    ("get_ticker_events", {"ticker": "NVDA", "since": "2026-02-31T00:00:00Z"}, "real UTC instant"),
    ("get_ticker_context", {"ticker": "NVDA", "horizon": "3d"}, "must be one of"),
    ("get_user_subscriptions", {"ticker": "NVDA"}, "unknown argument"),
])
def test_every_failure_mode_names_the_field_and_the_allowed_values(tool, args, needle):
    violations = tools.validate_args(tool, args)
    assert violations, f"{tool}{args} should not have validated"
    assert any(needle in v for v in violations), violations


def test_valid_arguments_produce_no_violations():
    assert tools.validate_args("get_candles",
                               {"ticker": "NVDA", "timeframe": "1D", "limit": 60}) == []
    assert tools.validate_args("get_multi_timeframe_context", {"ticker": "NVDA"}) == []
    assert tools.validate_args("get_user_subscriptions", {}) == []


def test_arguments_must_be_an_object():
    assert tools.validate_args("get_candles", ["NVDA"])


# ───────────────────────────────────────────────────── exactly one retry, then loud

def test_one_retry_then_a_terminal_error(rec):
    c = ctx()
    bad = {"ticker": "nvda", "timeframe": "1D", "limit": 60}

    first = tools.execute("get_candles", bad, c)
    assert first["ok"] is False and first["attempts"] == 1
    assert first["error"]["code"] == "invalid_arguments"
    assert first["error"]["retryable"] is True
    assert "uppercase" in first["error"]["message"]

    second = tools.execute("get_candles", bad, c)
    assert second["ok"] is False and second["attempts"] == 2
    assert second["error"]["code"] == "invalid_arguments_terminal"
    assert second["error"]["retryable"] is False

    assert rec.calls == [], "a malformed call is never executed, not even once"
    # Never coerced, never defaulted, never dropped.
    assert second["result"] is None


def test_a_corrected_second_call_succeeds(rec):
    c = ctx()
    tools.execute("get_candles", {"ticker": "nvda", "timeframe": "1D", "limit": 60}, c)
    ok = tools.execute("get_candles", {"ticker": "NVDA", "timeframe": "1D", "limit": 60}, c)
    assert ok["ok"] is True and ok["attempts"] == 2
    assert len(rec.calls) == 1


def test_the_retry_budget_is_per_tool_and_per_turn(rec):
    c = ctx()
    tools.execute("get_candles", {"ticker": "nvda", "timeframe": "1D", "limit": 60}, c)
    other = tools.execute("get_multi_timeframe_context", {"ticker": "nvda"}, c)
    assert other["error"]["code"] == "invalid_arguments", "another tool starts with a fresh budget"
    fresh = tools.execute("get_candles", {"ticker": "nvda", "timeframe": "1D", "limit": 60}, ctx())
    assert fresh["error"]["code"] == "invalid_arguments", "a new turn starts with a fresh budget"


# ─────────────────────────────────────────────────────────────── the result envelope

ENVELOPE_KEYS = {"tool", "ok", "asOf", "result", "error", "degraded", "attempts"}


def test_every_outcome_uses_the_same_seven_key_envelope(monkeypatch):
    c = ctx()
    outs = [
        tools.execute("get_user_subscriptions", {}, c),                       # envelope, ok
        tools.execute("get_candles", {"ticker": "!"}, c),                     # invalid
    ]
    monkeypatch.setattr(tools.requests, "get", Recorder())
    outs.append(tools.execute("get_macro_context", {}, c))                    # http, ok
    monkeypatch.setattr(tools.requests, "get", Recorder(raises=OSError("no route to host")))
    outs.append(tools.execute("get_ticker_events",
                              {"ticker": "NVDA", "since": "2026-08-01T00:00:00Z"}, c))
    for o in outs:
        assert set(o) == ENVELOPE_KEYS, o
        assert isinstance(o["degraded"], list)
        assert isinstance(o["attempts"], int) and o["attempts"] >= 1


def test_the_trace_serialises_into_evidence_tool_calls(rec):
    c = ctx()
    tools.execute("get_user_subscriptions", {}, c)
    tools.execute("get_candles", {"ticker": "NVDA", "timeframe": "1D", "limit": 5}, c)
    tools.execute("get_candles", {"ticker": "bad"}, c)

    trace = tools.tool_call_trace(c)
    assert trace == [
        {"tool": "get_user_subscriptions", "args": {}},
        {"tool": "get_candles", "args": {"ticker": "NVDA", "timeframe": "1D", "limit": 5}},
        {"tool": "get_candles", "args": {"ticker": "bad"}},
    ]
    # §5 stores this verbatim, so it must survive a JSON round trip and must not smuggle a cutoff.
    assert "as_of" not in json.dumps(trace) and "asOf" not in json.dumps(trace)
    assert len(tools.tool_results(c)) == 3


# ──────────────────────────────────────────────────────────── injection and execution

def test_the_injected_cutoff_reaches_every_upstream_query(rec):
    c = ctx(asOf=HISTORICAL)
    tools.execute("get_candles", {"ticker": "NVDA", "timeframe": "1D", "limit": 5}, c)
    tools.execute("get_multi_timeframe_context", {"ticker": "NVDA", "horizon": "20d"}, c)
    tools.execute("get_ticker_events", {"ticker": "NVDA", "since": "2024-01-01T00:00:00Z"}, c)
    tools.execute("get_event_details", {"eventId": "evt_1a2b3c4d5e6f7081"}, c)
    tools.execute("get_macro_context", {}, c)

    assert len(rec.calls) == 5
    for url, params in rec.calls:
        assert params.get("as_of") == HISTORICAL, (url, params)
        assert url.startswith("http://analysis:8001") or url.startswith("http://events:8004"), url


def test_a_historical_turn_reaches_no_provider_and_no_socket_outside_the_two_services(rec):
    """The Python half of the leakage proof.

    `tools.py` can only ever reach `ANALYSIS_URL` and `EVENTS_URL` — both of which enforce §1.3
    themselves — and it always hands them the cutoff. The three envelope-served tools open no socket
    at all. Asserted positively on the recorder; nothing here reads a log (§9.44).
    """
    c = ctx(asOf=HISTORICAL)
    for name in tools.ENVELOPE_TOOLS:
        args = {"ticker": "NVDA", "horizon": "5d"} if name == "get_ticker_context" else {}
        out = tools.execute(name, args, c)
        assert out["ok"] is True
    assert rec.calls == [], "an envelope-served tool must never open a socket"


def test_upstream_urls_and_page_caps(rec):
    c = ctx()
    tools.execute("get_ticker_events", {"ticker": "NVDA", "since": "2026-08-01T00:00:00Z"}, c)
    url, params = rec.calls[-1]
    assert url == "http://events:8004/events"
    assert params["tickers"] == "NVDA"          # §3.11's parameter is plural
    assert params["limit"] == tools.EVENTS_PAGE_LIMIT

    tools.execute("get_event_details", {"eventId": "evt_1a2b3c4d5e6f7081"}, c)
    assert rec.calls[-1][0] == "http://events:8004/events/evt_1a2b3c4d5e6f7081"

    tools.execute("get_candles", {"ticker": "NVDA", "timeframe": "1H", "limit": 5}, c)
    url, params = rec.calls[-1]
    assert url == "http://analysis:8001/candles/NVDA"
    assert params["timeframe"] == "1H" and params["limit"] == 5


def test_the_optional_horizon_is_omitted_when_not_supplied(rec):
    tools.execute("get_multi_timeframe_context", {"ticker": "NVDA"}, ctx())
    assert "horizon" not in rec.calls[-1][1]


def test_the_resolved_cutoff_is_echoed_from_the_upstream(monkeypatch):
    monkeypatch.setattr(tools.requests, "get",
                        Recorder(payload={"asOf": "2026-08-14T20:00:00Z", "bars": []}))
    out = tools.execute("get_candles", {"ticker": "NVDA", "timeframe": "1D", "limit": 5}, ctx())
    assert out["asOf"] == "2026-08-14T20:00:00Z"


# ─────────────────────────────────────────────────────────────── coverage passthrough

def test_insufficient_coverage_reaches_the_model_unchanged(monkeypatch):
    """§1.3 / §9.10: coverage is a RESULT, not an error. No retry, no fetch, no substitute."""
    payload = {"ticker": "NVDA", "timeframe": "1D", "asOf": HISTORICAL, "bars": [],
               "coverage": "insufficient", "coverageFrom": None, "coverageTo": None,
               "source": "store", "sourceIsSynthetic": False}
    r = Recorder(payload=payload)
    monkeypatch.setattr(tools.requests, "get", r)

    out = tools.execute("get_candles", {"ticker": "NVDA", "timeframe": "1D", "limit": 60},
                        ctx(asOf=HISTORICAL))
    assert out["ok"] is True
    assert out["result"] == payload
    assert out["attempts"] == 1
    assert len(r.calls) == 1, "insufficient coverage must not trigger a second fetch"


def test_provenance_fields_survive_untouched(monkeypatch):
    payload = {"asOf": AS_OF, "events": [
        {"id": "evt_1a2b3c4d5e6f7081", "sources": [
            {"documentId": "doc_0", "provider": "an-upstream", "sourceTier": "official"}]}],
        "coverage": "ok"}
    monkeypatch.setattr(tools.requests, "get", Recorder(payload=payload))
    out = tools.execute("get_ticker_events",
                        {"ticker": "NVDA", "since": "2026-08-01T00:00:00Z"}, ctx())
    assert out["result"]["events"][0]["sources"][0]["sourceTier"] == "official"


# ────────────────────────────────────────────────────────────────────── offline paths

@pytest.mark.parametrize("boom,token", [
    (OSError("no route to host"), "events:offline"),
    (tools.requests.ConnectionError("refused"), "events:offline"),
    (tools.requests.Timeout("slow"), "events:timeout"),
])
def test_an_unreachable_upstream_degrades_and_never_raises(monkeypatch, boom, token):
    monkeypatch.setattr(tools.requests, "get", Recorder(raises=boom))
    out = tools.execute("get_macro_context", {}, ctx())
    assert out["ok"] is False
    assert out["degraded"] == [token]
    assert out["error"]["code"] == "upstream_unreachable"
    assert out["result"] is None


@pytest.mark.parametrize("status,token", [(429, "analysis:quota"), (503, "analysis:http_503")])
def test_a_refusing_upstream_degrades_with_a_subject_reason_token(monkeypatch, status, token):
    monkeypatch.setattr(tools.requests, "get", Recorder(payload={}, status_code=status))
    out = tools.execute("get_candles", {"ticker": "NVDA", "timeframe": "1D", "limit": 5}, ctx())
    assert out["ok"] is False and out["degraded"] == [token]


def test_a_non_json_upstream_degrades_rather_than_raising(monkeypatch):
    monkeypatch.setattr(tools.requests, "get", Recorder(payload=_UNPARSEABLE))
    out = tools.execute("get_macro_context", {}, ctx())
    assert out["ok"] is False and out["degraded"] == ["events:bad_payload"]


def test_every_degraded_token_is_a_subject_reason_token_not_a_sentence(monkeypatch):
    monkeypatch.setattr(tools.requests, "get", Recorder(raises=OSError("down")))
    c = ctx(subscriptions=None)
    outs = [tools.execute("get_macro_context", {}, c),
            tools.execute("get_user_subscriptions", {}, c),
            tools.execute("get_candles", {"ticker": "x"}, c)]
    for o in outs:
        for tok in o["degraded"]:
            assert " " not in tok and ":" in tok, tok


def test_a_missing_envelope_section_degrades_rather_than_inventing_one():
    out = tools.execute("get_market_context", {}, ctx(marketContext=None))
    assert out["ok"] is False
    assert out["degraded"] == ["context:marketContext_absent"]
    assert out["result"] is None


def test_the_envelope_refuses_a_company_it_does_not_cover(rec):
    out = tools.execute("get_ticker_context", {"ticker": "AMD", "horizon": "5d"}, ctx())
    assert out["ok"] is False
    assert out["degraded"] == ["context:ticker_out_of_envelope"]
    assert rec.calls == []


def test_ticker_context_states_which_horizon_the_envelope_was_built_for():
    out = tools.execute("get_ticker_context", {"ticker": "NVDA", "horizon": "60d"}, ctx())
    assert out["ok"] is True
    assert out["result"]["requestedHorizon"] == "60d"
    assert out["result"]["envelopeHorizon"] == "5d"
