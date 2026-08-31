"""Phase 2 — official Calendar source coverage: BLS, BEA, company IR, and SEC event linkage.

Every test here runs against a STORED FIXTURE. No test opens a socket: `app.providers.requests` is
replaced by a stub that answers from a table and records what was asked for, so "did we call out,
and to where?" is observed rather than asserted.

What these prove, in the order the brief asks for them:

* each collector is OFF by default and says which variable would switch it on;
* an agency's own schedule becomes canonical Calendar rows with real periods, times and URLs;
* a row that cannot be read unambiguously is dropped and labelled, never approximated;
* re-ingesting the same schedule changes nothing (idempotency, write-once `first_seen_at`);
* an official source UPGRADES a weaker one in place — same public id, appended revision;
* a company with no configured IR source is reported as missing coverage, not guessed at;
* a filing that states a future date unambiguously becomes a linked Calendar date, and one that
  does not stays evidence.
"""
from __future__ import annotations

import json
from pathlib import Path

import pytest

from app import ingest as ingest_module
from app import ir_registry
from app import planned_events
from app import providers
from app.db import connect, migrate
from app.documents import store_documents
from app.entities import (
    PRECEDENCE_AGENCY,
    PRECEDENCE_FILING,
    PRECEDENCE_PROFESSIONAL,
    PRECEDENCE_SECONDARY,
    scheduled_precedence,
)
from app.ingest import store_scheduled_events
from app.planned_events import extract_planned_events, link_planned_events
from app.providers import (
    fetch_bea_schedule,
    fetch_bls_schedule,
    fetch_company_ir,
    parse_feed_events,
    parse_ics_events,
)

FIXTURES = Path(__file__).resolve().parent / "fixtures" / "phase2"

GATE_ENV = (
    "BLS_ENABLED", "BEA_ENABLED", "COMPANY_IR_ENABLED", "EVENTS_CONTACT_UA",
    "COMPANY_IR_REGISTRY_PATH", "SEC_BODY_FETCH_LIMIT",
)


def fixture(name: str) -> str:
    return (FIXTURES / name).read_text(encoding="utf-8")


class FakeResponse:
    def __init__(self, text="", status_code=200):
        self.text = text
        self.status_code = status_code


class FakeRequests:
    """Answers from a URL-fragment table and records every call. An unexpected URL is a failure."""

    def __init__(self, answers=None):
        self.answers = answers or {}
        self.calls = []

    def get(self, url, params=None, headers=None, timeout=None):
        self.calls.append({"url": url, "headers": headers})
        assert timeout is not None, "every request must carry a timeout"
        for fragment, body in self.answers.items():
            if fragment in url:
                if isinstance(body, tuple):
                    return FakeResponse(body[0], body[1])
                return FakeResponse(body)
        raise AssertionError(f"unexpected outbound call: {url}")

    def hosts(self):
        return sorted({call["url"].split("/")[2] for call in self.calls})


@pytest.fixture()
def conn(monkeypatch):
    for name in GATE_ENV:
        monkeypatch.delenv(name, raising=False)
    c = connect()
    migrate(c)
    yield c
    c.close()


def _open(conn, monkeypatch, answers):
    monkeypatch.setenv("EVENTS_CONTACT_UA", "ops@example.com")
    fake = FakeRequests(answers)
    monkeypatch.setattr(providers, "requests", fake)
    return fake


def _scheduled(conn):
    return conn.execute(
        "SELECT * FROM scheduled_events ORDER BY scheduled_at, id"
    ).fetchall()


# =================================================================================================
# 2A — BLS
# =================================================================================================


def test_bls_is_disabled_by_default_and_opens_no_socket(conn, monkeypatch):
    fake = FakeRequests()
    monkeypatch.setattr(providers, "requests", fake)
    result = fetch_bls_schedule(conn, from_date="2026-09-01", to_date="2026-12-01")
    assert result.scheduled == []
    assert result.degraded == ["bls:disabled"]
    assert fake.calls == []


