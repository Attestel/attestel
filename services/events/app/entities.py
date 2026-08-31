"""Deterministic resolvers: source tier, ticker resolution, event-type guess.

Three pure functions over a stored document row. No network, no model, no wall clock. Every table
below is a named constant so that changing one is a versioned decision rather than an edit at a
call site.

`event_type_guess` feeds `cluster_key` (contract §3.5), so it is versioned by `CLUSTER_VERSION` and
must never change for a document once computed: a rule change here re-buckets the whole corpus and
is a version bump, not a tweak.

Nothing in this module calls `hash()`. Python salts the builtin hash over `str` per process
(`PYTHONHASHSEED`), and `services/analysis/app/data.py` already seeds synthetic bars that way —
copying that pattern here would make clustering irreproducible across runs, which is precisely what
Wave 5A's ablation ladder cannot survive.
"""
from __future__ import annotations

import json
import re
import unicodedata
from typing import Iterable, NamedTuple

from .config import SOURCE_TIERS

# ---- source tiers (contract §3.1, §3.2, doc §15.5) -----------------------------------------------

# Wave 1 Lane 1C has not landed, so there is no provider→tier map to import; this is the first
# declaration. When 1C ships one, this table is the one that must be deleted, not duplicated —
# two copies will drift. See the handoff.
PROVIDER_TIER: dict[str, str] = {
    "sec-edgar": "official",
    "fred": "official",
    "federal-reserve": "official",
    "company-ir": "official",
    # Phase 2 — direct official statistical agencies. Both publish their own release calendars.
    "bls": "official",
    "bea": "official",
    "marketaux": "professional",
    "google-news-rss": "professional",
    "alphavantage": "professional",
    "fmp": "professional",
}

# Rank order, not a string comparison: 'official' > 'professional' > 'discussion' is false
# alphabetically, and a lexical max() here would silently pick the wrong tier.
TIER_RANK: dict[str, int] = {"discussion": 0, "professional": 1, "official": 2}

# The `discussion` tier is honoured everywhere tier is read, but NO provider maps to it today.
# Adding one is a contract amendment (§3.1's provider list is closed), not a judgement call.
UNMAPPED_PROVIDER_TIER = "professional"

assert set(TIER_RANK) == set(SOURCE_TIERS)

# ---- scheduled-event source precedence (Phase 2) -------------------------------------------------
#
# THE TIER IS TOO COARSE FOR A CALENDAR DATE, and only for a calendar date.
#
# `source_tier` answers "how much of this document may we store" (D-29) and has three values by
# design; widening it would ripple through two CHECK constraints and every stored row. But the
# Calendar needs a FOURTH distinction that the tier cannot express:
#
#     official company/agency source  >  SEC filing  >  professional provider  >  secondary/news
#
# An 8-K that mentions a future event and NVIDIA's own investor-relations page are both `official`,
# yet the company's own schedule is the better authority on the company's own date — the filing
# states an intention, the IR page states the schedule. Ranking them equal would let whichever
# arrived second overwrite the other's date, arbitrarily.
#
# So precedence for `scheduled_events` merging is this map, not `TIER_RANK`. It is used ONLY there.
# Everything else — retention, excerpt caps, document tiering — still reads `source_tier`, unchanged.
PRECEDENCE_AGENCY = 4       # the agency or company that OWNS the date
PRECEDENCE_FILING = 3       # a regulatory filing that states it
PRECEDENCE_PROFESSIONAL = 2  # a structured commercial provider
PRECEDENCE_SECONDARY = 1    # news and everything else

SCHEDULED_SOURCE_PRECEDENCE: dict[str, int] = {
    "bls": PRECEDENCE_AGENCY,
    "bea": PRECEDENCE_AGENCY,
    "federal-reserve": PRECEDENCE_AGENCY,
    "fred": PRECEDENCE_AGENCY,
    "company-ir": PRECEDENCE_AGENCY,
    "sec-edgar": PRECEDENCE_FILING,
    "alphavantage": PRECEDENCE_PROFESSIONAL,
    "fmp": PRECEDENCE_PROFESSIONAL,
    "marketaux": PRECEDENCE_SECONDARY,
    "google-news-rss": PRECEDENCE_SECONDARY,
}


