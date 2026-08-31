"""The deterministic clustering key (contract §3.5).

The gates here are the ones Wave 5A's ablation ladder rests on: near-duplicates collapse, genuinely
different stories stay apart, and neither the input order nor the process hash seed can change the
answer.
"""
from __future__ import annotations

import json
import random
import subprocess
import sys
from pathlib import Path

import pytest

from app import events as events_mod
from app.cluster import (
    CLUSTER_JACCARD_MIN,
    CLUSTER_VERSION,
    MINHASH_BAND_ROWS,
    MINHASH_BANDS,
    MINHASH_PERMUTATIONS,
    SHINGLE_K,
    band_digests,
    bands_collide,
    cluster_key,
    date_bucket,
    estimated_jaccard,
    iso,
    minhash_band,
    minhash_signature,
    normalise_ticker_set,
    normalise_title,
    shingles,
)
from app.db import connect, migrate

SERVICE_ROOT = Path(__file__).resolve().parent.parent


@pytest.fixture()
def conn(tmp_path, monkeypatch):
    c = connect()
    migrate(c)
    yield c
    c.close()


# ---- title normalisation -------------------------------------------------------------------------


def test_normalisation_folds_case_punctuation_and_whitespace():
    assert normalise_title("Washington  Announces   NEW Export-Restrictions!") == \
        "washington announces new export restrictions"


def test_normalisation_collapses_dotted_acronyms():
    """"U.S." and "US" are the same token; word shingles would otherwise see two different ones."""
    assert normalise_title("U.S. curbs A.I. chip exports") == "us curbs ai chip exports"
    assert normalise_title("US curbs AI chip exports") == "us curbs ai chip exports"


def test_normalisation_strips_a_known_source_prefix():
    assert normalise_title("Reuters: NVIDIA beats estimates") == "nvidia beats estimates"
    assert normalise_title("Bloomberg | NVIDIA beats estimates") == "nvidia beats estimates"


def test_normalisation_leaves_a_company_prefix_alone():
    """"NVIDIA:" is part of the headline, not a byline. A general strip-before-the-colon rule would
    eat it and make two unrelated NVDA headlines look alike."""
    assert normalise_title("NVIDIA: Q2 results") == "nvidia q2 results"


def test_normalisation_is_nfkc():
    assert normalise_title("ＮＶＩＤＩＡ beats") == "nvidia beats"


# ---- shingles ------------------------------------------------------------------------------------


def test_shingles_are_k_word_grams_sorted_and_unique():
    got = shingles("alpha beta gamma delta epsilon zeta")
    assert got == ("alpha beta gamma delta epsilon", "beta gamma delta epsilon zeta")
    assert list(got) == sorted(got)


def test_a_short_title_yields_one_shingle_rather_than_none():
    assert shingles("nvidia beats") == ("nvidia beats",)
    assert shingles("") == ()


def test_shingle_k_is_five():
    assert SHINGLE_K == 5


# ---- MinHash -------------------------------------------------------------------------------------


def test_signature_and_band_geometry():
    assert MINHASH_PERMUTATIONS == 128
    assert MINHASH_BANDS == 4
    assert MINHASH_BAND_ROWS == 2
    signature = minhash_signature(shingles("a b c d e f g h"))
    assert len(signature) == MINHASH_PERMUTATIONS
    assert len(band_digests(signature)) == MINHASH_BANDS


def test_the_banding_knee_sits_below_the_disposal_threshold():
    """Banding proposes, the threshold disposes — so the band knee must be the LOWER of the two.

    With MINHASH_BAND_ROWS = 32 the knee would be ~0.96 and CLUSTER_JACCARD_MIN could never fire.
    """
    knee = (1.0 / MINHASH_BANDS) ** (1.0 / MINHASH_BAND_ROWS)
    assert knee < CLUSTER_JACCARD_MIN


def test_minhash_band_is_band_zero():
    sh = shingles("us announces new export restrictions on advanced ai accelerators")
    assert minhash_band(sh) == band_digests(minhash_signature(sh))[0]


def test_identical_titles_have_jaccard_one():
    a = minhash_signature(shingles("us announces new export restrictions on ai accelerators"))
    b = minhash_signature(shingles("us announces new export restrictions on ai accelerators"))
    assert estimated_jaccard(a, b) == 1.0
    assert bands_collide(a, b)