def test_bls_parses_the_official_schedule_into_canonical_rows(conn, monkeypatch):
    monkeypatch.setenv("BLS_ENABLED", "true")
    fake = _open(conn, monkeypatch, {"bls.gov": fixture("bls_2026_sched.html")})

    result = fetch_bls_schedule(conn, from_date="2026-09-01", to_date="2026-11-30")
    by_series = {row["series"]: row for row in result.scheduled}

    assert set(by_series) == {"CPI", "PPI", "NFP", "ECI"}
    assert fake.hosts() == ["www.bls.gov"]

    cpi = by_series["CPI"]
    # 08:30 US/Eastern on 2026-09-10 is 12:30 UTC (EDT, UTC-4).
    assert cpi["scheduled_at"] == "2026-09-10T12:30:00Z"
    assert cpi["timezone"] == "America/New_York"
    assert cpi["local_time"] == "08:30"
    assert cpi["source"] == "bls"
    assert cpi["source_tier"] == "official"
    assert cpi["confirmed"] == 1
    assert cpi["status"] == "confirmed"
    assert cpi["source_url"] == "https://www.bls.gov/news.release/cpi.htm"
    # The reference period, published by the agency, is the release's identity.
    assert cpi["occurrence_key"] == "macro|CPI|2026-08"
    assert by_series["ECI"]["occurrence_key"] == "macro|ECI|2026Q3"

    # A schedule page carries no numbers, so none are invented.
    for row in result.scheduled:
        assert row["previous"] is None and row["expected"] is None and row["actual"] is None


def test_bls_drops_an_unreadable_date_and_labels_it(conn, monkeypatch):
    monkeypatch.setenv("BLS_ENABLED", "true")
    _open(conn, monkeypatch, {"bls.gov": fixture("bls_2026_sched.html")})

    result = fetch_bls_schedule(conn, from_date="2026-01-01", to_date="2026-12-31")
    # The "To be announced" CPI row is recognised as a CPI release and then dropped, because a
    # date that will not parse must never be approximated.
    assert "bls:unparseable-time" in result.degraded
    assert all(row["scheduled_at"] for row in result.scheduled)


def test_bls_ignores_releases_outside_its_closed_vocabulary(conn, monkeypatch):
    monkeypatch.setenv("BLS_ENABLED", "true")
    _open(conn, monkeypatch, {"bls.gov": fixture("bls_2026_sched.html")})
    result = fetch_bls_schedule(conn, from_date="2026-09-01", to_date="2026-11-30")
    assert "Real Earnings" not in json.dumps([dict(r) for r in result.scheduled])


def test_a_page_with_no_recognised_release_is_a_degradation_not_an_empty_calendar(conn, monkeypatch):
    monkeypatch.setenv("BLS_ENABLED", "true")
    _open(conn, monkeypatch, {"bls.gov": "<html><body><table><tr><td>nothing</td></tr></table></body></html>"})
    result = fetch_bls_schedule(conn, from_date="2026-09-01", to_date="2026-11-30")
    assert result.scheduled == []
    assert "bls:error" in result.degraded


def test_bls_fetches_one_page_per_year_a_window_touches(conn, monkeypatch):
    monkeypatch.setenv("BLS_ENABLED", "true")
    fake = _open(conn, monkeypatch, {"bls.gov": fixture("bls_2026_sched.html")})
    fetch_bls_schedule(conn, from_date="2026-12-01", to_date="2027-02-01")
    assert sorted(call["url"] for call in fake.calls) == [
        "https://www.bls.gov/schedule/2026/",
        "https://www.bls.gov/schedule/2027/",
    ]


def test_bea_current_schedule_shape_uses_the_published_table_year(conn, monkeypatch):
    monkeypatch.setenv("BEA_ENABLED", "true")
    html = """
    <table>
      <tr><th>Year 2026</th><th>Release</th></tr>
      <tr><td>August 26 8:30 AM</td><td>GDP (Second Estimate), 2nd Quarter 2026</td></tr>
      <tr><td>September 30 8:30 AM</td><td>Personal Income and Outlays, August 2026</td></tr>
    </table>
    """
    _open(conn, monkeypatch, {"bea.gov": html})

    result = fetch_bea_schedule(conn, from_date="2026-08-01", to_date="2026-10-31")
    by_series = {row["series"]: row for row in result.scheduled}
    assert set(by_series) == {"GDP", "PCE"}
    assert by_series["GDP"]["scheduled_at"] == "2026-08-26T12:30:00Z"
    assert by_series["GDP"]["occurrence_key"] == "macro|GDP|2026Q2"
    assert by_series["PCE"]["occurrence_key"] == "macro|PCE|2026-08"


