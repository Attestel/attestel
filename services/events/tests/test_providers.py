"""Provider no-key skips, timestamp normalisation, tiers, and the 2A handshake.

No test here opens a socket. `app.providers.requests` is replaced by a stub whose whole job is to
hand back canned payloads and record what was asked for, so "did we call out?" is a fact the tests
observe rather than a claim they make.
"""
from __future__ import annotations

import json
from datetime import datetime, timezone

import pytest

from app import providers
from app.db import connect, migrate
from app.entities import PROVIDER_TIER, guess_event_type, primary_ticker, resolve_tickers
from app.providers import (
    ALL_PROVIDERS,
    normalise_timestamp,
    fetch_alphavantage_earnings,
    fetch_federal_reserve_calendar,
    fetch_fmp_calendar,
    fetch_fred_releases,
    fetch_marketaux,
    fetch_rss_news,
    fetch_sec_filings,
)

KEY_ENV = (
    "MARKETAUX_API_KEY", "ALPHAVANTAGE_API_KEY", "FRED_API_KEY", "FMP_API_KEY",
    "EVENTS_CONTACT_UA", "FEDERAL_RESERVE_ENABLED",
)


@pytest.fixture()
def conn(tmp_path, monkeypatch):
    for name in KEY_ENV:
        monkeypatch.delenv(name, raising=False)
    c = connect()
    migrate(c)
    yield c
    c.close()


class FakeResponse:
    def __init__(self, text="", status_code=200):
        self.text = text
        self.status_code = status_code


class FakeRequests:
    """Records every outbound call and answers from a queue keyed by a URL fragment."""

    def __init__(self, answers=None):
        self.answers = answers or {}
        self.calls = []

    def get(self, url, params=None, headers=None, timeout=None):
        self.calls.append({"url": url, "params": params, "headers": headers, "timeout": timeout})
        assert timeout is not None, "every request must carry a timeout"
        for fragment, response in self.answers.items():
            if fragment in url and (
                not isinstance(response, dict)
                or all((params or {}).get(k) == v for k, v in response.get("when", {}).items())
            ):
                body = response["body"] if isinstance(response, dict) else response
                status = response.get("status", 200) if isinstance(response, dict) else 200
                return FakeResponse(body, status)
        raise AssertionError(f"unexpected outbound call: {url}")


@pytest.fixture()
def fake(monkeypatch):
    stub = FakeRequests()
    monkeypatch.setattr(providers, "requests", stub)
    return stub


# =================================================================================================
# The empty-.env path (CLAUDE.md invariant #1)
# =================================================================================================


def test_every_provider_skips_without_its_key_and_never_attempts(conn, fake):
    """Skip, with a degraded entry. Never attempt, never raise — this is what makes an empty
    `.env` work, and `fake` raises on any outbound call so 'never attempt' is observed."""
    results = [
        fetch_marketaux(conn, ticker="NVDA"),
        fetch_sec_filings(conn, ticker="NVDA"),
        fetch_rss_news(conn, ticker="NVDA"),
        fetch_alphavantage_earnings(conn, ticker="NVDA"),
        fetch_federal_reserve_calendar(
            conn, from_date="2026-08-01", to_date="2026-09-01"
        ),
        fetch_fred_releases(conn, from_date="2026-08-01", to_date="2026-09-01"),
        fetch_fmp_calendar(conn, from_date="2026-08-01", to_date="2026-09-01"),
    ]
    assert fake.calls == []
    assert all(r.documents == [] and r.scheduled == [] for r in results)
    assert [r.degraded for r in results] == [
        ["marketaux:no-key"], ["sec-edgar:no-key"], ["google-news-rss:no-key"],
        ["alphavantage:no-key"], ["federal-reserve:disabled"], ["fred:no-key"],
        ["fmp:no-key"],
    ]
    # a skip does not spend a budget reservation either
    assert conn.execute("SELECT COALESCE(SUM(calls), 0) AS n FROM provider_budget") \
        .fetchone()["n"] == 0


# =================================================================================================
# Timestamps (§1.1)
# =================================================================================================


