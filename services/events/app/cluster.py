"""Deterministic near-duplicate clustering (contract §3.5). No model, ever.

The LLM never decides cluster membership; it enriches an already-formed cluster. Everything here is
code so that two runs over the same documents produce the same events — Wave 5A's ablation ladder
compares rungs that share these clusters, and a clustering result that drifts between runs silently
invalidates every benchmark number the programme produces.

    cluster_key = sha256(
        normalised_primary_ticker_set + event_type_guess + date_bucket(occurred_at, 1 day)
        + minhash_band(title_shingles, k=5, bands=4)
    )[:16]

Every hash below comes from `hashlib`. The builtin `hash()` over `str` is salted per process by
PYTHONHASHSEED, so a signature built with it would differ between two runs of the same code —
`tests/test_events.py::test_two_runs_identical` runs under two different seeds to catch exactly
that.
"""
from __future__ import annotations

import hashlib
import re
import unicodedata
from collections import Counter
from datetime import date, datetime, timezone

# Bumping this re-buckets the whole corpus. Every constant in this module, plus
# `entities.guess_event_type`'s rule tables, is covered by it.
CLUSTER_VERSION = "cluster@1"

SHINGLE_K = 5
MINHASH_PERMUTATIONS = 128
MINHASH_BANDS = 4

# Banding proposes, the threshold disposes: a band collision makes a document a CANDIDATE, and only
# an estimated Jaccard at or above this value actually joins it. Named here rather than written at
# the call site so changing it is a versioned decision.
CLUSTER_JACCARD_MIN = 0.55

# Rows per band, and the reason this is 2 rather than MINHASH_PERMUTATIONS // MINHASH_BANDS.
#
# An LSH band of r rows over b bands makes a pair a candidate with probability 1 − (1 − s^r)^b, an
# S-curve whose knee sits at roughly (1/b)^(1/r). With b = 4 the knee is:
#
#     r = 32  ->  0.96      r = 4  ->  0.71      r = 2  ->  0.50      r = 1  ->  0.25
#
# Splitting all 128 permutations across 4 bands gives r = 32 and a knee at ~0.96, which means
# banding alone rejects everything below near-identity and CLUSTER_JACCARD_MIN never binds — a
# constant that can never fire is not a threshold, it is a comment. Worse, it collapses this whole
# module to "cluster titles that are the same after normalisation", which is roughly what
# `gateway/enrich.go::newsFor`'s exact-lowercased-headline dedupe already does and what §3.5 exists
# to replace: seven outlets running one wire story would stay seven events.
#
# So the knee is placed just BELOW the disposal threshold (0.50 against 0.55): banding admits a
# little more than the threshold keeps, the threshold makes the decision, and both constants do the
# job they are named for. The full 128-permutation signature is still what `estimated_jaccard`
# measures — only the candidate index is narrowed — so joining stays as accurate as the contract's
# arithmetic allows.
MINHASH_BAND_ROWS = 2

_MAX_HASH = 2**64 - 1

# A leading source prefix is stripped only when it names a KNOWN publisher. A general
# "everything before the first colon" rule would eat "NVIDIA: Q2 results" — the company name is
# part of the headline, not a byline — and would make normalisation depend on punctuation luck.
KNOWN_SOURCE_PREFIXES: frozenset[str] = frozenset({
    "ap", "associated press", "barron's", "barrons", "bloomberg", "business wire", "businesswire",
    "cnbc", "financial times", "ft", "globe newswire", "globenewswire", "marketwatch",
    "pr newswire", "prnewswire", "reuters", "sec edgar", "seeking alpha", "techcrunch",
    "the information", "the verge", "the wall street journal", "wsj", "yahoo finance",
})

_PREFIX_RE = re.compile(r"^\s*([^:|]{2,30}?)\s*(?::|\s\|\s)\s*")
_PUNCT_RE = re.compile(r"[^a-z0-9 ]+")
_WS_RE = re.compile(r"\s+")

# Dotted acronyms are collapsed BEFORE punctuation is stripped: "U.S." and "A.I." must become the
# single tokens "us" and "ai", not the word pairs "u s" and "a i". Word shingles are sensitive to
# tokenisation, and one outlet writing "U.S." where another writes "US" would otherwise drag a pair
# of identical headlines from a Jaccard of 1.0 down to 0.2 — two events for one occurrence.
_ACRONYM_RE = re.compile(r"\b(?:[a-z]\.){2,}")


# ---- title normalisation -------------------------------------------------------------------------


def normalise_title(title: str) -> str:
    """NFKC, casefold, strip a known source prefix, strip punctuation, collapse whitespace."""
    text = unicodedata.normalize("NFKC", title or "").casefold()
    match = _PREFIX_RE.match(text)
    if match and match.group(1).strip() in KNOWN_SOURCE_PREFIXES:
        text = text[match.end():]
    text = _ACRONYM_RE.sub(lambda m: m.group(0).replace(".", ""), text)
    text = _PUNCT_RE.sub(" ", text)
    return _WS_RE.sub(" ", text).strip()