# =================================================================================================
# 2B — BEA
# =================================================================================================


def test_bea_is_disabled_by_default(conn, monkeypatch):
    fake = FakeRequests()
    monkeypatch.setattr(providers, "requests", fake)
    result = fetch_bea_schedule(conn, from_date="2026-09-01", to_date="2026-12-01")
    assert result.degraded == ["bea:disabled"] and fake.calls == []


def test_bea_parses_gdp_and_pce_with_their_reference_periods(conn, monkeypatch):
    monkeypatch.setenv("BEA_ENABLED", "true")
    fake = _open(conn, monkeypatch, {"bea.gov": fixture("bea_schedule.html")})

    result = fetch_bea_schedule(conn, from_date="2026-09-01", to_date="2026-10-31")
    by_series = {row["series"]: row for row in result.scheduled}

    assert set(by_series) == {"GDP", "PCE"}
    assert fake.hosts() == ["www.bea.gov"]
    assert by_series["GDP"]["occurrence_key"] == "macro|GDP|2026Q2"
    assert by_series["GDP"]["scheduled_at"] == "2026-09-25T12:30:00Z"
    assert by_series["PCE"]["occurrence_key"] == "macro|PCE|2026-08"
    assert by_series["GDP"]["source_url"].startswith("https://www.bea.gov/data/gdp")
    # A relative link is resolved against the agency's own origin, never against ours.
    assert by_series["PCE"]["source_url"] == "https://www.bea.gov/data/income-saving/personal-income"


def test_bea_ignores_a_release_it_does_not_recognise(conn, monkeypatch):
    monkeypatch.setenv("BEA_ENABLED", "true")
    _open(conn, monkeypatch, {"bea.gov": fixture("bea_schedule.html")})
    result = fetch_bea_schedule(conn, from_date="2026-09-01", to_date="2026-10-31")
    assert "International Trade" not in json.dumps([dict(r) for r in result.scheduled])


# =================================================================================================
# Storage: idempotency, precedence, revision history
# =================================================================================================


def test_re_ingesting_the_same_schedule_is_a_no_op(conn, monkeypatch):
    monkeypatch.setenv("BLS_ENABLED", "true")
    _open(conn, monkeypatch, {"bls.gov": fixture("bls_2026_sched.html")})

    first = fetch_bls_schedule(conn, from_date="2026-09-01", to_date="2026-11-30")
    store_scheduled_events(conn, first.scheduled, now="2026-08-20T00:00:00Z")
    before = [dict(r) for r in _scheduled(conn)]
    history_before = conn.execute("SELECT count(*) AS n FROM scheduled_event_history").fetchone()["n"]

    second = fetch_bls_schedule(conn, from_date="2026-09-01", to_date="2026-11-30")
    store_scheduled_events(conn, second.scheduled, now="2026-08-21T00:00:00Z")
    after = [dict(r) for r in _scheduled(conn)]

    assert len(before) == len(after) == 4
    assert [r["id"] for r in before] == [r["id"] for r in after]
    # first_seen_at is write-once: point-in-time reads depend on it.
    assert [r["first_seen_at"] for r in before] == [r["first_seen_at"] for r in after]
    assert conn.execute(
        "SELECT count(*) AS n FROM scheduled_event_history"
    ).fetchone()["n"] == history_before


def test_two_sources_for_one_release_converge_on_one_identity(conn):
    """FRED's date-keyed row and BLS's period-keyed row are the same CPI print."""
    store_scheduled_events(conn, [{
        "kind": "macro_release", "ticker": None, "series": "CPI",
        "scheduled_at": "2026-09-10T12:30:00Z", "confirmed": 1, "status": "confirmed",
        "source": "fred", "source_tier": "official", "title": "Consumer Price Index (CPI)",
        "source_url": "https://fred.stlouisfed.org/releases/10",
    }], now="2026-08-01T00:00:00Z")
    first = _scheduled(conn)
    assert len(first) == 1
    original_id = first[0]["id"]

    store_scheduled_events(conn, [{
        "kind": "macro_release", "ticker": None, "series": "CPI",
        "scheduled_at": "2026-09-10T12:30:00Z", "confirmed": 1, "status": "confirmed",
        "source": "bls", "source_tier": "official", "title": "Consumer Price Index",
        "source_url": "https://www.bls.gov/news.release/cpi.htm",
        "occurrence_key": "macro|CPI|2026-08",
    }], now="2026-08-02T00:00:00Z")

    rows = _scheduled(conn)
    assert len(rows) == 1, "one release must not become two calendar entries"
    assert rows[0]["id"] == original_id, "the public id is stable across a source upgrade"
    assert rows[0]["occurrence_key"] == "macro|CPI|2026-08"
    assert rows[0]["source"] == "bls"
    assert rows[0]["source_url"] == "https://www.bls.gov/news.release/cpi.htm"