@pytest.mark.parametrize("raw,expected", [
    ("2026-08-15T13:21:00Z", "2026-08-15T13:21:00Z"),
    ("2026-08-15T13:21:00.123456Z", "2026-08-15T13:21:00Z"),
    ("2026-08-15T09:21:00-04:00", "2026-08-15T13:21:00Z"),
    ("2026-08-15", "2026-08-15T00:00:00Z"),
    ("2026-08-15 08:30:00", "2026-08-15T08:30:00Z"),
    ("Sat, 15 Aug 2026 13:21:00 GMT", "2026-08-15T13:21:00Z"),
])
def test_timestamps_are_normalised_to_fixed_width_utc(raw, expected):
    assert normalise_timestamp(raw) == expected


@pytest.mark.parametrize("raw", ["", None, "yesterday", "not a date", "15/08/2026"])
def test_an_unparseable_timestamp_is_none(raw):
    assert normalise_timestamp(raw) is None


def test_a_document_with_an_unparseable_time_is_dropped_not_backdated(conn, monkeypatch, fake):
    """A wrong `published_at` is a leak in disguise: it would make the document visible at a cutoff
    it was not visible at. Dropping it is the only safe answer."""
    monkeypatch.setenv("MARKETAUX_API_KEY", "k")
    fake.answers["marketaux"] = json.dumps({"data": [
        {"title": "NVIDIA good", "url": "https://x.test/1",
         "published_at": "2026-08-15T13:21:00Z",
         "entities": [{"symbol": "NVDA"}]},
        {"title": "bad", "url": "https://x.test/2", "published_at": "soon", "entities": []},
    ]})
    result = fetch_marketaux(conn, ticker="NVDA")
    assert [d["title"] for d in result.documents] == ["NVIDIA good"]
    assert result.degraded == ["marketaux:unparseable-time"]


# =================================================================================================
# Tiers
# =================================================================================================


def test_tier_assignment_is_a_constant_map_not_a_judgement(conn, monkeypatch, fake):
    """The map lives in `entities.PROVIDER_TIER` — Wave 2 Lane 2A's, the ONLY declaration. This
    lane imports `tier_for_provider` rather than restating it, so the two cannot drift."""
    assert PROVIDER_TIER["sec-edgar"] == "official"
    assert PROVIDER_TIER["fred"] == "official"
    assert PROVIDER_TIER["federal-reserve"] == "official"
    assert PROVIDER_TIER["company-ir"] == "official"
    for professional in ("marketaux", "google-news-rss", "alphavantage", "fmp"):
        assert PROVIDER_TIER[professional] == "professional"
    # no provider in this lane emits `discussion`
    assert set(ALL_PROVIDERS) & {p for p, t in PROVIDER_TIER.items() if t == "discussion"} == set()

    import inspect
    source = inspect.getsource(providers)
    assert "PROVIDER_TIER" not in source.split('"""', 2)[2], \
        "providers.py must import the tier map, never declare a second one"


# =================================================================================================
# Marketaux
# =================================================================================================


def test_marketaux_normalises_a_response(conn, monkeypatch, fake):
    monkeypatch.setenv("MARKETAUX_API_KEY", "k")
    fake.answers["marketaux"] = json.dumps({"data": [{
        "title": "NVIDIA  beats\nestimates",
        "description": "  Data-centre revenue set a record.  ",
        "url": "https://news.test/a",
        "published_at": "2026-08-15T13:21:00Z",
        "entities": [{"symbol": "NVDA"}, {"symbol": "AMD"}],
    }]})
    result = fetch_marketaux(conn, ticker="NVDA")
    document = result.documents[0]
    assert document["provider"] == "marketaux"
    assert document["source_tier"] == "professional"
    assert document["title"] == "NVIDIA beats estimates"
    assert document["excerpt"] == "Data-centre revenue set a record."
    assert document["published_at"] == "2026-08-15T13:21:00Z"
    # raw_tickers is EXACTLY what the provider reported, in its order
    assert document["raw_tickers"] == ["NVDA", "AMD"]
    assert document["body"] is None
    assert fake.calls[0]["params"]["api_token"] == "k"


