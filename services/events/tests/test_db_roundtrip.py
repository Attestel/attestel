"""PostgreSQL round-trip tests for the events service.

Everything here runs in an isolated PostgreSQL schema with no provider keys or model calls. The constraints being
exercised are the ones the contract calls "enforced, not advised" — content-hash dedup, the dual
as-of filter, the backfill guard, and the macro expectation-timing rule. They are in SQL rather than
in application code precisely so a later lane cannot forget them.
"""
from __future__ import annotations

import pytest

from app import budget
from app.db import IntegrityError, applied_versions, connect, migrate

# ---- fixtures ------------------------------------------------------------------------------------


@pytest.fixture()
def conn():
    """A migrated database in the test's isolated PostgreSQL schema."""
    c = connect()
    migrate(c)
    yield c
    c.close()


CUTOFF = "2026-08-15T00:00:00Z"


def insert_document(conn, doc_id, *, published_at, first_seen_at, content_hash, tier="professional"):
    conn.execute(
        "INSERT INTO source_documents (id, content_hash, provider, source_tier, url, title, "
        "excerpt, published_at, first_seen_at, retrieved_at, ingest_run_id) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?)",
        (
            doc_id,
            content_hash,
            "marketaux",
            tier,
            f"https://example.test/{doc_id}",
            f"headline {doc_id}",
            "bounded excerpt",
            published_at,
            first_seen_at,
            first_seen_at,
            "run_0123456789abcdef",
        ),
    )
    conn.commit()


def insert_event(conn, event_id="evt_1a2b3c4d5e6f7081", **overrides):
    row = {
        "id": event_id,
        "event_type": "regulatory_action",
        "title": "New export restrictions announced",
        "summary": "…",
        "occurred_at": "2026-08-14T13:17:00Z",
        "published_at": "2026-08-14T13:21:00Z",
        "first_seen_at": "2026-08-14T13:24:11Z",
        "source_tier": "official",
        "official_confirmed": 1,
        "importance": 0.87,
        "novelty": 0.91,
        "document_count": 2,
        "cluster_key": "abc123def456789a",
    }
    row.update(overrides)
    cols = ", ".join(row)
    marks = ", ".join("?" * len(row))
    conn.execute(f"INSERT INTO events ({cols}) VALUES ({marks})", tuple(row.values()))
    conn.commit()
    return event_id


# ---- migrations ----------------------------------------------------------------------------------

# Every migration, in order. Named once: five assertions used to hardcode this list, so adding
# 0003 broke all five at the same time for the same uninteresting reason.
ALL_MIGRATIONS = ["0001_postgres_baseline", "0002_automation",
                  "0003_scheduled_event_documents", "0004_event_relationships",
                  "0005_event_reactions", "0006_discovery_scout",
                  "0007_opportunity_radar", "0008_event_data_repairs"]



EXPECTED_TABLES = {
    "source_documents",
    "ingest_runs",
    "events",
    "event_state_history",
    "event_documents",
    "event_tickers",
    "macro_events",
    "provider_budget",
    "scheduled_events",
    "scheduled_event_history",
    "predictions",
    "prediction_evidence",
    "outcomes",
    # Phase 1 — the automation lane lease and its durable run ledger.
    "automation_lanes",
    "automation_runs",
    # Phase 2D — the filing a derived Calendar date was read out of.
    "scheduled_event_documents",
    # Phase 3 — which companies an event bears on, and why.
    "event_relationships",
    # Phase 4 — what the market did afterwards, and whether the window has matured.
    "event_reactions",
    "event_reaction_windows",
    # Discovery Scout — immutable company-level research-lead runs and their ranked candidates.
    "scout_runs",
    "scout_candidates",
    # Early Opportunity Radar — immutable completed-bar setup state.
    "opportunity_runs",
    "opportunity_candidates",
    # Versioned deterministic repairs over already-ingested canonical data.
    "event_data_repairs",
}


def test_migrate_creates_every_contract_table(conn):
    names = {
        r["name"]
        for r in conn.execute(
            "SELECT table_name AS name FROM information_schema.tables "
            "WHERE table_schema = current_schema()"
        ).fetchall()
    }
    assert EXPECTED_TABLES <= names
    assert applied_versions(conn) == ALL_MIGRATIONS
    # bars belongs to the analysis service's private PostgreSQL schema (§4.1), not here.
    assert "bars" not in names


def test_migrate_twice_is_a_noop(conn):
    before = conn.execute("SELECT version, applied_at FROM schema_migrations").fetchall()
    migrate(conn)
    migrate(conn)
    after = conn.execute("SELECT version, applied_at FROM schema_migrations").fetchall()
    assert [tuple(r.values()) for r in before] == [tuple(r.values()) for r in after]
    assert applied_versions(conn) == ALL_MIGRATIONS