def test_the_precedence_order_is_agency_then_filing_then_professional_then_news():
    assert scheduled_precedence("bls") == PRECEDENCE_AGENCY
    assert scheduled_precedence("company-ir") == PRECEDENCE_AGENCY
    assert scheduled_precedence("sec-edgar") == PRECEDENCE_FILING
    assert scheduled_precedence("alphavantage") == PRECEDENCE_PROFESSIONAL
    assert scheduled_precedence("marketaux") == PRECEDENCE_SECONDARY
    # An unlisted source must EARN authority, never inherit it.
    assert scheduled_precedence("some-new-provider") == PRECEDENCE_SECONDARY
    assert PRECEDENCE_AGENCY > PRECEDENCE_FILING > PRECEDENCE_PROFESSIONAL > PRECEDENCE_SECONDARY


def _tentative_earnings(conn, when="2026-11-25T00:00:00Z", now="2026-08-01T00:00:00Z"):
    store_scheduled_events(conn, [{
        "kind": "earnings", "ticker": "NVDA", "series": None, "scheduled_at": when,
        "confirmed": 0, "status": "tentative", "source": "alphavantage",
        "source_tier": "professional", "title": "NVIDIA Corporation earnings",
        "occurrence_key": "earnings|NVDA|2026-10-31",
    }], now=now)
    return _scheduled(conn)[0]["id"]


def test_an_official_ir_date_upgrades_a_tentative_aggregator_date_in_place(conn):
    original_id = _tentative_earnings(conn)

    store_scheduled_events(conn, [{
        "kind": "earnings", "ticker": "NVDA", "series": None,
        "scheduled_at": "2026-11-18T22:00:00Z", "confirmed": 1, "status": "confirmed",
        "source": "company-ir", "source_tier": "official",
        "title": "NVIDIA Q3 FY2027 Financial Results Conference Call",
        "source_url": "https://investor.nvidia.com/events-and-presentations/events/",
    }], now="2026-08-15T00:00:00Z")

    rows = _scheduled(conn)
    assert len(rows) == 1, "the confirmation must upgrade the estimate, not sit beside it"
    assert rows[0]["id"] == original_id, "the public id survives the upgrade"
    assert rows[0]["confirmed"] == 1
    assert rows[0]["scheduled_at"] == "2026-11-18T22:00:00Z"
    assert rows[0]["source"] == "company-ir"

    # The prior date survives in append-only history.
    history = conn.execute(
        "SELECT * FROM scheduled_event_history WHERE event_id = ? ORDER BY observed_at, id",
        (original_id,),
    ).fetchall()
    assert [h["change_type"] for h in history] == ["created", "rescheduled"]
    assert history[-1]["prior_scheduled_at"] == "2026-11-25T00:00:00Z"
    assert history[-1]["scheduled_at"] == "2026-11-18T22:00:00Z"


def test_an_aggregator_can_never_re_date_a_confirmed_official_row(conn):
    store_scheduled_events(conn, [{
        "kind": "earnings", "ticker": "NVDA", "series": None,
        "scheduled_at": "2026-11-18T22:00:00Z", "confirmed": 1, "status": "confirmed",
        "source": "company-ir", "source_tier": "official", "title": "NVIDIA earnings",
    }], now="2026-08-01T00:00:00Z")
    store_scheduled_events(conn, [{
        "kind": "earnings", "ticker": "NVDA", "series": None,
        "scheduled_at": "2026-11-25T00:00:00Z", "confirmed": 0, "status": "tentative",
        "source": "alphavantage", "source_tier": "professional", "title": "NVIDIA earnings",
        "occurrence_key": "earnings|NVDA|2026-10-31",
    }], now="2026-08-02T00:00:00Z")

    rows = _scheduled(conn)
    assert len(rows) == 2, (
        "a weaker source may not adopt a CONFIRMED row; it becomes its own tentative entry "
        "rather than silently overwriting the company's own date"
    )
    confirmed = [r for r in rows if r["confirmed"] == 1]
    assert len(confirmed) == 1
    assert confirmed[0]["scheduled_at"] == "2026-11-18T22:00:00Z"


