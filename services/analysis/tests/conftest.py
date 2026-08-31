"""PostgreSQL isolation for the analysis suite.

Set ``ANALYSIS_TEST_DATABASE_URL`` to a disposable PostgreSQL database. Every test receives a
private schema so the full suite can run deterministically without deleting another test's rows.
"""
from __future__ import annotations

import hashlib
import os

import psycopg
import pytest
from psycopg import sql

import app.store as store


@pytest.fixture(autouse=True)
def isolated_bar_schema(request):
    url = os.getenv("ANALYSIS_TEST_DATABASE_URL", "").strip()
    if not url:
        yield
        return

    schema = "analysis_test_" + hashlib.sha256(request.node.nodeid.encode()).hexdigest()[:20]
    previous_url = os.environ.get("BARS_DATABASE_URL")
    previous_schema = os.environ.get("BARS_DATABASE_SCHEMA")
    # Assign directly instead of through monkeypatch: a few golden tests intentionally call
    # monkeypatch.undo(), but they must not escape their isolated database schema when they do.
    os.environ["BARS_DATABASE_URL"] = url
    os.environ["BARS_DATABASE_SCHEMA"] = schema
    store.close()

    try:
        yield
    finally:
        store.close()
        with psycopg.connect(url, autocommit=True) as conn:
            conn.execute(sql.SQL("DROP SCHEMA IF EXISTS {} CASCADE").format(sql.Identifier(schema)))
        if previous_url is None:
            os.environ.pop("BARS_DATABASE_URL", None)
        else:
            os.environ["BARS_DATABASE_URL"] = previous_url
        if previous_schema is None:
            os.environ.pop("BARS_DATABASE_SCHEMA", None)
        else:
            os.environ["BARS_DATABASE_SCHEMA"] = previous_schema