def test_unrelated_titles_fall_below_the_threshold():
    a = minhash_signature(shingles("nvidia reports record data centre revenue for the quarter"))
    b = minhash_signature(shingles("tesla opens a new assembly plant in monterrey this year"))
    assert estimated_jaccard(a, b) < CLUSTER_JACCARD_MIN


def test_an_empty_shingle_set_is_similar_to_nothing():
    empty = minhash_signature(())
    assert estimated_jaccard(empty, empty) == 0.0
    assert estimated_jaccard(empty, minhash_signature(shingles("a b c d e f"))) == 0.0


def test_signature_uses_hashlib_not_the_salted_builtin():
    """A signature built with the builtin hash() would differ between two processes.

    services/analysis/app/data.py seeds synthetic bars with `abs(hash((ticker, timeframe)))`; this
    test is the reason that pattern is not copied here.
    """
    script = (
        f"import sys; sys.path.insert(0, {str(SERVICE_ROOT)!r});"
        "from app.cluster import minhash_signature, shingles;"
        "print(minhash_signature(shingles('us announces new export restrictions on ai chips')))"
    )
    outputs = set()
    for seed in ("0", "1", "12345"):
        out = subprocess.run(
            [sys.executable, "-c", script],
            capture_output=True, text=True, check=True,
            env={"PATH": "/usr/bin:/bin", "PYTHONHASHSEED": seed},
        )
        outputs.add(out.stdout)
    assert len(outputs) == 1


# ---- date bucket ---------------------------------------------------------------------------------


def test_date_bucket_is_utc_calendar_day():
    assert date_bucket("2026-08-15T13:17:00Z") == "2026-08-15"
    assert date_bucket("2026-08-15T23:59:59Z") == "2026-08-15"
    assert date_bucket("2026-08-16T00:00:01Z") == "2026-08-16"


def test_an_event_straddling_midnight_utc_produces_two_buckets():
    """Accepted deliberately: a fuzzy window would make membership order-dependent."""
    assert date_bucket("2026-08-15T23:58:00Z") != date_bucket("2026-08-16T00:02:00Z")


def test_iso_is_fixed_width():
    """Lexicographic order is chronological order only while the width is constant, and every
    as_of filter in this service is a string comparison in SQL."""
    assert iso("2026-08-15T13:17:00+00:00") == "2026-08-15T13:17:00Z"
    assert iso("2026-08-15T13:17:00Z") == "2026-08-15T13:17:00Z"
    assert len(iso("2026-08-15T13:17:00Z")) == len(iso("2026-12-01T00:00:00Z"))


# ---- the key itself ------------------------------------------------------------------------------


def test_cluster_key_is_sixteen_lowercase_hex():
    key = cluster_key("NVDA", "regulatory_action", "2026-08-15T13:00:00Z", shingles("a b c d e f"))
    assert len(key) == 16
    assert key == key.lower()
    assert all(c in "0123456789abcdef" for c in key)


def test_cluster_key_changes_with_every_input():
    base = ("NVDA", "regulatory_action", "2026-08-15T13:00:00Z", shingles("a b c d e f g"))
    key = cluster_key(*base)
    assert cluster_key("AMD", *base[1:]) != key
    assert cluster_key(base[0], "supply_chain", *base[2:]) != key
    assert cluster_key(*base[:2], "2026-08-16T13:00:00Z", base[3]) != key
    assert cluster_key(*base[:3], shingles("z y x w v u t")) != key


def test_ticker_set_normalisation_is_order_and_case_independent():
    assert normalise_ticker_set(["nvda", "AMD"]) == normalise_ticker_set(["AMD", " nvda "])
    assert normalise_ticker_set("NVDA") == "NVDA"
    assert normalise_ticker_set([]) == ""


def test_cluster_version_is_frozen():
    assert CLUSTER_VERSION == "cluster@1"


# =================================================================================================
# End-to-end clustering behaviour
# =================================================================================================

# Seven documents about ONE occurrence, as a real feed produces them: a wire story picked up and
# lightly re-edited across outlets. This is what §3.5's 5-shingle MinHash is for — near-duplicate
# detection — and it is what today's gateway `newsFor` cannot do, because it dedupes only by exact
# lowercased headline equality and would return all seven.
ONE_OCCURRENCE = [
    ("doc_export0000000001", "marketaux",
     "US announces new export restrictions on advanced AI accelerators"),
    ("doc_export0000000002", "google-news-rss",
     "Reuters: US announces new export restrictions on advanced AI accelerators"),
    ("doc_export0000000003", "google-news-rss",
     "US announces new export restrictions on advanced AI accelerators, officials say"),
    ("doc_export0000000004", "marketaux",
     "U.S. announces new export restrictions on advanced A.I. accelerators"),
    ("doc_export0000000005", "google-news-rss",
     "Washington: US announces new export restrictions on advanced AI accelerators"),
    ("doc_export0000000006", "alphavantage",
     "US announces new export restrictions on advanced AI accelerators this week"),
    ("doc_export0000000007", "marketaux",
     "US announces new export restrictions on advanced AI accelerators — report"),
]