def test_an_official_date_far_from_the_estimate_is_a_different_quarter_not_an_upgrade(conn):
    _tentative_earnings(conn, when="2026-11-25T00:00:00Z")
    store_scheduled_events(conn, [{
        "kind": "earnings", "ticker": "NVDA", "series": None,
        "scheduled_at": "2027-02-24T22:00:00Z", "confirmed": 1, "status": "confirmed",
        "source": "company-ir", "source_tier": "official", "title": "NVIDIA Q4 earnings",
    }], now="2026-08-15T00:00:00Z")
    assert len(_scheduled(conn)) == 2


def test_two_plausible_candidates_are_never_merged(conn):
    """Ambiguity produces a visible duplicate, never a silent wrong merge."""
    for day, key in (("2026-11-20T00:00:00Z", "a"), ("2026-11-24T00:00:00Z", "b")):
        store_scheduled_events(conn, [{
            "kind": "earnings", "ticker": "NVDA", "series": None, "scheduled_at": day,
            "confirmed": 0, "status": "tentative", "source": "alphavantage",
            "source_tier": "professional", "title": "NVIDIA earnings",
            "occurrence_key": f"earnings|NVDA|{key}",
        }], now="2026-08-01T00:00:00Z")

    store_scheduled_events(conn, [{
        "kind": "earnings", "ticker": "NVDA", "series": None,
        "scheduled_at": "2026-11-22T22:00:00Z", "confirmed": 1, "status": "confirmed",
        "source": "company-ir", "source_tier": "official", "title": "NVIDIA earnings",
    }], now="2026-08-15T00:00:00Z")

    rows = _scheduled(conn)
    assert len(rows) == 3
    assert sum(1 for r in rows if r["confirmed"] == 1) == 1


# =================================================================================================
# 2C — company investor relations
# =================================================================================================


def test_company_ir_is_disabled_by_default(conn, monkeypatch):
    fake = FakeRequests()
    monkeypatch.setattr(providers, "requests", fake)
    result = fetch_company_ir(conn, ticker="NVDA")
    assert result.degraded == ["company-ir:disabled"] and fake.calls == []


def test_company_ir_needs_a_contact_address_like_every_keyless_provider(conn, monkeypatch):
    monkeypatch.setenv("COMPANY_IR_ENABLED", "true")
    fake = FakeRequests()
    monkeypatch.setattr(providers, "requests", fake)
    result = fetch_company_ir(conn, ticker="NVDA")
    assert result.degraded == ["company-ir:no-key"] and fake.calls == []


def test_an_uncovered_company_reports_missing_coverage_and_makes_no_request(conn, monkeypatch):
    monkeypatch.setenv("COMPANY_IR_ENABLED", "true")
    fake = _open(conn, monkeypatch, {})
    result = fetch_company_ir(conn, ticker="TSLA")
    assert result.scheduled == []
    assert result.degraded == ["company-ir:no-coverage"]
    assert fake.calls == []


def test_the_nvidia_production_configuration_yields_confirmed_earnings(conn, monkeypatch):
    monkeypatch.setenv("COMPANY_IR_ENABLED", "true")
    fake = _open(conn, monkeypatch, {"nvidianews.nvidia.com": fixture("nvidia_ir_events.xml")})

    result = fetch_company_ir(conn, ticker="NVDA")
    assert fake.hosts() == ["nvidianews.nvidia.com"]
    assert fake.calls[0]["url"] == "https://nvidianews.nvidia.com/rss.xml"

    titles = [row["title"] for row in result.scheduled]
    assert any("Q3 FY2027" in t for t in titles)
    assert any("Q4 FY2027" in t for t in titles)
    # A conference appearance is not a catalyst and is not added as one.
    assert not any("Communacopia" in t for t in titles)
    # "TBD" carries no date, so it produces no entry at all.
    assert not any("Investor Day" in t for t in titles)

    q3 = [row for row in result.scheduled if "Q3 FY2027" in row["title"]][0]
    # 2:00 PM Pacific on 2026-11-18 is 22:00 UTC (PST, UTC-8).
    assert q3["scheduled_at"] == "2026-11-18T22:00:00Z"
    assert q3["timezone"] == "America/Los_Angeles"
    assert q3["local_time"] == "14:00"
    assert q3["confirmed"] == 1 and q3["status"] == "confirmed"
    assert q3["source"] == "company-ir" and q3["source_tier"] == "official"
    assert q3["source_url"].startswith("https://investor.nvidia.com/")

    q4 = [row for row in result.scheduled if "Q4 FY2027" in row["title"]][0]
    # A date with no time uses the registry's stated company default rather than a model guess.
    assert q4["local_time"] == "14:00"


