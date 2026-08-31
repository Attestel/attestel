"""PostgreSQL connection and migration runner for the global event corpus.

The events service is the only database owner. Callers keep using the small DB-API surface that
the service already relied on (``execute``, ``commit``, ``rollback`` and ``close``); this module
adapts the legacy qmark/named placeholders to psycopg so the storage cutover does not force an
unrelated rewrite of every query in the domain modules.
"""
from __future__ import annotations

import hashlib
import json
import os
import re
import time
from contextlib import contextmanager
from datetime import date, datetime, timezone
from pathlib import Path
from urllib.parse import urlsplit

import psycopg
from psycopg import sql

from .config import (
    EVENTS_DATABASE_URL,
    EVENTS_DB_CONNECT_TIMEOUT,
    EVENTS_DB_STATEMENT_TIMEOUT_MS,
)

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"
DatabaseError = psycopg.Error
IntegrityError = psycopg.IntegrityError

_NAMED_PLACEHOLDER = re.compile(r"(?<!:):([A-Za-z_][A-Za-z0-9_]*)")
_TEST_DATABASE_URL_ENV = "EVENTS_TEST_DATABASE_URL"
_SCHEMA_ENV = "EVENTS_DATABASE_SCHEMA"


class Row(dict):
    """Mapping row with optional integer indexing for the service's existing query consumers."""

    def __getitem__(self, key):
        if isinstance(key, int):
            return tuple(self.values())[key]
        return super().__getitem__(key)


def _normalise_value(value):
    """Keep the existing service boundary stable while PostgreSQL stores native values."""
    if isinstance(value, datetime):
        moment = value if value.tzinfo else value.replace(tzinfo=timezone.utc)
        return moment.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    if isinstance(value, date):
        return value.isoformat()
    if isinstance(value, (dict, list)):
        return json.dumps(value, separators=(",", ":"), sort_keys=True)
    return value


def _row_factory(cursor):
    columns = [column.name for column in cursor.description] if cursor.description else []

    def make_row(values) -> Row:
        return Row((name, _normalise_value(value)) for name, value in zip(columns, values))

    return make_row


def _postgres_query(query: str, params) -> str:
    if params is None:
        return query
    if isinstance(params, dict):
        return _NAMED_PLACEHOLDER.sub(r"%(\1)s", query)
    return query.replace("?", "%s")


class Connection:
    """Narrow psycopg connection facade used by the service and its tests."""

    def __init__(self, raw: psycopg.Connection):
        self._raw = raw

    def execute(self, query: str, params=None):
        return self._raw.execute(_postgres_query(query, params), params)

    def commit(self) -> None:
        self._raw.commit()

    def rollback(self) -> None:
        self._raw.rollback()

    def close(self) -> None:
        self._raw.close()

    @contextmanager
    def transaction(self):
        with self._raw.transaction():
            yield self


def _utc_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def database_url() -> str:
    """Return the required PostgreSQL URL, read at call time for test/deployment overrides."""
    value = (
        os.getenv("EVENTS_DATABASE_URL", "").strip()
        or os.getenv("DATABASE_URL", "").strip()
        or EVENTS_DATABASE_URL
    )
    if not value:
        raise RuntimeError(
            "DATABASE_URL (or EVENTS_DATABASE_URL) is required: "
            "the events service is PostgreSQL-only"
        )
    if not value.startswith(("postgresql://", "postgres://")):
        raise RuntimeError("DATABASE_URL must use postgresql:// or postgres://")
    return value


def database_target() -> str:
    """Credential-free database identity for health/diagnostics."""
    parsed = urlsplit(database_url())
    host = parsed.hostname or "unknown"
    port = parsed.port or 5432
    name = parsed.path.lstrip("/") or "unknown"
    return f"{host}:{port}/{name}"


def database_schema() -> str:
    return os.getenv(_SCHEMA_ENV, "public").strip() or "public"


def _test_schema(label: str | Path) -> str:
    digest = hashlib.sha256(str(label).encode("utf-8")).hexdigest()[:20]
    prefix = os.getenv("EVENTS_TEST_SCHEMA_PREFIX", "test_").strip() or "test_"
    return f"{prefix}{digest}"