def test_marketaux_drops_articles_without_a_resolvable_headline_subject(monkeypatch):
    monkeypatch.setenv("MARKETAUX_API_KEY", "k")
    payload = json.dumps({"data": [
        {
            "title": "What's Going on with SpaceX Stock?",
            "description": "The article body briefly compares several chipmakers.",
            "url": "https://news.test/noise",
            "published_at": "2026-08-15T13:21:00Z",
            "entities": [{"symbol": "NVDA"}],
        },
        {
            "title": "NVIDIA launches a new accelerator",
            "description": "A company-specific article.",
            "url": "https://news.test/nvda",
            "published_at": "2026-08-15T13:22:00Z",
            "entities": [{"symbol": "NVDA"}, {"symbol": "AMD"}],
        },
    ]})
    monkeypatch.setattr(
        providers, "_get", lambda *_args, **_kwargs: providers.Response(ok=True, text=payload),
    )

    result = fetch_marketaux(None, ticker="NVDA")

    assert [row["title"] for row in result.documents] == [
        "NVIDIA launches a new accelerator",
    ]
    # Accepted documents retain the provider's entity list exactly for provenance; only the
    # canonical event-company mapping filters body-only entities.
    assert result.documents[0]["raw_tickers"] == ["NVDA", "AMD"]


def test_marketaux_error_payload_is_a_degraded_entry(conn, monkeypatch, fake):
    monkeypatch.setenv("MARKETAUX_API_KEY", "k")
    fake.answers["marketaux"] = json.dumps({"error": {"message": "quota"}})
    result = fetch_marketaux(conn, ticker="NVDA")
    assert result.documents == []
    assert result.degraded == ["marketaux:error"]


# =================================================================================================
# SEC EDGAR
# =================================================================================================


SEC_PAYLOAD = json.dumps({"filings": {"recent": {
    "form": ["8-K", "10-Q", "8-K"],
    "accessionNumber": ["0001045810-26-000123", "x", "0001045810-26-000124"],
    "primaryDocument": ["nvda-8k.htm", "y", "nvda-8k2.htm"],
    "items": ["2.02,9.01", "", "5.02"],
    "filingDate": ["2026-08-14", "2026-08-01", "2026-08-10"],
    "acceptanceDateTime": ["2026-08-14T16:05:12.000Z", "", ""],
}}})


def test_sec_filings_render_the_gateway_headline_and_feed_2a_correctly(conn, monkeypatch, fake):
    """The headline shape is load-bearing: `entities.guess_event_type` matches on the 8-K item
    LABELS, so `8-K filed 2026-08-14 — Results of Operations` must render byte-for-byte as
    `gateway/providers.go` renders it or every earnings 8-K silently types as `other`."""
    monkeypatch.setenv("EVENTS_CONTACT_UA", "attestel ops@example.test")
    fake.answers["data.sec.gov"] = SEC_PAYLOAD
    result = fetch_sec_filings(conn, ticker="NVDA")

    assert len(result.documents) == 2  # the 10-Q is not an 8-K
    first = result.documents[0]
    assert first["title"] == "8-K filed 2026-08-14 — Results of Operations, Financial Statements & Exhibits"
    assert first["source_tier"] == "official"
    assert first["published_at"] == "2026-08-14T16:05:12Z"  # acceptanceDateTime beats filingDate
    assert first["url"] == \
        "https://www.sec.gov/Archives/edgar/data/1045810/000104581026000123/nvda-8k.htm"
    assert fake.calls[0]["headers"]["User-Agent"] == "attestel ops@example.test"

    # the filer must be raw_tickers[0]: entities.resolve_tickers reads that position and nothing
    # downstream can recover the ordering afterwards
    assert first["raw_tickers"] == ["NVDA"]
    row = {"provider": "sec-edgar", "title": first["title"], "raw_tickers": json.dumps(["NVDA"])}
    assert primary_ticker(resolve_tickers(row)) == "NVDA"
    assert guess_event_type(row) == "earnings_result"

    second = result.documents[1]
    assert second["published_at"] == "2026-08-10T00:00:00Z"  # no acceptance time → midnight UTC
    assert guess_event_type(
        {"provider": "sec-edgar", "title": second["title"], "raw_tickers": "[]"}
    ) == "management_change"


def test_an_unknown_cik_is_a_skip_with_a_degraded_entry_not_a_guess(conn, monkeypatch, fake):
    monkeypatch.setenv("EVENTS_CONTACT_UA", "attestel ops@example.test")
    result = fetch_sec_filings(conn, ticker="ZZZZ")
    assert fake.calls == []
    assert result.documents == []
    assert result.degraded == ["sec-edgar:no-cik"]


# =================================================================================================
# Google News RSS
# =================================================================================================