def scheduled_precedence(source: str) -> int:
    """Where a source ranks when two of them describe the same scheduled occurrence.

    An unknown source falls to `PRECEDENCE_SECONDARY`, the weakest rank — a new provider must earn
    its authority explicitly rather than inherit it by being unlisted.
    """
    return SCHEDULED_SOURCE_PRECEDENCE.get((source or "").strip().lower(), PRECEDENCE_SECONDARY)


def tier_for_provider(provider: str) -> str:
    """The tier a provider's documents carry. An unknown provider is treated conservatively."""
    return PROVIDER_TIER.get((provider or "").strip().lower(), UNMAPPED_PROVIDER_TIER)


def highest_tier(tiers: Iterable[str]) -> str:
    """The event's tier is the HIGHEST among its supporting documents (contract §3.3)."""
    ranked = [t for t in tiers if t in TIER_RANK]
    if not ranked:
        return "professional"
    return max(ranked, key=lambda t: (TIER_RANK[t], t))


def official_confirmed(tiers: Iterable[str]) -> int:
    """1 iff an `official` document supports the event (contract §3.3)."""
    return 1 if any(t == "official" for t in tiers) else 0


# ---- ticker resolution ---------------------------------------------------------------------------

# Resolution priority, lower is stronger. The filer identity on an SEC document is authoritative;
# a provider-reported ticker is next; an alias matched out of the title is weakest.
RESOLUTION_FILER = 0
RESOLUTION_PROVIDER = 1
RESOLUTION_ALIAS = 2

# Deterministic, coarse and WRITE-ONCE (contract §9.15). The enrichment endpoint rejects
# `relevance` exactly as it rejects `importance` and `novelty`: Wave 5B thresholds notifications on
# it, so it may not be a model-authored float.
RELEVANCE_PRIMARY = 1.0
RELEVANCE_SECONDARY_PROVIDER = 0.6
RELEVANCE_SECONDARY_ALIAS = 0.4

RELEVANCE_BY_PRIORITY = {
    RESOLUTION_FILER: RELEVANCE_SECONDARY_PROVIDER,
    RESOLUTION_PROVIDER: RELEVANCE_SECONDARY_PROVIDER,
    RESOLUTION_ALIAS: RELEVANCE_SECONDARY_ALIAS,
}

# Only these providers carry a filer identity. `fred` is market-wide and has none; `company-ir` is
# deliberately excluded so this stays exactly what the contract grants — the SEC filer.
FILER_PROVIDERS = ("sec-edgar",)

# Marketaux returns every company entity found anywhere in an article, including passing body
# mentions. Those values remain untouched in source_documents.raw_tickers for provenance, but only
# an entity explicitly named in the headline may become an event-company link. Structured sources
# such as SEC and Alpha Vantage retain their authoritative provider-reported mapping.
TITLE_SCOPED_ENTITY_PROVIDERS = ("marketaux",)

# Never fuzzy-match, never match a 1-2 character token: "AI" is not Air Products and "IT" is not
# an information-technology ETF. An unresolved document contributes NO ticker rather than a guess.
ALIAS_MIN_TOKEN_LENGTH = 3