def test_postgres_baseline_uses_native_timestamp_and_json_types(conn):
    columns = {
        (row["table_name"], row["column_name"]): row["data_type"]
        for row in conn.execute(
            "SELECT table_name, column_name, data_type FROM information_schema.columns "
            "WHERE table_schema = current_schema()"
        ).fetchall()
    }
    assert columns[("source_documents", "published_at")] == "timestamp with time zone"
    assert columns[("source_documents", "raw_tickers")] == "jsonb"
    assert columns[("provider_budget", "date")] == "date"


# ---- round trip ----------------------------------------------------------------------------------


def test_event_round_trip_with_tickers_and_macro(conn):
    insert_document(conn, "doc_aaaaaaaaaaaaaaaa", published_at="2026-08-14T13:21:00Z",
                    first_seen_at="2026-08-14T13:24:11Z", content_hash="hash-a", tier="official")
    event_id = insert_event(conn)
    conn.execute(
        "INSERT INTO event_documents (event_id, document_id, url, provider, source_tier, "
        "published_at) VALUES (?,?,?,?,?,?)",
        (event_id, "doc_aaaaaaaaaaaaaaaa", "https://example.test/a", "sec-edgar", "official",
         "2026-08-14T13:21:00Z"),
    )
    for ticker, relevance, primary in (("NVDA", 0.94, 1), ("AMD", 0.62, 0)):
        conn.execute(
            "INSERT INTO event_tickers (event_id, ticker, relevance, is_primary, "
            "potential_direction, potential_strength, expected_horizon, affected_dimensions) "
            "VALUES (?,?,?,?,?,?,?,?)",
            (event_id, ticker, relevance, primary, "negative", 0.8, "medium_term",
             '["revenue_exposure"]'),
        )
    conn.execute(
        "INSERT INTO macro_events (id, event_id, series, release_at, actual, expected, surprise, "
        "unit, expectation_source, expectation_captured_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
        ("mac_bbbbbbbbbbbbbbbb", event_id, "CPI", "2026-08-14T12:30:00Z", 3.1, 2.9,
         0.2, "pct", "fmp", "2026-08-13T12:00:00Z"),
    )
    conn.commit()

    rows = conn.execute(
        "SELECT e.id, e.title, t.ticker, t.is_primary FROM events e "
        "JOIN event_tickers t ON t.event_id = e.id ORDER BY t.is_primary DESC, t.ticker"
    ).fetchall()
    assert [(r["ticker"], r["is_primary"]) for r in rows] == [("NVDA", 1), ("AMD", 0)]
    assert rows[0]["title"] == "New export restrictions announced"

    # The event's attribution survives independently of the document row (§3.7).
    link = conn.execute("SELECT url, provider FROM event_documents").fetchone()
    assert link["provider"] == "sec-edgar"


def test_duplicate_content_hash_is_rejected(conn):
    insert_document(conn, "doc_1111111111111111", published_at="2026-08-01T00:00:00Z",
                    first_seen_at="2026-08-01T00:00:00Z", content_hash="same-hash")
    with pytest.raises(IntegrityError):
        insert_document(conn, "doc_2222222222222222", published_at="2026-08-02T00:00:00Z",
                        first_seen_at="2026-08-02T00:00:00Z", content_hash="same-hash")


# ---- the as-of rule (§1.1) -----------------------------------------------------------------------

AS_OF_QUERY = (
    "SELECT id FROM source_documents "
    "WHERE published_at <= :as_of AND first_seen_at <= :as_of ORDER BY id"
)


def test_as_of_excludes_documents_published_after_the_cutoff(conn):
    insert_document(conn, "doc_visible00000000", published_at="2026-08-10T00:00:00Z",
                    first_seen_at="2026-08-10T00:00:00Z", content_hash="h-visible")
    insert_document(conn, "doc_future000000000", published_at="2026-08-20T00:00:00Z",
                    first_seen_at="2026-08-20T00:00:00Z", content_hash="h-future")
    got = [r["id"] for r in conn.execute(AS_OF_QUERY, {"as_of": CUTOFF}).fetchall()]
    assert got == ["doc_visible00000000"]


def test_as_of_excludes_a_backfilled_document(conn):
    """The backfill guard (§1.2): an old article FIRST SEEN after the cutoff was not visible then.

    published_at alone would let it through, which is exactly the leak first_seen_at exists to stop.
    """
    insert_document(conn, "doc_visible00000000", published_at="2026-08-10T00:00:00Z",
                    first_seen_at="2026-08-10T00:00:00Z", content_hash="h-visible")
    insert_document(conn, "doc_backfilled00000", published_at="2026-07-01T00:00:00Z",
                    first_seen_at="2026-08-16T09:00:00Z", content_hash="h-backfill")
    got = [r["id"] for r in conn.execute(AS_OF_QUERY, {"as_of": CUTOFF}).fetchall()]
    assert got == ["doc_visible00000000"]
    # published_at alone would wrongly include it — this is the assertion that pins the rule.
    lax = [
        r["id"]
        for r in conn.execute(
            "SELECT id FROM source_documents WHERE published_at <= :as_of ORDER BY id",
            {"as_of": CUTOFF},
        ).fetchall()
    ]
    assert "doc_backfilled00000" in lax