# The shared PostgreSQL this service points at (the platform's managed addon cluster) has been
# observed cycling: "server closed the connection unexpectedly" and "Connection refused" seconds
# apart, for windows that last a few seconds. During such a window the FIRST connect attempt fails
# fast, and a request that dies on it becomes a user-visible 500 for an outage that would have been
# over before the user could retry. So a failed connect is retried on a short, bounded schedule.
#
# Bounded is the contract: both fault shapes fail in milliseconds, so the worst case adds
# _FLAP_RETRY_DELAYS_TOTAL (~1s) of sleep — small enough that /health, which also connects, stays
# comfortably answerable. A database that is DOWN rather than flapping still fails, just ~1s later,
# and the degraded-read path upstream is unchanged. This is a mitigation for someone else's
# instability, not the fix; the fix is the platform's.
_FLAP_RETRY_DELAYS = (0.3, 0.7)


def _connect_retries() -> int:
    try:
        n = int(os.getenv("EVENTS_DB_CONNECT_RETRIES", "").strip() or len(_FLAP_RETRY_DELAYS))
    except ValueError:
        n = len(_FLAP_RETRY_DELAYS)
    return max(0, min(n, len(_FLAP_RETRY_DELAYS)))


def _connect_with_flap_retry(url: str) -> psycopg.Connection:
    retries = _connect_retries()
    for attempt in range(retries + 1):
        try:
            return psycopg.connect(
                url,
                connect_timeout=EVENTS_DB_CONNECT_TIMEOUT,
                row_factory=_row_factory,
            )
        except psycopg.OperationalError:
            if attempt >= retries:
                raise
            time.sleep(_FLAP_RETRY_DELAYS[attempt])
    raise AssertionError("unreachable")  # pragma: no cover


def connect(test_label: str | Path | None = None) -> Connection:
    """Open PostgreSQL and bind the connection to the configured schema.

    ``test_label`` is accepted only with EVENTS_TEST_DATABASE_URL and gives tests an isolated
    schema. Production callers never pass it.
    """
    url = database_url()
    if test_label is not None:
        test_url = os.getenv(_TEST_DATABASE_URL_ENV, "").strip()
        if not test_url:
            raise RuntimeError("test-labelled connections require EVENTS_TEST_DATABASE_URL")
        url = test_url

    raw = _connect_with_flap_retry(url)
    conn = Connection(raw)
    schema = _test_schema(test_label) if test_label is not None else database_schema()
    # PostgreSQL accepts a missing name in search_path and silently falls through to public. Create
    # an explicitly configured schema first so isolation cannot degrade without an error.
    if schema != "public":
        raw.execute(sql.SQL("CREATE SCHEMA IF NOT EXISTS {}").format(sql.Identifier(schema)))
        raw.commit()
    raw.execute(sql.SQL("SET search_path TO {}, public").format(sql.Identifier(schema)))
    raw.execute(
        "SELECT set_config('statement_timeout', %s, false)",
        (str(EVENTS_DB_STATEMENT_TIMEOUT_MS),),
    )
    raw.commit()
    return conn


def _ensure_registry(conn: Connection) -> None:
    conn.execute(
        "CREATE TABLE IF NOT EXISTS schema_migrations ("
        "  version TEXT PRIMARY KEY,"
        "  applied_at TIMESTAMPTZ NOT NULL"
        ")"
    )
    conn.commit()


def applied_versions(conn: Connection) -> list[str]:
    _ensure_registry(conn)
    rows = conn.execute("SELECT version FROM schema_migrations ORDER BY version").fetchall()
    conn.commit()
    return [row["version"] for row in rows]


def migrate(conn: Connection, migrations_dir: Path | None = None) -> list[str]:
    """Apply every pending PostgreSQL migration once, transactionally and under an advisory lock."""
    source = migrations_dir or MIGRATIONS_DIR
    _ensure_registry(conn)
    for path in sorted(source.glob("*.sql")):
        version = path.stem
        with conn.transaction():
            conn.execute(
                "SELECT pg_advisory_xact_lock(hashtext(?))",
                ("attestel.events.schema_migrations",),
            )
            exists = conn.execute(
                "SELECT 1 AS present FROM schema_migrations WHERE version = ?", (version,)
            ).fetchone()
            if exists:
                continue
            conn.execute(path.read_text(encoding="utf-8"))
            conn.execute(
                "INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
                (version, _utc_now()),
            )
    return applied_versions(conn)