# A bounded static table of company legal and trading names. It is deliberately small: every entry
# is a claim that this string, on a word boundary, means this company, and a wrong entry mislabels
# every event that mentions it.
TICKER_ALIASES: dict[str, tuple[str, ...]] = {
    "AAPL": ("apple inc", "apple"),
    "ADBE": ("adobe inc", "adobe"),
    "AMD": ("advanced micro devices", "amd"),
    "AMZN": ("amazon.com", "amazon"),
    "AVGO": ("broadcom",),
    "BA": ("boeing company", "boeing"),
    "CAT": ("caterpillar inc", "caterpillar"),
    "COST": ("costco wholesale", "costco"),
    "CRM": ("salesforce inc", "salesforce"),
    "CVX": ("chevron corporation", "chevron"),
    "DIS": ("walt disney", "disney"),
    "GOOGL": ("alphabet inc", "alphabet", "google"),
    "HD": ("home depot",),
    "INTC": ("intel corporation", "intel"),
    "JNJ": ("johnson and johnson", "johnson johnson"),
    "JPM": ("jpmorgan chase", "jp morgan chase", "jpmorgan"),
    "KO": ("coca cola",),
    "MA": ("mastercard inc", "mastercard"),
    "META": ("meta platforms", "meta"),
    "MSFT": ("microsoft corporation", "microsoft"),
    "NFLX": ("netflix inc", "netflix"),
    "NVDA": ("nvidia corporation", "nvidia"),
    "ORCL": ("oracle corporation", "oracle"),
    "PEP": ("pepsico inc", "pepsico"),
    "PG": ("procter and gamble", "procter gamble"),
    "QCOM": ("qualcomm inc", "qualcomm"),
    "TSLA": ("tesla inc", "tesla"),
    "TSM": ("taiwan semiconductor", "tsmc"),
    "UNH": ("unitedhealth group", "unitedhealth"),
    "V": ("visa inc", "visa"),
    "WMT": ("walmart inc", "walmart"),
    "XOM": ("exxon mobil", "exxonmobil", "exxon"),
}

# A provider-reported ticker still has to look like one; junk in `raw_tickers` becomes no ticker.
_TICKER_RE = re.compile(r"^[A-Z][A-Z0-9.\-]{0,9}$")

# Built once, in sorted ticker order, so iteration is deterministic regardless of dict insertion.
_ALIAS_PATTERNS: tuple[tuple[str, re.Pattern[str]], ...] = tuple(
    (ticker, re.compile(r"\b" + re.escape(alias) + r"\b"))
    for ticker in sorted(TICKER_ALIASES)
    for alias in sorted(TICKER_ALIASES[ticker], key=lambda a: (-len(a), a))
    if len(alias) >= ALIAS_MIN_TOKEN_LENGTH
)

_ALIAS_HAYSTACK_RE = re.compile(r"[^a-z0-9. ]+")

# Sorts after any real match position, so a provider-reported ticker absent from the title still
# gets a stable, documented ordering rather than a negative index or a None comparison.
_NO_POSITION = 10**6


class ResolvedTicker(NamedTuple):
    """One company this document touches, with how we know and how much we trust it."""

    ticker: str
    priority: int
    position: int
    relevance: float


def _alias_haystack(title: str) -> str:
    """Casefolded, punctuation-flattened title for word-boundary alias matching."""
    folded = unicodedata.normalize("NFKC", title or "").casefold()
    return " " + _ALIAS_HAYSTACK_RE.sub(" ", folded).strip() + " "


def _raw_ticker_list(raw_tickers) -> list[str]:
    """`raw_tickers` as the provider reported it: uppercased, trimmed, deduped, order preserved."""
    values = raw_tickers
    if isinstance(values, str):
        try:
            values = json.loads(values or "[]")
        except (TypeError, ValueError):
            return []
    if not isinstance(values, (list, tuple)):
        return []
    out: list[str] = []
    for value in values:
        token = str(value or "").strip().upper()
        if token and _TICKER_RE.match(token) and token not in out:
            out.append(token)
    return out


def resolve_tickers(document) -> list[ResolvedTicker]:
    """Every company this document touches, strongest resolution first.

    Priority order, per the contract: (1) the SEC filer identity, (2) `raw_tickers` as the provider
    reported it, (3) a bounded static alias table matched on word boundaries. Marketaux is the one
    exception to (2): its article-wide entities are accepted only when the headline names them.
    Ties break on the ticker's position in the title, then lexically, so the ordering is total and
    stable.
    """
    provider = str(document["provider"] or "").strip().lower()
    title = str(document["title"] or "")
    haystack = _alias_haystack(title)
    reported = _raw_ticker_list(document["raw_tickers"])

    found: dict[str, int] = {}
    if provider in FILER_PROVIDERS and reported:
        # The filer is the subject of its own filing; the rest of the list is ordinary reporting.
        found[reported[0]] = RESOLUTION_FILER
    for token in reported:
        if provider in TITLE_SCOPED_ENTITY_PROVIDERS \
                and _title_subject_position(token, haystack) == _NO_POSITION:
            continue
        found.setdefault(token, RESOLUTION_PROVIDER)
    for ticker, pattern in _ALIAS_PATTERNS:
        if ticker not in found and _first_subject_match(pattern, haystack) is not None:
            found[ticker] = RESOLUTION_ALIAS

    resolved = []
    for ticker, priority in found.items():
        position = _title_subject_position(ticker, haystack)
        resolved.append(
            ResolvedTicker(ticker, priority, position, RELEVANCE_BY_PRIORITY[priority])
        )
    resolved.sort(key=lambda r: (r.priority, r.position, r.ticker))
    return resolved


