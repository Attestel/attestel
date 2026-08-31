"""Macro releases, surprise arithmetic and the store-backed calendar (contract §3.9, §3.11, §9.27).

One test per refusal guard. A silent unit coercion or a post-release "consensus" here would corrupt
Wave 5A's rung F with a number that looks entirely reasonable, and nothing downstream would notice.
"""
from __future__ import annotations


import pytest
from fastapi.testclient import TestClient

from app.db import IntegrityError, connect, migrate
from app.macro import (
    SURPRISE_REFUSAL_CAPTURED_AFTER_RELEASE,
    SURPRISE_REFUSAL_MISSING_ACTUAL,
    SURPRISE_REFUSAL_MISSING_EXPECTED,
    SURPRISE_REFUSAL_NO_CAPTURE_TIME,
    SURPRISE_REFUSAL_NO_EXPECTATION_SOURCE,
    SURPRISE_REFUSAL_UNIT_MISMATCH,
    compute_surprise,
    record_macro_release,
)

RELEASE_AT = "2026-08-12T12:30:00Z"
BEFORE_RELEASE = "2026-08-12T11:30:00Z"
AFTER_RELEASE = "2026-08-12T13:30:00Z"


@pytest.fixture()
def conn(tmp_path, monkeypatch):
    c = connect()
    migrate(c)
    yield c
    c.close()


@pytest.fixture()
def client(conn):
    from app.main import app

    with TestClient(app) as c:
        yield c


def insert_event(conn, event_id="evt_macro00000000a1", *, event_type="macro_release",
                 published_at="2026-08-12T12:30:00Z", first_seen_at=None):
    conn.execute(
        "INSERT INTO events (id, event_type, title, summary, occurred_at, published_at, "
        "first_seen_at, source_tier, official_confirmed, importance, novelty, document_count, "
        "cluster_key) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (event_id, event_type, "CPI release", "", published_at, published_at,
         first_seen_at or published_at, "official", 1, 0.8, 1.0, 1, "abc123def4567890"),
    )
    conn.commit()
    return event_id


BASE = dict(
    actual=2.8,
    expected=3.1,
    unit="pp",
    expectation_source="fmp",
    expectation_captured_at=BEFORE_RELEASE,
    release_at=RELEASE_AT,
)


# ---- the happy path ------------------------------------------------------------------------------


def test_cpi_actual_below_a_consensus_captured_before_the_release():
    surprise, reason = compute_surprise(**BASE)
    assert surprise == pytest.approx(-0.3)
    assert reason is None


def test_the_happy_path_round_trips_through_the_store(conn):
    event_id = insert_event(conn)
    macro_id = record_macro_release(
        conn, event_id=event_id, series="CPI", release_at=RELEASE_AT, actual=2.8, expected=3.1,
        previous=3.0, unit="pp", expectation_source="fmp",
        expectation_captured_at=BEFORE_RELEASE,
    )
    row = conn.execute("SELECT * FROM macro_events WHERE id = ?", (macro_id,)).fetchone()
    assert macro_id.startswith("mac_") and len(macro_id) == 20
    assert row["surprise"] == pytest.approx(-0.3)
    assert row["unit"] == "pp"


# ---- one test per refusal guard ------------------------------------------------------------------


def test_refuses_when_actual_is_missing():
    surprise, reason = compute_surprise(**{**BASE, "actual": None})
    assert surprise is None and reason == SURPRISE_REFUSAL_MISSING_ACTUAL


def test_refuses_when_expected_is_missing():
    surprise, reason = compute_surprise(**{**BASE, "expected": None})
    assert surprise is None and reason == SURPRISE_REFUSAL_MISSING_EXPECTED


def test_refuses_when_there_is_no_expectation_source():
    """`none` means we have no consensus — which is different from having one that happens to be
    zero."""
    surprise, reason = compute_surprise(**{**BASE, "expectation_source": "none"})
    assert surprise is None and reason == SURPRISE_REFUSAL_NO_EXPECTATION_SOURCE
    surprise, reason = compute_surprise(**{**BASE, "expectation_source": None})
    assert surprise is None and reason == SURPRISE_REFUSAL_NO_EXPECTATION_SOURCE