DIFFERENT_STORIES = [
    ("doc_other00000000001", "marketaux",
     "NVIDIA reports record data centre revenue for the quarter"),
    ("doc_other00000000002", "marketaux",
     "NVIDIA names a new chief financial officer effective next month"),
]


def _insert(conn, doc_id, provider, title, *, published_at="2026-08-15T13:00:00Z",
            tier="professional", tickers=("NVDA",)):
    conn.execute(
        "INSERT INTO source_documents (id, content_hash, provider, source_tier, url, title, "
        "excerpt, published_at, first_seen_at, retrieved_at, raw_tickers, ingest_run_id) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
        (doc_id, f"hash-{doc_id}", provider, tier, f"https://example.test/{doc_id}", title,
         "", published_at, published_at, published_at, json.dumps(list(tickers)),
         "run_0123456789abcdef"),
    )
    conn.commit()


def test_seven_headlines_about_one_occurrence_collapse_to_one_event(conn):
    for doc_id, provider, title in ONE_OCCURRENCE:
        # This is a market-wide export-rule occurrence. No headline names a covered company, so
        # provider tickers would be an unrelated test assumption under the headline-subject rule.
        _insert(conn, doc_id, provider, title, tickers=())
    events_mod.assemble(conn)

    rows = conn.execute("SELECT id, document_count FROM events").fetchall()
    assert len(rows) == 1, "seven near-duplicate headlines must be ONE event"
    assert rows[0]["document_count"] == 7
    assert conn.execute("SELECT count(*) AS n FROM event_tickers").fetchone()["n"] == 0


def test_two_different_same_day_nvda_stories_stay_separate(conn):
    for doc_id, provider, title in DIFFERENT_STORIES:
        _insert(conn, doc_id, provider, title)
    events_mod.assemble(conn)
    assert conn.execute("SELECT COUNT(*) AS n FROM events").fetchone()["n"] == 2


def _dump(conn):
    """Every row this lane writes, in primary-key order, as comparable text."""
    out = {}
    for table, order in (
        ("events", "id"),
        ("event_documents", "event_id, document_id"),
        ("event_tickers", "event_id, ticker"),
        ("event_state_history", "event_id, as_of"),
    ):
        rows = conn.execute(f"SELECT * FROM {table} ORDER BY {order}").fetchall()
        out[table] = [[repr(v) for v in tuple(r)] for r in rows]
    return json.dumps(out, sort_keys=True)


def _build(db_path, documents):
    conn = connect(db_path)
    migrate(conn)
    for doc_id, provider, title in documents:
        _insert(conn, doc_id, provider, title)
    events_mod.assemble(conn)
    dump = _dump(conn)
    conn.close()
    return dump


def test_shuffled_input_yields_identical_events(tmp_path):
    """Processing the same documents in a different order must produce the same events."""
    documents = ONE_OCCURRENCE + DIFFERENT_STORIES
    ordered = _build(tmp_path / "ordered", documents)

    shuffled = list(documents)
    random.Random(20260817).shuffle(shuffled)
    assert shuffled != documents
    assert _build(tmp_path / "shuffled", shuffled) == ordered


def test_running_the_clusterer_twice_produces_identical_rows(tmp_path):
    documents = ONE_OCCURRENCE + DIFFERENT_STORIES
    assert _build(tmp_path / "one", documents) == _build(tmp_path / "two", documents)


def test_cluster_keys_are_stable_across_runs(tmp_path):
    documents = ONE_OCCURRENCE + DIFFERENT_STORIES
    keys = []
    for name in ("a", "b"):
        conn = connect(tmp_path / name)
        migrate(conn)
        for doc_id, provider, title in documents:
            _insert(conn, doc_id, provider, title)
        events_mod.assemble(conn)
        keys.append([r["cluster_key"] for r in
                     conn.execute("SELECT cluster_key FROM events ORDER BY id").fetchall()])
        conn.close()
    assert keys[0] == keys[1]