def _first_subject_match(pattern: re.Pattern[str], haystack: str) -> re.Match[str] | None:
    """First non-excluded headline mention.

    A title such as "Not Nvidia. Not AMD." explicitly says those companies are not its subject.
    The bounded `not <company>` rule fixes that observed failure without attempting sentiment or
    open-ended language understanding here.
    """
    for match in pattern.finditer(haystack):
        prefix = haystack[:match.start()].rstrip()
        if prefix and prefix.rsplit(" ", 1)[-1] == "not":
            continue
        return match
    return None


def _title_subject_position(ticker: str, haystack: str) -> int:
    """Where this company is first named as a headline subject, by ticker or known alias."""
    best = _NO_POSITION
    names = (ticker.casefold(), *TICKER_ALIASES.get(ticker, ()))
    for name in names:
        if len(name) < ALIAS_MIN_TOKEN_LENGTH:
            continue
        match = _first_subject_match(
            re.compile(r"\b" + re.escape(name) + r"\b"), haystack
        )
        if match and match.start() < best:
            best = match.start()
    return best


def primary_ticker(resolved: Iterable[ResolvedTicker]) -> str | None:
    """The subject company. Exactly one per event; None for a market-wide document."""
    ordered = sorted(resolved, key=lambda r: (r.priority, r.position, r.ticker))
    return ordered[0].ticker if ordered else None


# ---- event type (contract §3.4) ------------------------------------------------------------------

EVENT_TYPES = (
    "earnings_result", "earnings_guidance", "analyst_revision", "product_launch",
    "management_change", "ma_transaction", "regulatory_action", "legal_action",
    "supply_chain", "capital_return", "financing", "partnership", "macro_release",
    "central_bank", "sector_event", "other",
)

DEFAULT_EVENT_TYPE = "other"

# SEC 8-K item labels as gateway/providers.go's `sec8KItems` renders them into a headline
# ("8-K filed 2026-08-14 — Results of Operations"). Matching the label is far more reliable than
# matching the prose that follows it.
SEC_ITEM_TYPES: tuple[tuple[str, str], ...] = (
    ("results of operations", "earnings_result"),
    ("exec/director change", "management_change"),
    ("acquisition/disposition", "ma_transaction"),
    ("material agreement", "partnership"),
    ("new financial obligation", "financing"),
    ("unregistered equity sales", "financing"),
    ("shareholder vote", "management_change"),
    ("bylaw amendment", "management_change"),
    ("cybersecurity incident", "regulatory_action"),
)

SEC_FORM_TYPES: tuple[tuple[str, str], ...] = (
    ("10-q", "earnings_result"),
    ("10-k", "earnings_result"),
    ("8-k", "other"),
)