RSS_PAYLOAD = """<?xml version="1.0"?>
<rss version="2.0"><channel>
  <item><title>NVIDIA unveils next-generation accelerator</title>
        <link>https://news.test/rss1</link>
        <description>A product announcement.</description>
        <pubDate>Sat, 15 Aug 2026 13:21:00 GMT</pubDate></item>
  <item><title>Undated item</title><link>https://news.test/rss2</link>
        <pubDate>whenever</pubDate></item>
</channel></rss>"""


def test_rss_is_gated_on_the_contact_user_agent(conn, fake):
    """Keyless, but not ungated: without this it is the ONE provider that still opens a socket on a
    `docker compose up` with an empty `.env`. Measured, not hypothesised — see the handoff."""
    result = fetch_rss_news(conn, ticker="NVDA")
    assert fake.calls == []
    assert result.degraded == ["google-news-rss:no-key"]


def test_rss_normalises_items_and_reports_no_tickers(conn, monkeypatch, fake):
    monkeypatch.setenv("EVENTS_CONTACT_UA", "attestel ops@example.test")
    fake.answers["news.google.com"] = RSS_PAYLOAD
    result = fetch_rss_news(conn, ticker="NVDA")

    assert len(result.documents) == 1
    document = result.documents[0]
    assert document["provider"] == "google-news-rss"
    assert document["published_at"] == "2026-08-15T13:21:00Z"
    # Google News reports no entities. Inferring NVDA from the query we happened to send is exactly
    # the inference §3.1 forbids in this column; the alias table recovers it instead.
    assert document["raw_tickers"] == []
    assert primary_ticker(resolve_tickers(
        {"provider": "google-news-rss", "title": document["title"], "raw_tickers": "[]"}
    )) == "NVDA"
    assert result.degraded == ["google-news-rss:unparseable-time"]


# =================================================================================================
# Alpha Vantage
# =================================================================================================


AV_EARNINGS = json.dumps({"quarterlyEarnings": [{
    "fiscalDateEnding": "2026-07-31", "reportedDate": "2026-08-14",
    "reportedEPS": "1.21", "estimatedEPS": "1.10", "surprise": "0.11",
}]})
AV_CALENDAR = "symbol,name,reportDate,fiscalDateEnding,estimate,currency\nNVDA,NVIDIA,2026-11-19,2026-10-31,1.30,USD\nAMD,AMD,2026-11-01,2026-09-30,0.9,USD\n"


def test_alphavantage_produces_documents_and_scheduled_events(conn, monkeypatch, fake):
    monkeypatch.setenv("ALPHAVANTAGE_API_KEY", "k")
    fake.answers["alphavantage.co"] = None  # replaced below by a param-aware stub

    def get(url, params=None, headers=None, timeout=None):
        fake.calls.append({"url": url, "params": params, "headers": headers, "timeout": timeout})
        if params.get("function") == "EARNINGS":
            return FakeResponse(AV_EARNINGS)
        return FakeResponse(AV_CALENDAR)

    fake.get = get
    result = fetch_alphavantage_earnings(conn, ticker="NVDA")

    document = result.documents[0]
    assert document["provider"] == "alphavantage"
    assert document["published_at"] == "2026-08-14T00:00:00Z"
    assert document["raw_tickers"] == ["NVDA"]
    assert guess_event_type(
        {"provider": "alphavantage", "title": document["title"], "raw_tickers": "[]"}
    ) == "earnings_result"

    # EARNINGS_CALENDAR populates one tentative, source-ranked catalyst for this symbol only.
    assert len(result.scheduled) == 1
    scheduled = result.scheduled[0]
    assert {key: scheduled[key] for key in (
        "kind", "ticker", "series", "scheduled_at", "confirmed", "source", "source_tier",
        "status", "importance", "expected", "unit",
    )} == {
        "kind": "earnings", "ticker": "NVDA", "series": None,
        "scheduled_at": "2026-11-19T00:00:00Z", "confirmed": 0,
        "source": "alphavantage", "source_tier": "professional", "status": "tentative",
        "importance": "high", "expected": 1.3, "unit": "USD",
    }
    assert scheduled["title"] == "NVIDIA earnings"
    assert scheduled["description"]
    assert scheduled["occurrence_key"] == "earnings|NVDA|2026-10-31"