def test_refuses_when_the_capture_time_is_unknown():
    """Without a capture time we cannot know the consensus predated the release."""
    surprise, reason = compute_surprise(**{**BASE, "expectation_captured_at": None})
    assert surprise is None and reason == SURPRISE_REFUSAL_NO_CAPTURE_TIME


def test_refuses_a_consensus_captured_after_the_release():
    """§3.9, enforced not advised: a post-release consensus is just the actual, wearing a hat."""
    surprise, reason = compute_surprise(**{**BASE, "expectation_captured_at": AFTER_RELEASE})
    assert surprise is None and reason == SURPRISE_REFUSAL_CAPTURED_AFTER_RELEASE


def test_refuses_a_consensus_captured_exactly_at_the_release():
    surprise, reason = compute_surprise(**{**BASE, "expectation_captured_at": RELEASE_AT})
    assert surprise is None and reason == SURPRISE_REFUSAL_CAPTURED_AFTER_RELEASE


def test_refuses_a_unit_mismatch_rather_than_converting():
    """A `pct` minus a `pp` is not a surprise, it is a bug with a number in front of it."""
    surprise, reason = compute_surprise(**{**BASE, "unit": "pct", "expectation_unit": "pp"})
    assert surprise is None and reason == SURPRISE_REFUSAL_UNIT_MISMATCH


def test_matching_units_are_not_a_mismatch():
    surprise, _ = compute_surprise(**{**BASE, "unit": "pp", "expectation_unit": "pp"})
    assert surprise == pytest.approx(-0.3)


def test_a_refused_surprise_is_stored_as_null(conn):
    event_id = insert_event(conn)
    macro_id = record_macro_release(
        conn, event_id=event_id, series="CPI", release_at=RELEASE_AT, actual=2.8, expected=3.1,
        unit="pp", expectation_source="fmp", expectation_captured_at=AFTER_RELEASE,
    )
    row = conn.execute("SELECT surprise FROM macro_events WHERE id = ?", (macro_id,)).fetchone()
    assert row["surprise"] is None


def test_the_database_refuses_a_lie_the_code_somehow_let_through(conn):
    """Belt and braces: 0001_init.sql carries the same timing rule as a CHECK constraint."""
    event_id = insert_event(conn)
    with pytest.raises(IntegrityError):
        conn.execute(
            "INSERT INTO macro_events (id, event_id, series, release_at, actual, expected, "
            "surprise, unit, expectation_source, expectation_captured_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
            ("mac_0000000000000001", event_id, "CPI", RELEASE_AT, 2.8, 3.1, -0.3, "pp", "fmp",
             AFTER_RELEASE),
        )
        conn.commit()


# ---- GET /macro ----------------------------------------------------------------------------------


def test_macro_endpoint_shape(conn, client):
    event_id = insert_event(conn)
    record_macro_release(
        conn, event_id=event_id, series="CPI", release_at=RELEASE_AT, actual=2.8, expected=3.1,
        unit="pp", expectation_source="fmp", expectation_captured_at=BEFORE_RELEASE,
    )
    body = client.get("/macro").json()
    assert body["asOf"]
    assert body["degraded"] == []
    assert len(body["macro"]) == 1
    assert body["macro"][0]["series"] == "CPI"
    assert body["macro"][0]["surprise"] == pytest.approx(-0.3)


def test_macro_series_filter(conn, client):
    for series in ("CPI", "NFP"):
        event_id = insert_event(conn, event_id=f"evt_macro_{series.lower()}0000")
        record_macro_release(conn, event_id=event_id, series=series, release_at=RELEASE_AT,
                             unit="pp")
    body = client.get("/macro", params={"series": "NFP"}).json()
    assert [m["series"] for m in body["macro"]] == ["NFP"]


def test_macro_respects_the_parent_events_as_of(conn, client):
    """A macro row is only as visible as the event it hangs off — both halves of the predicate."""
    event_id = insert_event(conn, published_at="2026-08-12T12:30:00Z",
                            first_seen_at="2026-08-20T00:00:00Z")  # backfilled
    record_macro_release(conn, event_id=event_id, series="CPI", release_at=RELEASE_AT, unit="pp")
    hidden = client.get("/macro", params={"as_of": "2026-08-15T00:00:00Z"}).json()
    assert hidden["macro"] == []
    shown = client.get("/macro", params={"as_of": "2026-08-25T00:00:00Z"}).json()
    assert len(shown["macro"]) == 1