# ---- enforced-not-advised constraints ------------------------------------------------------------


def test_macro_consensus_captured_after_release_cannot_carry_a_surprise(conn):
    event_id = insert_event(conn, event_id="evt_cccccccccccccccc")
    with pytest.raises(IntegrityError):
        conn.execute(
            "INSERT INTO macro_events (id, event_id, series, release_at, actual, expected, "
            "surprise, expectation_source, expectation_captured_at) VALUES (?,?,?,?,?,?,?,?,?)",
            ("mac_dddddddddddddddd", event_id, "CPI", "2026-08-14T12:30:00Z", 3.1, 2.9, 0.2,
             "fmp", "2026-08-14T13:00:00Z"),  # captured AFTER the release
        )
        conn.commit()


def test_body_is_rejected_for_a_non_official_tier(conn):
    """D-29 enforced in SQL: only `official` sources may store a full body."""
    with pytest.raises(IntegrityError):
        conn.execute(
            "INSERT INTO source_documents (id, content_hash, provider, source_tier, url, title, "
            "body, published_at, first_seen_at, retrieved_at, ingest_run_id) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?)",
            ("doc_3333333333333333", "h-body", "marketaux", "professional",
             "https://example.test/x", "t", "full article text", "2026-08-01T00:00:00Z",
             "2026-08-01T00:00:00Z", "2026-08-01T00:00:00Z", "run_0123456789abcdef"),
        )
        conn.commit()


def test_cluster_key_index_is_not_unique(conn):
    """§9.7: two events may legitimately share a cluster_key. A UNIQUE index would break §3.5."""
    insert_event(conn, event_id="evt_aaaaaaaaaaaaaaa1", cluster_key="shared-key-000000")
    insert_event(conn, event_id="evt_aaaaaaaaaaaaaaa2", cluster_key="shared-key-000000")
    count = conn.execute(
        "SELECT COUNT(*) AS n FROM events WHERE cluster_key = 'shared-key-000000'"
    ).fetchone()["n"]
    assert count == 2


# ---- /health (§3.11) -----------------------------------------------------------------------------


def test_health_reports_the_contract_shape():
    from fastapi.testclient import TestClient

    from app.main import app

    with TestClient(app) as client:
        body = client.get("/health").json()

    assert body["status"] == "ok"
    assert body["service"] == "events"
    assert body["storage"] == "postgresql"
    assert body["schema"].startswith("test_")
    assert body["migrations"] == ALL_MIGRATIONS
    assert body["dataRepairs"] == [{
        "version": "marketaux-headline-subject@1",
        "appliedAt": body["dataRepairs"][0]["appliedAt"],
        "details": {
            "eventsRepaired": 0,
            "eventsScanned": 0,
            "eventsSkippedMissingSources": 0,
            "linksRemoved": 0,
            "primariesReassigned": 0,
        },
    }]
    assert "/" in body["db"] and "@" not in body["db"]

    # `providers` is wired from budget.state() (Wave 1 integration). It reports the FULL cascade,
    # including providers that have never been called, so an operator can tell "configured and idle"
    # from "not in the build" — and so the gate can judge whether a provider ran from PROVENANCE
    # rather than from a log grep (§9.44).
    providers = {p["provider"]: p for p in body["providers"]}
    assert set(providers) == set(budget.DAILY_LIMITS), (
        "/health must list every provider in the cascade, not only the ones that have run")
    for name, entry in providers.items():
        assert set(entry) == {
            "provider", "date", "calls", "limit", "remaining", "utilizationPct",
            "exhausted", "warning", "status", "blockedUntil", "blocked", "lastError",
        }
        # A service that has never fetched reports zero calls — this is the empty-.env baseline the
        # gate compares against, and it must be zero rather than absent.
        assert entry["calls"] == 0, f"{name} reports {entry['calls']} calls on a fresh database"
        assert entry["blocked"] is False
        assert entry["lastError"] is None
    assert "providersError" not in body, body.get("providersError")