def test_alphavantage_rate_limit_arrives_as_http_200(conn, monkeypatch, fake):
    """Alpha Vantage signals a rate limit with a 200 and a `Note` field — the same trap
    `gateway/providers.go::fetchEarnings` documents."""
    monkeypatch.setenv("ALPHAVANTAGE_API_KEY", "k")
    fake.answers["alphavantage.co"] = json.dumps({"Note": "call frequency"})
    result = fetch_alphavantage_earnings(conn, ticker="NVDA")
    assert result.documents == []
    assert "alphavantage:error" in result.degraded


# =================================================================================================
# Federal Reserve, FRED and FMP
# =================================================================================================


FOMC_HTML = """
<div class="panel-heading"><h4><a>2026 FOMC Meetings</a></h4></div>
<div class="row fomc-meeting"><div class="fomc-meeting__month"><strong>January</strong></div>
<div class="fomc-meeting__date">27-28</div></div>
<div class="row fomc-meeting"><div class="fomc-meeting__month"><strong>March</strong></div>
<div class="fomc-meeting__date">17-18*</div></div>
<div class="row fomc-meeting"><div class="fomc-meeting__month"><strong>April</strong></div>
<div class="fomc-meeting__date">28-29</div></div>
<div class="row fomc-meeting"><div class="fomc-meeting__month"><strong>June</strong></div>
<div class="fomc-meeting__date">16-17*</div></div>
<div class="row fomc-meeting"><div class="fomc-meeting__month"><strong>July</strong></div>
<div class="fomc-meeting__date">28-29</div></div>
<div class="row fomc-meeting"><div class="fomc-meeting__month"><strong>August</strong></div>
<div class="fomc-meeting__date">22 (notation vote)</div></div>
<div class="row fomc-meeting"><div class="fomc-meeting__month"><strong>September</strong></div>
<div class="fomc-meeting__date">15-16*</div></div>
<div class="row fomc-meeting"><div class="fomc-meeting__month"><strong>October</strong></div>
<div class="fomc-meeting__date">27-28</div></div>
<div class="row fomc-meeting"><div class="fomc-meeting__month"><strong>December</strong></div>
<div class="fomc-meeting__date">8-9*</div></div>
<div class="panel-heading"><h4><a>2027 FOMC Meetings</a></h4></div>
<div class="row fomc-meeting"><div class="fomc-meeting__month"><strong>Jan/Feb</strong></div>
<div class="fomc-meeting__date">31-1</div></div>
"""


def test_federal_reserve_emits_official_fomc_decisions(conn, monkeypatch, fake):
    monkeypatch.setenv("FEDERAL_RESERVE_ENABLED", "true")
    monkeypatch.setenv("EVENTS_CONTACT_UA", "attestel ops@example.test")
    fake.answers["federalreserve.gov"] = FOMC_HTML
    result = fetch_federal_reserve_calendar(
        conn,
        from_date="2026-09-01",
        to_date="2026-12-31",
        now=datetime(2026, 8, 22, tzinfo=timezone.utc),
    )

    assert result.documents == []
    assert result.degraded == []
    assert [item["scheduled_at"] for item in result.scheduled] == [
        "2026-09-16T18:00:00Z", "2026-10-28T18:00:00Z", "2026-12-09T19:00:00Z",
    ]
    assert [item["status"] for item in result.scheduled] == [
        "confirmed", "tentative", "tentative",
    ]
    september = result.scheduled[0]
    assert september["occurrence_key"] == "fomc|2026|regular-06"
    assert september["source"] == "federal-reserve"
    assert september["source_tier"] == "official"
    assert september["kind"] == "central_bank"
    assert september["series"] == "FOMC"
    assert september["timezone"] == "America/New_York"
    assert september["local_time"] == "14:00"
    assert "Summary of Economic Projections" in september["description"]
    assert fake.calls[0]["headers"]["User-Agent"] == "attestel ops@example.test"


def test_fomc_parser_uses_the_final_day_of_a_cross_month_meeting():
    rows = providers.parse_fomc_calendar(FOMC_HTML)
    assert rows[-1] == {
        "date": "2027-02-01",
        "has_projections": False,
        "occurrence_key": "fomc|2027|regular-01",
    }


FRED_PAYLOAD = json.dumps({"release_dates": [
    {"release_id": 10, "release_name": "Consumer Price Index", "date": "2026-08-12"},
    {"release_id": 999, "release_name": "Something we do not track", "date": "2026-08-13"},
    {"release_id": 50, "release_name": "Employment Situation", "date": "2026-12-01"},
]})