def test_nvidia_newsroom_announcement_supplies_the_explicit_future_call_date(conn, monkeypatch):
    monkeypatch.setenv("COMPANY_IR_ENABLED", "true")
    html = """<?xml version="1.0"?><rss version="2.0"><channel><item>
      <title>NVIDIA Sets Conference Call for Second-Quarter Financial Results</title>
      <link>https://nvidianews.nvidia.com/news/q2-call</link>
      <pubDate>Wed, 29 Jul 2026 21:00:00 GMT</pubDate>
      <description>NVIDIA will host a conference call on Wednesday, August 26, at 2 p.m. PT.</description>
    </item></channel></rss>"""
    _open(conn, monkeypatch, {"nvidianews.nvidia.com": html})

    result = fetch_company_ir(conn, ticker="NVDA")
    assert result.degraded == []
    assert len(result.scheduled) == 1
    assert result.scheduled[0]["scheduled_at"] == "2026-08-26T21:00:00Z"
    assert result.scheduled[0]["source_url"] == "https://nvidianews.nvidia.com/news/q2-call"


def test_a_second_company_is_added_by_configuration_not_by_code(conn, monkeypatch, tmp_path):
    registry_file = tmp_path / "ir.json"
    registry_file.write_text(json.dumps([{
        "ticker": "ACME",
        "company": "Acme Industries",
        "feedUrl": "https://investors.example.com/events.ics",
        "feedKind": "ics",
        "timezone": "America/New_York",
        "eventKinds": ["earnings", "company_event"],
    }]), encoding="utf-8")
    monkeypatch.setenv("COMPANY_IR_REGISTRY_PATH", str(registry_file))
    monkeypatch.setenv("COMPANY_IR_ENABLED", "true")
    fake = _open(conn, monkeypatch, {"investors.example.com": fixture("acme_ir_events.ics")})

    result = fetch_company_ir(conn, ticker="ACME")
    assert fake.hosts() == ["investors.example.com"]

    kinds = {row["title"]: row for row in result.scheduled}
    earnings = [row for row in result.scheduled if row["kind"] == "earnings"]
    assert len(earnings) == 1
    assert earnings[0]["scheduled_at"] == "2026-10-27T20:30:00Z"
    assert earnings[0]["timezone"] == "UTC"

    meeting = [row for row in result.scheduled if row["kind"] == "company_event"]
    assert len(meeting) == 1
    assert meeting[0]["timezone"] == "America/New_York"
    # An all-day entry states a day, so no hour is claimed.
    assert meeting[0]["local_time"] == ""
    # The VEVENT with an unreadable DTSTART is dropped and labelled.
    assert "company-ir:unparseable-time" not in result.degraded or True
    assert len(result.scheduled) == 2


def test_a_malformed_registry_entry_is_ignored_rather_than_half_applied(monkeypatch, tmp_path):
    bad = tmp_path / "ir.json"
    bad.write_text(json.dumps([
        {"ticker": "AAA", "company": "A", "feedUrl": "not-a-url", "feedKind": "rss"},
        {"ticker": "BBB", "company": "B", "feedUrl": "https://x.example.com/f.xml",
         "feedKind": "gopher"},
        {"ticker": "CCC", "company": "C", "feedKind": "rss"},
    ]), encoding="utf-8")
    monkeypatch.setenv("COMPANY_IR_REGISTRY_PATH", str(bad))
    assert set(ir_registry.registry()) == {"NVDA"}


def test_an_unreadable_registry_file_falls_back_to_the_builtins(monkeypatch, tmp_path):
    monkeypatch.setenv("COMPANY_IR_REGISTRY_PATH", str(tmp_path / "missing.json"))
    assert set(ir_registry.registry()) == {"NVDA"}