# Ordered most-specific first; the FIRST match wins, so the order is part of the versioned rule and
# reordering it is a CLUSTER_VERSION bump. Keywords are matched on word boundaries against the
# casefolded, punctuation-flattened title.
TITLE_TYPE_RULES: tuple[tuple[str, tuple[str, ...]], ...] = (
    ("central_bank", ("fomc", "federal reserve", "rate decision", "interest rate decision",
                      "central bank", "ecb", "bank of japan", "rate cut", "rate hike")),
    ("macro_release", ("cpi report", "consumer price index", "inflation data", "pce",
                       "nonfarm payrolls", "jobs report", "unemployment rate", "gdp growth",
                       "gdp report", "retail sales data")),
    ("earnings_guidance", ("guidance", "outlook", "forecasts revenue", "raises forecast",
                           "cuts forecast", "full-year view")),
    ("earnings_result", ("earnings", "quarterly results", "beats estimates", "misses estimates",
                         "reports revenue", "record revenue", "quarterly revenue")),
    ("analyst_revision", ("price target", "upgrades", "downgrades", "initiates coverage",
                          "raises target", "cuts target", "trim estimates", "raises estimates")),
    ("regulatory_action", ("export restriction", "export restrictions", "export control",
                           "export controls", "sanction", "sanctions", "antitrust", "tariff",
                           "tariffs", "regulator", "ftc", "doj probe", "investigation into")),
    ("legal_action", ("lawsuit", "sues", "settlement", "class action", "patent infringement",
                      "court ruling", "jury")),
    ("ma_transaction", ("acquire", "acquires", "acquisition", "merger", "to buy", "takeover",
                        "stake in", "divest")),
    ("management_change", ("chief executive", "ceo", "cfo", "resigns", "steps down", "appoints",
                           "names new")),
    ("capital_return", ("dividend", "buyback", "share repurchase", "stock split")),
    ("financing", ("senior notes", "bond offering", "convertible notes", "credit facility",
                   "secondary offering", "raises capital")),
    ("partnership", ("partnership", "partners with", "collaboration", "joint venture",
                     "teams up", "expand their")),
    ("product_launch", ("unveils", "launches", "introduces", "debuts", "announces new",
                        "next-generation")),
    ("supply_chain", ("supply", "supplier", "foundry", "wafer", "chip shortage", "capacity",
                      "production cut", "shipment")),
    ("sector_event", ("semiconductor sector", "chip stocks", "sector rotation", "sector-wide")),
)

# `fred` publishes economic series and nothing else; the title rules refine macro vs central bank.
PROVIDER_DEFAULT_TYPE: dict[str, str] = {"fred": "macro_release"}

_KEYWORD_PATTERNS: tuple[tuple[str, tuple[re.Pattern[str], ...]], ...] = tuple(
    (event_type, tuple(re.compile(r"\b" + re.escape(kw) + r"\b") for kw in keywords))
    for event_type, keywords in TITLE_TYPE_RULES
)

# The tables above are written the way gateway/providers.go renders them ("Exec/Director Change",
# "8-K") so the two are visibly the same strings; matching happens against the punctuation-flattened
# haystack, so the labels are flattened the same way here rather than being restated by hand.
_SEC_ITEM_PATTERNS: tuple[tuple[str, str], ...] = tuple(
    (_alias_haystack(label).strip(), event_type) for label, event_type in SEC_ITEM_TYPES
)
_SEC_FORM_PATTERNS: tuple[tuple[re.Pattern[str], str], ...] = tuple(
    (re.compile(r"\b" + re.escape(_alias_haystack(form).strip()) + r"\b"), event_type)
    for form, event_type in SEC_FORM_TYPES
)


def guess_event_type(document) -> str:
    """A member of the §3.4 closed enum. Unmatched ⇒ `other`, never an invented type.

    This value is an INPUT to `cluster_key`, so it must be stable for a given document forever.
    Version it with CLUSTER_VERSION; do not "improve" a rule in place.
    """
    provider = str(document["provider"] or "").strip().lower()
    haystack = _alias_haystack(document["title"])

    if provider in FILER_PROVIDERS:
        for label, event_type in _SEC_ITEM_PATTERNS:
            if label in haystack:
                return event_type
        for pattern, event_type in _SEC_FORM_PATTERNS:
            if pattern.search(haystack):
                if event_type != DEFAULT_EVENT_TYPE:
                    return event_type
                break  # an 8-K with no recognised item falls through to the title rules

    for event_type, patterns in _KEYWORD_PATTERNS:
        if any(p.search(haystack) for p in patterns):
            return event_type

    return PROVIDER_DEFAULT_TYPE.get(provider, DEFAULT_EVENT_TYPE)