def test_the_route_table_is_additive():
    """Wave 0 shipped /health alone and an inert OPTIONAL_ROUTERS seam.

    Wave 2 Lane 2A fills that seam via §9.28's `router` auto-discovery, so the assertion changes
    from "nothing but /health" to "/health, unchanged, plus exactly the routes this lane owns".
    A route appearing here that no lane claims is the regression this test now catches.

    Wave 1 Lane 1C adds `/documents`, `/ingest` and `/ingest/runs` (contract §3.11) through the
    same mechanism and hands the integrator no `include_router` line, per §9.28. Every route below
    is claimed by a lane; the list is the register, and a lane that fills the seam updates it.
    """
    from app.main import app

    assert sorted(app.openapi()["paths"].keys()) == [
        "/automation/lease",      # Phase 1
        "/automation/runs",       # Phase 1
        "/automation/runs/{run_id}/complete",  # Phase 1
        "/automation/status",     # Phase 1
        "/calendar",              # 2A
        "/documents",             # 1C
        "/events",                # 2A
        "/events/{event_id}",     # 2A
        "/events/{event_id}/enrichment",  # 2A
        "/health",                # Wave 0, unchanged
        "/ingest",                # 1C
        "/ingest/runs",           # 1C
        "/ir/coverage",           # Phase 2C
        "/macro",                 # 2A
        "/opportunities",         # Early Opportunity Radar
        "/outcomes/backfill",     # 3C
        "/predictions",           # 3C
        "/reactions",             # Phase 4
        "/relationships",         # Phase 3
        "/relationships/reference",  # Phase 3
        "/scout",                # Discovery Scout
        "/sensitivity",           # Phase 4
    ]


# =================================================================================================
# PostgreSQL baseline ownership and indexes (§9.41)
# =================================================================================================


def test_macro_event_owner_is_nullable(conn):
    column = conn.execute(
        "SELECT is_nullable FROM information_schema.columns "
        "WHERE table_schema = current_schema() AND table_name = 'macro_events' "
        "AND column_name = 'event_id'"
    ).fetchone()
    assert column["is_nullable"] == "YES"


def test_a_macro_row_can_be_written_with_a_null_event_id(conn):
    """§9.41.1: ingestion may create the release before assembly links an event."""
    conn.execute(
        "INSERT INTO macro_events (id, event_id, series, release_at) VALUES (?,?,?,?)",
        ("mac_00000000000000aa", None, "CPI", "2026-09-10T12:30:00Z"),
    )
    conn.commit()
    row = conn.execute("SELECT * FROM macro_events").fetchone()
    assert row["event_id"] is None


def test_the_natural_key_is_unique(conn):
    """§9.41.2. Two providers reporting the same release converge on one row."""
    conn.execute(
        "INSERT INTO macro_events (id, series, release_at) VALUES (?,?,?)",
        ("mac_00000000000000ab", "CPI", "2026-09-10T12:30:00Z"),
    )
    conn.commit()
    with pytest.raises(IntegrityError):
        conn.execute(
            "INSERT INTO macro_events (id, series, release_at) VALUES (?,?,?)",
            ("mac_00000000000000ac", "CPI", "2026-09-10T12:30:00Z"),
        )


def test_postgres_baseline_keeps_the_expectation_timing_check(conn):
    with pytest.raises(IntegrityError):
        conn.execute(
            "INSERT INTO macro_events (id, series, release_at, actual, expected, surprise, "
            "expectation_source, expectation_captured_at) VALUES (?,?,?,?,?,?,?,?)",
            ("mac_00000000000000ad", "CPI", "2026-09-10T12:30:00Z", 3.4, 3.2, 0.2, "fmp",
             "2026-09-10T13:00:00Z"),  # captured AFTER the release
        )


def test_postgres_baseline_indexes_macro_key(conn):
    column = conn.execute(
        "SELECT is_nullable, column_default FROM information_schema.columns "
        "WHERE table_schema = current_schema() AND table_name = 'source_documents' "
        "AND column_name = 'macro_key'"
    ).fetchone()
    assert column["is_nullable"] == "YES"
    assert column["column_default"] is None
    indexes = {
        row["indexname"]
        for row in conn.execute(
            "SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() "
            "AND tablename = 'source_documents'"
        ).fetchall()
    }
    assert "idx_source_documents_macro_key" in indexes


# =================================================================================================
# §9.54: `failed` is removed from enrichment_state
# =================================================================================================


def test_enrichment_state_failed_is_rejected_not_answered_empty():
    """§9.54 binding 2. `failed` was accepted and always matched zero rows, because nothing can
    write it: put_enrichment hardcodes 'enriched' and is the only writer. An empty 200 reads as a
    working queue selector that found nothing; a 400 says the value does not exist.
    """
    from fastapi.testclient import TestClient

    from app.main import app

    with TestClient(app) as client:
        r = client.get("/events", params={"enrichmentState": "failed"})
        assert r.status_code == 400, r.text
        assert "raw or enriched" in r.json()["detail"]

        # the two real values still serve, so this is a rejection of one value, not a broken filter
        for good in ("raw", "enriched"):
            assert client.get(
                "/events", params={"enrichmentState": good}
            ).status_code == 200, good