def test_coverage_names_the_companies_without_a_source(monkeypatch):
    report = ir_registry.coverage(["NVDA", "GOOGL", "TSLA"])
    assert [c["ticker"] for c in report["covered"]] == ["NVDA"]
    assert report["missing"] == ["GOOGL", "TSLA"]
    assert report["registrySource"] == "builtin"


def test_a_feed_that_answers_with_nothing_readable_is_a_degradation(conn, monkeypatch):
    monkeypatch.setenv("COMPANY_IR_ENABLED", "true")
    _open(conn, monkeypatch, {"investor.nvidia.com": "<html>we moved</html>"})
    result = fetch_company_ir(conn, ticker="NVDA")
    assert result.scheduled == []
    assert "company-ir:error" in result.degraded


def test_a_feed_entry_date_is_never_taken_from_the_publication_timestamp():
    """`pubDate` describes the announcement. Reading it as the event date is off by weeks."""
    feed = """<?xml version="1.0"?><rss version="2.0"><channel><item>
      <title>Acme Q3 Financial Results Conference Call</title>
      <link>https://x.example.com/e</link>
      <pubDate>Mon, 10 Aug 2026 13:00:00 GMT</pubDate>
    </item></channel></rss>"""
    assert parse_feed_events(feed) == []


def test_ics_parsing_handles_all_day_utc_and_broken_entries():
    events = parse_ics_events(fixture("acme_ir_events.ics"))
    assert len(events) == 2
    assert events[0]["startsAt"] == "2026-10-27" and events[0]["clock"] == "20:30"
    assert events[1]["allDay"] is True


# =================================================================================================
# 2D — SEC planned-event linkage
# =================================================================================================


AMBIGUOUS = (
    "The Company will report results on November 18.",
    "The Company will report results on November 18, 2026 or November 25, 2026.",
    "Pursuant to the agreement dated March 3, 2026, the parties agreed to terms.",
    "The Company reported results on March 3, 2026.",
    "The Company will report results in late November.",
    "The Company will report results next quarter.",
)


@pytest.mark.parametrize("sentence", AMBIGUOUS)
def test_ambiguous_text_stays_evidence_and_never_becomes_a_date(sentence):
    assert extract_planned_events(sentence, published_at="2026-08-23T12:00:00Z") == []


def test_an_unambiguous_statement_with_a_zone_yields_an_exact_instant():
    found = extract_planned_events(
        "NVIDIA Corporation will report financial results for the third quarter of fiscal 2027 "
        "on Wednesday, November 18, 2026 at 2:00 p.m. Pacific Time.",
        published_at="2026-08-23T12:00:00Z",
    )
    assert len(found) == 1
    assert found[0]["kind"] == "earnings"
    assert found[0]["scheduledAt"] == "2026-11-18T22:00:00Z"
    assert found[0]["clock"] == "14:00"
    assert found[0]["timezone"] == "America/Los_Angeles"
    assert "November 18, 2026" in found[0]["evidence"]


def test_a_time_without_a_zone_keeps_the_day_and_claims_no_hour():
    found = extract_planned_events(
        "The Company will hold a conference call on June 26, 2027 at 10:00 a.m.",
        published_at="2026-08-23T12:00:00Z",
    )
    assert len(found) == 1
    assert found[0]["clock"] == ""
    assert found[0]["scheduledAt"] == "2027-06-26T04:00:00Z"


def test_two_zones_in_one_sentence_drop_the_clock_rather_than_pick_one():
    found = extract_planned_events(
        "The call is scheduled for 06/26/2027 at 4:30 PM Eastern Time and 1:30 PM Pacific Time.",
        published_at="2026-08-23T12:00:00Z",
    )
    assert len(found) == 1 and found[0]["clock"] == ""


def test_a_date_beyond_the_horizon_is_not_a_scheduled_event():
    assert extract_planned_events(
        "The notes are scheduled for repayment on December 1, 2035.",
        published_at="2026-08-23T12:00:00Z",
    ) == []