def test_macro_on_an_empty_store_is_not_a_500(client):
    body = client.get("/macro").json()
    assert body["macro"] == []


# ---- GET /calendar -------------------------------------------------------------------------------


def test_calendar_serves_macro_releases_from_the_store(conn, client):
    event_id = insert_event(conn)
    record_macro_release(conn, event_id=event_id, series="CPI", release_at=RELEASE_AT, unit="pp")
    body = client.get(
        "/calendar", params={"from": "2026-08-01T00:00:00Z", "to": "2026-08-31T00:00:00Z"}
    ).json()
    assert [s["series"] for s in body["scheduled"]] == ["CPI"]
    assert body["scheduled"][0]["kind"] == "macro_release"


def test_calendar_includes_scheduled_earnings(conn, client):
    """§9.27's table, populated by Wave 1 Lane 1C. This lane reads it; it does not fetch it."""
    conn.execute(
        "INSERT INTO scheduled_events (id, occurrence_key, kind, ticker, series, scheduled_at, "
        "confirmed, source, first_seen_at) VALUES (?,?,?,?,?,?,?,?,?)",
        ("sch_0000000000000001", "earnings|NVDA|2026Q2", "earnings", "NVDA", None,
         "2026-08-20T20:00:00Z", 1, "alphavantage", "2026-08-01T00:00:00Z"),
    )
    conn.commit()
    body = client.get(
        "/calendar", params={"from": "2026-08-01T00:00:00Z", "to": "2026-08-31T00:00:00Z"}
    ).json()
    assert [s["ticker"] for s in body["scheduled"]] == ["NVDA"]
    assert body["scheduled"][0]["kind"] == "earnings"


def test_calendar_returns_catalyst_lifecycle_provenance_and_values(conn, client):
    conn.execute(
        "INSERT INTO scheduled_events (id, occurrence_key, kind, ticker, series, scheduled_at, confirmed, source, "
        "first_seen_at, title, description, importance, status, source_tier, source_url, timezone, "
        "local_time, previous, expected, actual, unit, updated_at) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (
                "sch_0000000000000003", "CPI|2026-09", "macro_release", None, "CPI",
            "2026-09-10T12:30:00Z", 1, "fred", "2026-08-01T00:00:00Z",
            "Consumer Price Index (CPI)", "Inflation updates the policy backdrop.", "high",
            "confirmed", "official", "https://fred.test/cpi", "America/New_York", "08:30",
            3.0, 3.1, None, "%", "2026-08-02T00:00:00Z",
        ),
    )
    conn.commit()
    body = client.get(
        "/calendar",
        params={
            "from": "2026-09-01T00:00:00Z", "to": "2026-09-30T00:00:00Z",
            "as_of": "2026-08-25T00:00:00Z",
        },
    ).json()
    event = body["scheduled"][0]
    assert event["title"] == "Consumer Price Index (CPI)"
    assert event["status"] == "confirmed" and event["sourceTier"] == "official"
    assert event["timezone"] == "America/New_York" and event["localTime"] == "08:30"
    assert event["previous"] == 3.0 and event["expected"] == 3.1 and event["actual"] is None


def test_calendar_excludes_entries_captured_after_the_cutoff(conn, client):
    conn.execute(
        "INSERT INTO scheduled_events (id, occurrence_key, kind, ticker, series, scheduled_at, "
        "confirmed, source, first_seen_at) VALUES (?,?,?,?,?,?,?,?,?)",
        ("sch_0000000000000002", "earnings|NVDA|2026Q2", "earnings", "NVDA", None,
         "2026-08-20T20:00:00Z", 1, "alphavantage", "2026-08-18T00:00:00Z"),
    )
    conn.commit()
    params = {"from": "2026-08-01T00:00:00Z", "to": "2026-08-31T00:00:00Z"}
    assert client.get("/calendar", params={**params, "as_of": "2026-08-10T00:00:00Z"}) \
        .json()["scheduled"] == []
    assert len(client.get("/calendar", params={**params, "as_of": "2026-08-25T00:00:00Z"})
               .json()["scheduled"]) == 1


