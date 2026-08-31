"""PostgreSQL test isolation.

Set EVENTS_TEST_DATABASE_URL to a disposable PostgreSQL database. Each test receives its own schema;
the schema and any labelled child schemas are dropped afterwards.
"""
from __future__ import annotations

import os
import sys
import uuid

import psycopg
import pytest
from psycopg import sql


@pytest.fixture(autouse=True)
def isolated_postgres_schema(monkeypatch):
    url = os.getenv("EVENTS_TEST_DATABASE_URL", "").strip()
    if not url:
        # Pure unit tests can still run. Tests that open the store fail with db.py's explicit
        # configuration error, which names the missing variable.
        yield
        return

    prefix = f"test_{uuid.uuid4().hex[:12]}_"
    schema = prefix + "main"
    with psycopg.connect(url, autocommit=True) as admin:
        admin.execute(sql.SQL("CREATE SCHEMA {}").format(sql.Identifier(schema)))

    monkeypatch.setenv("EVENTS_DATABASE_URL", url)
    monkeypatch.setenv("EVENTS_DATABASE_SCHEMA", schema)
    monkeypatch.setenv("EVENTS_TEST_SCHEMA_PREFIX", prefix)

    # app.main caches successful schema initialization for production request efficiency. Tests
    # deliberately switch to a new isolated schema for every case, so a cached success from the
    # prior case cannot describe the current schema and must not suppress its migration.
    main_module = sys.modules.get("app.main")
    if main_module is not None:
        monkeypatch.setattr(main_module, "_SCHEMA_READY", False)
        monkeypatch.setattr(main_module, "_SCHEMA_ERROR", None)

    try:
        yield
    finally:
        with psycopg.connect(url, autocommit=True) as admin:
            rows = admin.execute(
                "SELECT nspname FROM pg_namespace WHERE nspname LIKE %s", (prefix + "%",)
            ).fetchall()
            for (name,) in rows:
                admin.execute(sql.SQL("DROP SCHEMA {} CASCADE").format(sql.Identifier(name)))