def shingles(title: str) -> tuple[str, ...]:
    """`k=5` word shingles over the normalised title, as a sorted tuple.

    A title shorter than k words yields its whole word sequence as one shingle rather than nothing:
    without this a five-word headline would have an empty signature and would collide with every
    other short headline.
    """
    words = normalise_title(title).split()
    if not words:
        return ()
    if len(words) <= SHINGLE_K:
        return (" ".join(words),)
    grams = {" ".join(words[i:i + SHINGLE_K]) for i in range(len(words) - SHINGLE_K + 1)}
    return tuple(sorted(grams))


def shingle_counts(title: str) -> Counter:
    """Shingle count vector, the basis for §3.6's cosine similarity."""
    return Counter(shingles(title))


# ---- MinHash -------------------------------------------------------------------------------------


def _permuted_hash(shingle: str, permutation: int) -> int:
    """One permutation's hash of one shingle. blake2b, salted by the permutation index."""
    digest = hashlib.blake2b(
        shingle.encode("utf-8"),
        digest_size=8,
        salt=permutation.to_bytes(2, "big"),
    ).digest()
    return int.from_bytes(digest, "big")


def minhash_signature(title_shingles: tuple[str, ...]) -> tuple[int, ...]:
    """The MinHash signature: `MINHASH_PERMUTATIONS` minima, one per permutation."""
    if not title_shingles:
        return (_MAX_HASH,) * MINHASH_PERMUTATIONS
    return tuple(
        min(_permuted_hash(s, i) for s in title_shingles)
        for i in range(MINHASH_PERMUTATIONS)
    )


def band_digests(signature: tuple[int, ...]) -> tuple[str, ...]:
    """One hex digest per band. Band 0 goes into `cluster_key`; 1..3 are the candidate index."""
    out = []
    for band in range(MINHASH_BANDS):
        start = band * MINHASH_BAND_ROWS
        chunk = b"".join(
            v.to_bytes(8, "big") for v in signature[start:start + MINHASH_BAND_ROWS]
        )
        out.append(hashlib.blake2b(chunk, digest_size=8).hexdigest())
    return tuple(out)


def minhash_band(title_shingles: tuple[str, ...]) -> str:
    """Contract §3.5's `minhash_band(title_shingles, k=5, bands=4)` — the band-0 digest."""
    return band_digests(minhash_signature(title_shingles))[0]


def estimated_jaccard(a: tuple[int, ...], b: tuple[int, ...]) -> float:
    """MinHash's estimate of |A∩B| / |A∪B|: the fraction of permutations that agree."""
    if not a or not b or a[0] == _MAX_HASH or b[0] == _MAX_HASH:
        # An empty shingle set has no similarity to anything, including another empty one.
        return 0.0
    agree = sum(1 for x, y in zip(a, b) if x == y)
    return agree / MINHASH_PERMUTATIONS


def bands_collide(a: tuple[int, ...], b: tuple[int, ...]) -> bool:
    """True when any of the four bands matches — the candidate test, not the join test."""
    return any(x == y for x, y in zip(band_digests(a), band_digests(b)))


# ---- keys ----------------------------------------------------------------------------------------


def parse_iso(value: str) -> datetime:
    """Parse an ISO 8601 UTC timestamp. Tolerates a 'Z' suffix and a missing offset."""
    text = str(value or "").strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    parsed = datetime.fromisoformat(text)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def iso(value) -> str:
    """Canonical fixed-width ISO 8601 UTC ending 'Z'.

    Fixed width matters: every `as_of` filter in this service is a lexicographic string comparison
    in SQL, and lexicographic order is chronological order only while the width is constant.
    """
    parsed = value if isinstance(value, datetime) else parse_iso(value)
    return parsed.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def date_bucket(occurred_at: str) -> str:
    """UTC calendar-day truncation.

    An event that straddles midnight UTC produces two events. That is accepted deliberately: a
    fuzzy window would make membership depend on the order documents are processed in, and
    order-dependent membership is exactly what §3.5 exists to prevent.
    """
    value = occurred_at if isinstance(occurred_at, date) else parse_iso(occurred_at).date()
    return value.isoformat()


def normalise_ticker_set(tickers) -> str:
    """§3.5's `normalised_primary_ticker_set`, rendered for hashing.

    Interpretation, recorded in the handoff: this is the set of PRIMARY tickers — a singleton,
    because §3.8 allows exactly one primary per event — not every ticker a document mentions. Using
    every mention would split one occurrence into two events the moment one outlet names a second
    company and another does not.
    """
    if isinstance(tickers, str):
        tickers = [tickers] if tickers else []
    return ",".join(sorted({str(t).strip().upper() for t in tickers if str(t).strip()}))


def cluster_key(ticker_set, event_type: str, occurred_at: str,
                title_shingles: tuple[str, ...]) -> str:
    """Contract §3.5, verbatim. 16 lowercase hex.

    A bucket, not an identity (§9.7): two documents can share every input here and still fail the
    Jaccard threshold, which must produce two events with the same key. `idx_events_cluster_key` is
    deliberately not UNIQUE.
    """
    material = "|".join((
        CLUSTER_VERSION,
        normalise_ticker_set(ticker_set),
        event_type,
        date_bucket(occurred_at),
        minhash_band(title_shingles),
    ))
    return hashlib.sha256(material.encode("utf-8")).hexdigest()[:16]