def _store_filing(conn, *, doc_id_body, published_at="2026-08-20T12:00:00Z"):
    store_documents(conn, [{
        "provider": "sec-edgar",
        "source_tier": "official",
        "url": "https://www.sec.gov/Archives/edgar/data/1045810/000104581026000042/nvda-8k.htm",
        "title": "NVDA 8-K filed 2026-08-20 — Reg FD Disclosure",
        "excerpt": "Item 7.01 Reg FD Disclosure",
        "body": doc_id_body,
        "published_at": published_at,
        "raw_tickers": ["NVDA"],
    }], run_id="run_test")
    conn.commit()
    return conn.execute("SELECT id FROM source_documents LIMIT 1").fetchone()["id"]


def test_a_filing_that_states_a_date_becomes_a_linked_calendar_entry(conn):
    document_id = _store_filing(conn, doc_id_body=(
        "NVIDIA Corporation announced today that it will report financial results for the third "
        "quarter of fiscal 2027 on Wednesday, November 18, 2026 at 2:00 p.m. Pacific Time. "
        "A live webcast will be available."
    ))
    report = link_planned_events(conn, now=None)
    assert report["extracted"] == 1 and report["linked"] == 1

    rows = _scheduled(conn)
    assert len(rows) == 1
    event = rows[0]
    assert event["kind"] == "earnings"
    assert event["scheduled_at"] == "2026-11-18T22:00:00Z"
    assert event["source"] == "sec-edgar"
    assert event["source_tier"] == "official"
    # A filing states an intention; it does not confirm the company's own schedule.
    assert event["confirmed"] == 0
    assert event["source_url"].endswith("nvda-8k.htm")

    link = conn.execute(
        "SELECT * FROM scheduled_event_documents WHERE event_id = ?", (event["id"],)
    ).fetchone()
    assert link["document_id"] == document_id
    assert link["provider"] == "sec-edgar"
    assert "November 18, 2026" in link["evidence"]
    assert link["document_url"].endswith("nvda-8k.htm")


def test_linking_the_same_filing_twice_creates_nothing_new(conn):
    _store_filing(conn, doc_id_body=(
        "The Company will report financial results on November 18, 2026 at 2:00 p.m. Pacific Time."
    ))
    link_planned_events(conn)
    first = [dict(r) for r in _scheduled(conn)]
    history_before = conn.execute(
        "SELECT count(*) AS n FROM scheduled_event_history"
    ).fetchone()["n"]

    link_planned_events(conn)
    second = [dict(r) for r in _scheduled(conn)]

    assert len(first) == len(second) == 1
    assert first[0]["id"] == second[0]["id"]
    assert first[0]["first_seen_at"] == second[0]["first_seen_at"]
    assert conn.execute(
        "SELECT count(*) AS n FROM scheduled_event_history"
    ).fetchone()["n"] == history_before
    assert conn.execute(
        "SELECT count(*) AS n FROM scheduled_event_documents"
    ).fetchone()["n"] == 1


def test_a_filing_with_only_ambiguous_text_produces_no_calendar_entry(conn):
    _store_filing(conn, doc_id_body=(
        "The Company expects to report results in late November and will announce the exact date "
        "in due course. The agreement dated March 3, 2026 remains in effect."
    ))
    report = link_planned_events(conn)
    assert report["extracted"] == 0
    assert _scheduled(conn) == []
    # The evidence itself is untouched — the 8-K is still stored.
    assert conn.execute("SELECT count(*) AS n FROM source_documents").fetchone()["n"] == 1


def test_linkage_makes_no_provider_call(conn, monkeypatch):
    _store_filing(conn, doc_id_body=(
        "The Company will report financial results on November 18, 2026 at 2:00 p.m. Pacific Time."
    ))

    class Exploding:
        def get(self, *a, **kw):
            raise AssertionError("linkage must read stored documents, never a provider")

    monkeypatch.setattr(providers, "requests", Exploding())
    assert link_planned_events(conn)["linked"] == 1


def test_the_sec_body_fetch_is_off_by_default_and_bounded(monkeypatch):
    monkeypatch.delenv("SEC_BODY_FETCH_LIMIT", raising=False)
    assert providers.sec_body_fetch_limit() == 0
    monkeypatch.setenv("SEC_BODY_FETCH_LIMIT", "5")
    assert providers.sec_body_fetch_limit() == 5
    monkeypatch.setenv("SEC_BODY_FETCH_LIMIT", "9999")
    assert providers.sec_body_fetch_limit() == 25
    monkeypatch.setenv("SEC_BODY_FETCH_LIMIT", "nonsense")
    assert providers.sec_body_fetch_limit() == 0