def test_calendar_respects_its_window(conn, client):
    event_id = insert_event(conn)
    record_macro_release(conn, event_id=event_id, series="CPI", release_at=RELEASE_AT, unit="pp")
    body = client.get(
        "/calendar", params={"from": "2026-09-01T00:00:00Z", "to": "2026-09-30T00:00:00Z"}
    ).json()
    assert body["scheduled"] == []


def test_calendar_on_an_empty_store_is_not_a_500(client):
    body = client.get(
        "/calendar", params={"from": "2026-08-01T00:00:00Z", "to": "2026-08-31T00:00:00Z"}
    ).json()
    assert body["scheduled"] == []
    assert body["degraded"] == []


def test_an_unknown_expectation_source_is_refused_by_name(conn):
    event_id = insert_event(conn)
    with pytest.raises(ValueError, match="expectation_source"):
        record_macro_release(conn, event_id=event_id, series="CPI", release_at=RELEASE_AT,
                             unit="pp", expectation_source="a-blog-post")


def test_macro_ids_are_stable_across_rebuilds(tmp_path):
    """The reproducibility guarantee reaches macro rows too."""
    ids = []
    for name in ("rebuild-a", "rebuild-b"):
        c = connect(tmp_path / name)
        migrate(c)
        event_id = insert_event(c)
        ids.append(record_macro_release(
            c, event_id=event_id, series="CPI", release_at=RELEASE_AT, unit="pp"
        ))
        c.close()
    assert ids[0] == ids[1]


# ---- §9.49: deleting an event forgets the link, not the release ----------------------------------


def test_deleting_the_event_nulls_the_link_and_keeps_the_numbers(conn):
    """`ON DELETE SET NULL`, not `CASCADE` (§9.49).

    `actual`/`expected`/`previous`/`surprise` come from FRED and FMP and exist nowhere else in the
    tree. §3.2's retention sweep deletes events, so a cascade here would destroy the only copy of a
    number the release genuinely produced. `event_id` records which event the release was clustered
    INTO; sweeping that event does not un-happen the release.
    """
    event_id = insert_event(conn)
    macro_id = record_macro_release(conn, event_id=event_id, series="CPI", **BASE)
    before = conn.execute(
        "SELECT actual, expected, previous, surprise FROM macro_events WHERE id = ?", (macro_id,)
    ).fetchone()
    assert before["surprise"] == pytest.approx(-0.3)

    conn.execute("DELETE FROM events WHERE id = ?", (event_id,))
    conn.commit()

    row = conn.execute("SELECT * FROM macro_events WHERE id = ?", (macro_id,)).fetchone()
    assert row is not None, "the release row was cascaded away"
    assert row["event_id"] is None
    assert row["actual"] == pytest.approx(before["actual"])
    assert row["expected"] == pytest.approx(before["expected"])
    assert row["surprise"] == pytest.approx(before["surprise"])


def test_postgres_baseline_keeps_the_natural_key_and_timing_check(conn):
    """UNIQUE(series, release_at) and §3.9's post-release-consensus CHECK are enforced."""
    record_macro_release(conn, series="CPI", **BASE)
    record_macro_release(conn, series="CPI", **{**BASE, "actual": 2.9})
    rows = conn.execute(
        "SELECT surprise FROM macro_events WHERE series = ? AND release_at = ?", ("CPI", RELEASE_AT)
    ).fetchall()
    assert len(rows) == 1, "the PostgreSQL upsert forked the release"

    with pytest.raises(IntegrityError):
        conn.execute(
            "INSERT INTO macro_events (id, series, release_at, actual, expected, surprise, unit, "
            "expectation_source, expectation_captured_at) VALUES (?,?,?,?,?,?,?,?,?)",
            ("mac_deadbeefdeadbeef", "PCE", RELEASE_AT, 2.8, 3.1, -0.3, "pp", "fmp", AFTER_RELEASE),
        )