def test_fred_emits_official_documents_and_scheduled_releases(conn, monkeypatch, fake):
    monkeypatch.setenv("FRED_API_KEY", "k")
    fake.answers["stlouisfed.org"] = FRED_PAYLOAD
    result = fetch_fred_releases(conn, from_date="2026-08-01", to_date="2026-09-01")

    assert len(result.documents) == 1  # the unallowlisted release and the out-of-window one are out
    document = result.documents[0]
    assert document["source_tier"] == "official"
    assert document["published_at"] == "2026-08-12T12:30:00Z"
    assert document["raw_tickers"] == []  # a macro release has no subject company
    assert len(result.scheduled) == 1
    scheduled = result.scheduled[0]
    assert {key: scheduled[key] for key in (
        "kind", "ticker", "series", "scheduled_at", "confirmed", "source", "source_tier",
        "status", "importance", "timezone", "local_time",
    )} == {
        "kind": "macro_release", "ticker": None, "series": "CPI",
        "scheduled_at": "2026-08-12T12:30:00Z", "confirmed": 1, "source": "fred",
        "source_tier": "official", "status": "confirmed", "importance": "high",
        "timezone": "America/New_York", "local_time": "08:30",
    }
    assert scheduled["description"]


FMP_PAYLOAD = json.dumps([
    {"date": "2026-08-12 08:30:00", "country": "US", "event": "CPI YoY", "previous": 3.0,
     "estimate": 2.9, "actual": 3.1, "impact": "High", "unit": "%"},
    {"date": "2026-08-12 09:00:00", "country": "DE", "event": "German IFO", "impact": "Low"},
])


def test_fmp_keeps_us_events_only(conn, monkeypatch, fake):
    monkeypatch.setenv("FMP_API_KEY", "k")
    fake.answers["financialmodelingprep.com"] = FMP_PAYLOAD
    result = fetch_fmp_calendar(conn, from_date="2026-08-01", to_date="2026-09-01")
    assert [d["published_at"] for d in result.documents] == ["2026-08-12T08:30:00Z"]
    assert len(result.scheduled) == 1
    assert result.scheduled[0]["series"] == "CPI"
    assert result.scheduled[0]["status"] == "released"
    assert result.scheduled[0]["source_tier"] == "professional"


def test_fmp_paywall_object_is_a_degraded_entry(conn, monkeypatch, fake):
    """FMP signals an error with a JSON OBJECT; success is a JSON array."""
    monkeypatch.setenv("FMP_API_KEY", "k")
    fake.answers["financialmodelingprep.com"] = json.dumps({"Error Message": "premium endpoint"})
    result = fetch_fmp_calendar(conn, from_date="2026-08-01", to_date="2026-09-01")
    assert result.documents == []
    assert result.degraded == ["fmp:error"]


# =================================================================================================
# Failure posture
# =================================================================================================


def test_a_transport_error_never_escapes(conn, monkeypatch):
    monkeypatch.setenv("MARKETAUX_API_KEY", "k")

    class Exploding:
        def get(self, *args, **kwargs):
            raise OSError("connection reset")

    monkeypatch.setattr(providers, "requests", Exploding())
    result = fetch_marketaux(conn, ticker="NVDA")
    assert result.documents == []
    assert result.degraded == ["marketaux:error"]


def test_a_429_blocks_the_provider_for_the_next_call(conn, monkeypatch):
    monkeypatch.setenv("MARKETAUX_API_KEY", "k")
    monkeypatch.setattr(providers, "requests", FakeRequests(
        {"marketaux": {"body": "slow down", "status": 429}}
    ))
    first = fetch_marketaux(conn, ticker="NVDA")
    assert first.degraded == ["marketaux:error"]
    second = fetch_marketaux(conn, ticker="GOOGL")
    assert second.degraded == ["marketaux:backoff"]


def test_over_budget_skips_the_provider_for_the_rest_of_the_day(conn, monkeypatch):
    monkeypatch.setenv("MARKETAUX_API_KEY", "k")
    monkeypatch.setenv("EVENTS_BUDGET_MARKETAUX", "1")
    monkeypatch.setattr(providers, "requests", FakeRequests(
        {"marketaux": json.dumps({"data": []})}
    ))
    assert fetch_marketaux(conn, ticker="NVDA").degraded == []
    assert fetch_marketaux(conn, ticker="GOOGL").degraded == ["marketaux:budget"]
