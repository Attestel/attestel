"""The ingestion providers owned by the canonical event store (contract §3.1, §3.11, AD-10).

The first implementation was ported from the gateway. Calendar providers now live only here; the
gateway counterparts were removed so a read cannot accidentally become an ingest:

| here                            | gateway                                      |
|---------------------------------|----------------------------------------------|
| `fetch_marketaux`               | `providers.go::fetchNews`                    |
| `fetch_sec_filings`             | `providers.go::fetchSECFilings`              |
| `fetch_rss_news`                | `research.go::fetchRSSNews`                  |
| `fetch_alphavantage_earnings`   | `providers.go::fetchEarnings`                |
| `fetch_fred_releases`           | legacy `calendar.go::fetchFREDReleases`      |
| `fetch_fmp_calendar`            | legacy `calendar.go::fetchFMPCalendar`       |
| `fetch_federal_reserve_calendar`| direct official FOMC calendar                |

Five rules, all of them load-bearing:

1. **No key ⇒ skip, with a `degraded` entry. Never attempt, never raise.** This is what makes the
   empty-`.env` path work (CLAUDE.md invariant #1): the service starts, `/health` is green and a
   full ingest pass fetches nothing, inserts nothing and raises nothing.
2. **Every call goes through `budget.reserve()` first.** A provider function may not touch
   `requests` without a reservation — `tests/test_budget.py` asserts this against the source text.
3. **Timestamps are normalised on the way in** to `%Y-%m-%dT%H:%M:%SZ`, then stored as PostgreSQL
   `timestamptz`. A document
   whose published time cannot be parsed is **dropped with a `degraded` entry** — never stored with
   `retrieved_at` standing in for `published_at`, because a wrong timestamp is a leak in disguise.
4. **`raw_tickers` is exactly what the provider reported.** No normalisation, no inference. Mapping
   documents to companies is `entities.resolve_tickers`' job and it must be able to see what the
   provider actually said.
5. **There is no LLM in this module and none in this lane.** No model call, no import from
   `services.llm`, no URL pointing at :8002. Enrichment is Wave 2 Lane 2B, under the D-25 lease.

Timeouts are on every request, in the 10–15s range the gateway already uses. There are **no retries
here**: retry is the scheduler's and `budget.py`'s business, so a provider function cannot quietly
spend three reservations on one logical fetch.

The provider→tier map is NOT declared here. `entities.PROVIDER_TIER` is the first and only
declaration (Wave 2 Lane 2A landed before this lane); this module imports `tier_for_provider` from
it, so the two cannot drift. See the handoff.
"""
from __future__ import annotations

import csv
import io
import json
import os
import re
import unicodedata
import xml.etree.ElementTree as ElementTree
from dataclasses import dataclass, field
from datetime import date, datetime, timezone
from email.utils import parsedate_to_datetime
from html.parser import HTMLParser
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

import requests

from .db import Connection

from .budget import (
    REASON_ERROR,
    REASON_DISABLED,
    REASON_NO_CIK,
    REASON_NO_COVERAGE,
    REASON_NO_KEY,
    REASON_UNPARSEABLE_TIME,
    degraded,
    record_failure,
    record_success,
    reserve,
)
from . import ir_registry
from .entities import resolve_tickers, tier_for_provider

# ---- provider names (the §3.1 closed set this lane emits) ----------------------------------------

MARKETAUX = "marketaux"
SEC_EDGAR = "sec-edgar"
GOOGLE_NEWS_RSS = "google-news-rss"
ALPHAVANTAGE = "alphavantage"
FRED = "fred"
FMP = "fmp"
FEDERAL_RESERVE = "federal-reserve"
# Phase 2 — the two statistical agencies that OWN the releases FRED merely redistributes, plus the
# company's own investor-relations schedule.
BLS = "bls"
BEA = "bea"
COMPANY_IR = "company-ir"

FOMC_CALENDAR_URL = "https://www.federalreserve.gov/monetarypolicy/fomccalendars.htm"
FOMC_RELEASE_CLOCK = "14:00"
FEDERAL_RESERVE_ENABLED_ENV = "FEDERAL_RESERVE_ENABLED"

# Each Phase 2 collector has its OWN enable gate, read with no default (§9.42): a gate whose
# variable has a default closes nothing. All three are keyless, so the gate is the only thing
# standing between an empty `.env` and an outbound socket.
#: Phase 2D. How many 8-K PRIMARY DOCUMENTS one pass may fetch the text of. Default 0 — OFF.
#:
#: The submissions JSON carries filing metadata only; the sentence that states "we will report on
#: November 18, 2026" lives in the filing itself, one request away. Fetching every filing's text
#: would multiply this provider's budget by the filing count, which is why the original collector
#: deliberately did not. So it is a bounded, explicit opt-in: 0 keeps the request count exactly
#: what it was, and `planned_events` still works on whatever text is stored (title and excerpt).
SEC_BODY_FETCH_LIMIT_ENV = "SEC_BODY_FETCH_LIMIT"

BLS_ENABLED_ENV = "BLS_ENABLED"
BEA_ENABLED_ENV = "BEA_ENABLED"
COMPANY_IR_ENABLED_ENV = "COMPANY_IR_ENABLED"

#: The official BLS release schedule, one page per calendar year. This is the agency's own
#: publication of its own dates — the top of the precedence order in `entities`.
BLS_SCHEDULE_URL = "https://www.bls.gov/schedule/{year}/"
#: The official BEA release schedule.
BEA_SCHEDULE_URL = "https://www.bea.gov/news/schedule"

# §3.9's `expectation_source` enum happens to spell the FMP provider the same way. Named separately
# so a provider rename cannot silently write a value the schema's CHECK constraint rejects.
FMP_EXPECTATION_SOURCE = "fmp"

# ---- request settings ------------------------------------------------------------------------------

REQUEST_TIMEOUT_SECONDS = 15
RESPONSE_BYTE_CAP = 4 * 1024 * 1024  # the gateway caps the RSS read at 1 MiB; this is the same idea

# THE EVENTS SERVICE HAS ITS OWN CONTACT VARIABLE (§9.45): `EVENTS_CONTACT_UA`, not the gateway's
# `SEC_USER_AGENT`. Compose hands ONE `.env` to every service, and `SEC_USER_AGENT` must stay
# populated for the gateway — SEC blocks generic user-agents — so a shared name cannot be both
# populated there and empty here, and `cp .env.example .env` silently enabled corpus ingestion.
#
# The two are not duplicates. The gateway's means "identify us to SEC on a per-request fetch". This
# one means "we are permitted to build a PERSISTENT, CROSS-USER CORPUS from keyless sources" — the
# D-29 posture, a materially higher bar that must be opted into explicitly. Conflating them is what
# produced the original violation: a full pass against an empty environment fetched and stored 60
# real articles.
#
# It gates BOTH keyless providers, SEC EDGAR and Google News RSS, because both write to that same
# corpus. Without it we do not call SEC at all — a 403 loop is worse than a labelled skip.
#
# Named once, here, so a rename is one line and a test can assert the name rather than a string
# literal scattered across three fetchers.
CONTACT_UA_ENV = "EVENTS_CONTACT_UA"
DEFAULT_ITEM_LIMIT = 20

# ---- ticker → CIK -----------------------------------------------------------------------------------
#
# `gateway/tickers.go::builtinCIK`, copied verbatim. The gateway ALSO resolves CIKs dynamically from
# SEC's company_tickers.json; this lane deliberately does not, because that is a second provider
# call whose only purpose is to enable a first one. A ticker with no known CIK is a SKIP with a
# `degraded` entry — never an error, and never a guessed CIK.
BUILTIN_CIK: dict[str, str] = {
    "NVDA": "0001045810",
    "GOOGL": "0001652044",
    "TSLA": "0001318605",
}

# `gateway/providers.go::sec8KItems`, copied verbatim. The labels are what `entities.guess_event_type`
# matches on, so the two must render identically — see `_sec_headline`.
SEC_8K_ITEMS: dict[str, str] = {
    "1.01": "Material Agreement", "1.05": "Cybersecurity Incident",
    "2.01": "Acquisition/Disposition", "2.02": "Results of Operations",
    "2.03": "New Financial Obligation", "3.02": "Unregistered Equity Sales",
    "5.02": "Exec/Director Change", "5.03": "Bylaw Amendment",
    "5.07": "Shareholder Vote", "7.01": "Reg FD Disclosure",
    "8.01": "Other Events", "9.01": "Financial Statements & Exhibits",
}

# The curated FRED allowlist formerly lived in gateway/calendar.go. It is declared here now that
# providers are ingestion-only. `series` is the §3.9 series label this release prints.
FRED_RELEASES: dict[int, tuple[str, str, str, str]] = {
    # name, canonical series, qualitative importance, regular US/Eastern release clock
    10: ("Consumer Price Index (CPI)", "CPI", "high", "08:30"),
    46: ("Producer Price Index (PPI)", "PPI", "medium", "08:30"),
    50: ("Employment Situation (Jobs)", "NFP", "high", "08:30"),
    53: ("Gross Domestic Product (GDP)", "GDP", "high", "08:30"),
    54: ("Personal Income & Outlays (PCE)", "PCE", "high", "08:30"),
    8: ("Advance Retail Sales", "RSAFS", "medium", "08:30"),
    175: ("ISM Manufacturing PMI", "ISM", "medium", "10:00"),
    91: ("U. Michigan Consumer Sentiment", "UMCSENT", "medium", "10:00"),
    13: ("Durable Goods Orders", "DGORDER", "medium", "08:30"),
    20: ("Employment Cost Index", "ECI", "medium", "08:30"),
}

MACRO_RELEVANCE: dict[str, str] = {
    "CPI": "Inflation data can change expectations for interest rates and broad equity valuations.",
    "CORE_CPI": "Core inflation helps separate persistent price pressure from volatile components.",
    "PPI": "Producer prices can inform inflation and margin pressure before it reaches consumers.",
    "NFP": "Employment data informs the strength of demand and the Federal Reserve policy backdrop.",
    "GDP": "GDP is a broad check on economic growth and demand conditions.",
    "PCE": "PCE inflation is a key input to the Federal Reserve's inflation assessment.",
    "RSAFS": "Retail sales provide a timely view of consumer demand.",
    "ISM": "Manufacturing activity can reveal changes in demand, orders and supply conditions.",
    "UMCSENT": "Consumer sentiment can help explain changes in household demand expectations.",
    "DGORDER": "Durable-goods orders can indicate changes in business and household investment.",
    "ECI": "Employment costs are an input to inflation and corporate margin analysis.",
    "FOMC": "Federal Reserve decisions can change the policy-rate and liquidity backdrop.",
}


# =================================================================================================
# Results
# =================================================================================================


@dataclass
class FetchResult:
    """What one provider call produced.

    `documents` are `source_documents` rows minus the columns `documents.py` assigns (`id`,
    `content_hash`, `first_seen_at`, `retrieved_at`, `ingest_run_id`). `scheduled` are
    `scheduled_events` rows (§9.27), which the calendar providers produce and the news providers do
    not. `macro` are `macro_events` rows (§3.9, §9.41) — the STRUCTURED numbers, taken from the
    provider response directly rather than re-parsed out of the document text later. `degraded` is
    always `"<provider>:<reason>"`.
    """

    documents: list[dict] = field(default_factory=list)
    scheduled: list[dict] = field(default_factory=list)
    macro: list[dict] = field(default_factory=list)
    degraded: list[str] = field(default_factory=list)

    def extend(self, other: "FetchResult") -> "FetchResult":
        self.documents.extend(other.documents)
        self.scheduled.extend(other.scheduled)
        self.macro.extend(other.macro)
        self.degraded.extend(other.degraded)
        return self


# =================================================================================================
# Environment (read at call time, never at import — a test must be able to set a key)
# =================================================================================================


def _env(name: str) -> str:
    return os.getenv(name, "").strip()


def marketaux_key() -> str:
    return _env("MARKETAUX_API_KEY")


def alphavantage_key() -> str:
    return _env("ALPHAVANTAGE_API_KEY")


def fred_key() -> str:
    return _env("FRED_API_KEY")


def fmp_key() -> str:
    return _env("FMP_API_KEY")


def contact_user_agent() -> str:
    """The events service's own contact address (§9.45). NO DEFAULT — `_env` returns `""` for an
    absent or blank variable, and every caller treats `""` as "skip this provider, label the
    skip". A default here would close nothing while every unit test kept passing (§9.42)."""
    return _env(CONTACT_UA_ENV)


def federal_reserve_enabled() -> bool:
    """Explicit network gate for the direct official calendar; false on absent/malformed input."""
    return _env(FEDERAL_RESERVE_ENABLED_ENV).casefold() in {"1", "true", "yes", "on"}


def _gate(name: str) -> bool:
    """A Phase 2 collector gate. No default, false on absent or malformed input (§9.42)."""
    return _env(name).casefold() in {"1", "true", "yes", "on"}


def sec_body_fetch_limit() -> int:
    """How many filing bodies this pass may retrieve. 0 (the default) means none."""
    try:
        value = int(_env(SEC_BODY_FETCH_LIMIT_ENV) or 0)
    except ValueError:
        return 0
    return max(0, min(value, 25))


def bls_enabled() -> bool:
    return _gate(BLS_ENABLED_ENV)


def bea_enabled() -> bool:
    return _gate(BEA_ENABLED_ENV)


def company_ir_enabled() -> bool:
    return _gate(COMPANY_IR_ENABLED_ENV)


# =================================================================================================
# Timestamps (§1.1: a string comparison is only chronological while the width is fixed)
# =================================================================================================

_DATE_ONLY = re.compile(r"^\d{4}-\d{2}-\d{2}$")
_SPACE_DATETIME = re.compile(r"^(\d{4}-\d{2}-\d{2})[ T](\d{2}:\d{2}(:\d{2})?)$")


def normalise_timestamp(value) -> str | None:
    """`%Y-%m-%dT%H:%M:%SZ`, or None when the value cannot be parsed.

    Handles every timestamp shape the providers actually emit: RFC 3339 with or without an offset or
    fractional seconds (Marketaux, our own), RFC 1123 (`Mon, 17 Aug 2026 13:21:00 GMT` — Google
    News RSS), `YYYY-MM-DD HH:MM:SS` (FMP) and a bare `YYYY-MM-DD` (SEC filing dates, Alpha Vantage
    report dates, FRED release dates).

    A bare date becomes midnight UTC. That is a real interpretation and it is recorded here rather
    than hidden: an SEC filing dated 2026-08-14 is treated as visible from the start of that UTC
    day. It is the conservative direction — it can only make a document appear EARLIER than it
    truly did in `published_at`, and `first_seen_at` (which is a real wall-clock stamp) still gates
    the as-of filter's second half, so a backfilled document cannot leak.
    """
    if isinstance(value, datetime):
        parsed = value if value.tzinfo else value.replace(tzinfo=timezone.utc)
        return parsed.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    text = str(value or "").strip()
    if not text:
        return None
    if _DATE_ONLY.match(text):
        text = text + "T00:00:00+00:00"
    else:
        match = _SPACE_DATETIME.match(text)
        if match:
            clock = match.group(2)
            if len(clock) == 5:
                clock += ":00"
            text = f"{match.group(1)}T{clock}+00:00"
        elif text.endswith("Z"):
            text = text[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError:
        try:
            parsed = parsedate_to_datetime(str(value).strip())
        except (TypeError, ValueError):
            return None
        if parsed is None:
            return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def eastern_release_timestamp(date: str, clock: str) -> str | None:
    """A published US/Eastern release clock converted to canonical UTC.

    The prior calendar stored a bare FRED date at midnight and displayed an unrelated in-code ET
    clock beside it.  One instant is safer: it sorts correctly, survives DST, and every consumer
    sees the same schedule.  Invalid source values remain absent; they are never guessed.
    """
    try:
        local = datetime.strptime(f"{date} {clock}", "%Y-%m-%d %H:%M").replace(
            tzinfo=ZoneInfo("America/New_York")
        )
    except (TypeError, ValueError, ZoneInfoNotFoundError):
        return None
    return local.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def canonical_macro_series(name: str) -> str:
    """Map provider labels onto a bounded, deterministic macro vocabulary.

    Unknown releases keep a normalised form instead of being silently dropped.  Specific variants
    are checked before broad families so Core CPI does not collapse into the headline CPI event.
    """
    value = _clean(name).casefold()
    rules = (
        (("core cpi", "core consumer price"), "CORE_CPI"),
        (("consumer price", "cpi"), "CPI"),
        (("producer price", "ppi"), "PPI"),
        (("nonfarm", "non-farm", "employment situation", "payroll"), "NFP"),
        (("gross domestic product", "gdp"), "GDP"),
        (("personal consumption", "personal income & outlays", "pce"), "PCE"),
        (("retail sales",), "RSAFS"),
        (("ism manufacturing", "manufacturing pmi"), "ISM"),
        (("michigan", "consumer sentiment"), "UMCSENT"),
        (("durable goods",), "DGORDER"),
        (("employment cost",), "ECI"),
        (("fomc", "federal funds", "fed interest rate", "rate decision"), "FOMC"),
    )
    for needles, series in rules:
        if any(needle in value for needle in needles):
            return series
    return re.sub(r"[^A-Z0-9]+", "_", _clean(name).upper()).strip("_")[:40]


def macro_relevance(series: str) -> str:
    return MACRO_RELEVANCE.get(
        series,
        "This scheduled macro release may update the economic backdrop used in company research.",
    )


def _clean(value) -> str:
    """NFKC + whitespace-collapsed, for a title or an excerpt on the way in."""
    text = unicodedata.normalize("NFKC", str(value or ""))
    return re.sub(r"\s+", " ", text).strip()


# =================================================================================================
# The one outbound call
# =================================================================================================


@dataclass
class Response:
    ok: bool
    text: str = ""
    status: int | None = None
    degraded: list[str] = field(default_factory=list)

    def json(self):
        return json.loads(self.text)


def _get(
    conn: Connection,
    provider: str,
    url: str,
    *,
    params: dict | None = None,
    headers: dict | None = None,
) -> Response:
    """The ONLY place this module calls `requests`. Budget first, request second.

    Fails soft on absolutely everything: a refused reservation, a transport error, a non-2xx status
    and a decode failure all come back as `ok=False` with a `degraded` entry. Nothing raises.
    """
    allowed, reason = reserve(conn, provider)
    if not allowed:
        return Response(ok=False, degraded=[reason] if reason else [])

    try:
        response = requests.get(
            url,
            params=params or None,
            headers=headers or None,
            timeout=REQUEST_TIMEOUT_SECONDS,
        )
    except Exception as exc:  # noqa: BLE001 - every provider fails soft, no exception escapes
        record_failure(conn, provider, status=None, error=f"{type(exc).__name__}: {exc}")
        return Response(ok=False, degraded=[degraded(provider, REASON_ERROR)])

    status = getattr(response, "status_code", None)
    if status is None or status >= 400:
        record_failure(conn, provider, status=status, error=f"http {status}")
        return Response(ok=False, status=status, degraded=[degraded(provider, REASON_ERROR)])

    record_success(conn, provider)
    text = getattr(response, "text", "") or ""
    return Response(ok=True, text=text[:RESPONSE_BYTE_CAP], status=status)


def _document(
    *,
    provider: str,
    url: str,
    title: str,
    excerpt: str,
    published_at: str,
    raw_tickers,
    body: str | None = None,
) -> dict:
    """One normalised `source_documents` row, minus the columns `documents.py` assigns.

    `source_tier` comes from `entities.tier_for_provider` — the single provider→tier map — never
    from a judgement at this call site. `body` is passed through untouched; `documents.py` is the
    thing that decides whether it may be stored (D-29: `official` only).
    """
    return {
        "provider": provider,
        "source_tier": tier_for_provider(provider),
        "url": url,
        "title": title,
        "excerpt": excerpt,
        "body": body,
        "published_at": published_at,
        "raw_tickers": list(raw_tickers or []),
    }


def macro_key(series: str, release_at: str) -> str:
    """`"{series}|{release_at ISO}"` — §9.41.3's link between a macro document and its release row.

    The one place this string is built. It is stamped on the document AND is the natural key of the
    `macro_events` row, so Lane 2A's backfill can go from a clustered document straight to the row
    without a second convention to keep in step.
    """
    return f"{series}|{release_at}"


def _macro(*, series: str, release_at: str, actual=None, expected=None, previous=None,
           unit: str = "", expectation_source: str | None = None) -> dict:
    """One `macro_events` row (§3.9), minus everything the store decides.

    `surprise` is NOT here and never comes from a provider: it is computed in code by
    `macro.compute_surprise`, behind its named refusals. Neither is `expectation_captured_at` — the
    store stamps the moment WE captured the consensus, because a provider cannot be trusted to say
    when a number became knowable, and that timestamp is the whole of §3.9's timing rule.

    `event_id` is absent too: 1C does not know it and does not guess. §9.41.5 gives the backfill to
    Lane 2A's `assemble()`.
    """
    return {
        "series": series,
        "release_at": release_at,
        "actual": actual,
        "expected": expected,
        "previous": previous,
        "unit": unit,
        "expectation_source": expectation_source,
    }


def _number(value) -> float | None:
    """A provider's numeric field as a float, or None. A blank, a dash or prose is None, never 0.0 —
    a missing consensus and a consensus of zero are different facts and §3.9 turns on the
    difference."""
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, (int, float)):
        return float(value)
    text = str(value).strip().replace(",", "")
    if not text:
        return None
    suffix = 1.0
    if text.endswith("%"):
        text = text[:-1].strip()
    elif text[-1] in "KMB":
        suffix = {"K": 1e3, "M": 1e6, "B": 1e9}[text[-1]]
        text = text[:-1].strip()
    try:
        return float(text) * suffix
    except ValueError:
        return None


def _scheduled(
    *,
    kind: str,
    ticker: str | None,
    series: str | None,
    scheduled_at: str,
    confirmed: bool,
    source: str,
    title: str,
    description: str,
    importance: str,
    status: str,
    source_url: str,
    timezone_name: str = "UTC",
    local_time: str = "",
    previous=None,
    expected=None,
    actual=None,
    unit: str = "",
    occurrence_key: str | None = None,
) -> dict:
    """One canonical `scheduled_events` record, minus store-owned identity/timestamps.

    Importance and relevance are qualitative deterministic labels.  No impact percentage is
    emitted, and surprise is deliberately absent here: the store computes it only when both facts
    exist, using the same arithmetic as the macro release table.
    """
    return {
        "kind": kind,
        "ticker": ticker,
        "series": series,
        "scheduled_at": scheduled_at,
        "confirmed": 1 if confirmed else 0,
        "source": source,
        "source_tier": tier_for_provider(source),
        "source_url": source_url,
        "title": title,
        "description": description,
        "importance": importance,
        "status": status,
        "timezone": timezone_name,
        "local_time": local_time,
        "previous": previous,
        "expected": expected,
        "actual": actual,
        "unit": unit,
        "occurrence_key": occurrence_key,
    }


# =================================================================================================
# Marketaux — gateway/providers.go::fetchNews
# =================================================================================================


def fetch_marketaux(conn, *, ticker: str, limit: int = DEFAULT_ITEM_LIMIT) -> FetchResult:
    """Entity-tagged news. Professional tier: link + bounded excerpt, never a body (D-29)."""
    result = FetchResult()
    key = marketaux_key()
    if not key:
        result.degraded.append(degraded(MARKETAUX, REASON_NO_KEY))
        return result

    response = _get(
        conn,
        MARKETAUX,
        "https://api.marketaux.com/v1/news/all",
        params={
            "symbols": ticker,
            "filter_entities": "true",
            "language": "en",
            "limit": limit,
            "api_token": key,
        },
    )
    if not response.ok:
        result.degraded.extend(response.degraded)
        return result

    try:
        payload = response.json()
    except (ValueError, TypeError):
        result.degraded.append(degraded(MARKETAUX, REASON_ERROR))
        return result
    if not isinstance(payload, dict) or payload.get("error"):
        result.degraded.append(degraded(MARKETAUX, REASON_ERROR))
        return result

    dropped = 0
    for article in payload.get("data") or []:
        if not isinstance(article, dict):
            continue
        published_at = normalise_timestamp(article.get("published_at"))
        if not published_at:
            dropped += 1
            continue
        symbols = [
            str(e.get("symbol") or "")
            for e in (article.get("entities") or [])
            if isinstance(e, dict) and e.get("symbol")
        ]
        document = _document(
            provider=MARKETAUX,
            url=str(article.get("url") or ""),
            title=_clean(article.get("title")),
            excerpt=_clean(article.get("description")),
            published_at=published_at,
            raw_tickers=symbols,
        )
        # Marketaux's entity list covers the whole article body. Keep that list unchanged on an
        # accepted source document, but do not store a ticker-query result whose headline has no
        # resolvable company subject: it would become generic corpus noise, or worse, company news
        # for a body-only mention. Macro sources have their own dedicated collectors.
        if not resolve_tickers(document):
            continue
        result.documents.append(document)
    if dropped:
        result.degraded.append(degraded(MARKETAUX, REASON_UNPARSEABLE_TIME))
    return result


# =================================================================================================
# SEC EDGAR 8-K — gateway/providers.go::fetchSECFilings
# =================================================================================================


class _TextExtractor(HTMLParser):
    """Visible text from a filing's HTML. Script and style content is discarded, not rendered.

    Deliberately crude: `planned_events` reads SENTENCES, so what matters is that words and
    punctuation survive in order and that tag soup does not glue two sentences together. Every tag
    becomes a space for exactly that reason.
    """

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.parts: list[str] = []
        self._muted = 0

    def handle_starttag(self, tag, attrs):
        if tag in ("script", "style"):
            self._muted += 1
        else:
            self.parts.append(" ")

    def handle_endtag(self, tag):
        if tag in ("script", "style") and self._muted:
            self._muted -= 1
        else:
            self.parts.append(" ")

    def handle_data(self, data):
        if not self._muted:
            self.parts.append(data)


def _strip_markup(text: str) -> str:
    parser = _TextExtractor()
    try:
        parser.feed(text or "")
    except Exception:  # noqa: BLE001 — malformed filing markup is normal, not exceptional
        return _clean(text)
    return _clean("".join(parser.parts))


def _item_labels(items: str) -> str:
    """`gateway/providers.go::itemLabels`, verbatim: known 8-K item codes → their labels."""
    if not items:
        return ""
    labels = []
    for code in str(items).split(","):
        label = SEC_8K_ITEMS.get(code.strip())
        if label:
            labels.append(label)
    return ", ".join(labels)


def _sec_headline(date: str, items: str) -> str:
    """`"8-K filed 2026-08-14 — Results of Operations"`, byte-for-byte as the gateway renders it.

    This exact shape matters: `entities.SEC_ITEM_TYPES` matches on those labels to guess the event
    type, and a headline that renders "Results Of Operations" or drops the em dash would silently
    downgrade every earnings 8-K to `other`.
    """
    headline = f"8-K filed {date}"
    labels = _item_labels(items)
    if labels:
        headline += f" — {labels}"
    return headline


def fetch_sec_filings(conn, *, ticker: str, limit: int = DEFAULT_ITEM_LIMIT) -> FetchResult:
    """8-K filings. Official tier — the one tier D-29 entitles us to store a full body for.

    This lane stores no body: SEC's submissions JSON carries filing METADATA, and the filing text
    lives behind a second request per filing. Retrieving it is a deliberate non-goal here (it would
    multiply the budget by the filing count); `documents.py` implements and tests the official-tier
    body path so the entitlement is exercised the day a body-fetching provider is added.
    """
    result = FetchResult()
    agent = contact_user_agent()
    if not agent:
        result.degraded.append(degraded(SEC_EDGAR, REASON_NO_KEY))
        return result

    cik = BUILTIN_CIK.get(ticker.strip().upper())
    if not cik:
        # Never a guess and never an error: the gateway resolves CIKs from a second SEC endpoint,
        # and this lane will not spend a provider call to enable a provider call.
        result.degraded.append(degraded(SEC_EDGAR, REASON_NO_CIK))
        return result

    response = _get(
        conn,
        SEC_EDGAR,
        f"https://data.sec.gov/submissions/CIK{cik}.json",
        headers={"User-Agent": agent},
    )
    if not response.ok:
        result.degraded.extend(response.degraded)
        return result

    try:
        payload = response.json()
        recent = payload["filings"]["recent"]
    except (ValueError, TypeError, KeyError):
        result.degraded.append(degraded(SEC_EDGAR, REASON_ERROR))
        return result

    forms = recent.get("form") or []
    accessions = recent.get("accessionNumber") or []
    documents = recent.get("primaryDocument") or []
    items = recent.get("items") or []
    dates = recent.get("filingDate") or []
    acceptance = recent.get("acceptanceDateTime") or []
    cik_int = cik.lstrip("0")

    dropped = 0
    bodies_remaining = sec_body_fetch_limit()
    for index, form in enumerate(forms):
        if form != "8-K" or len(result.documents) >= limit:
            continue
        date = dates[index] if index < len(dates) else ""
        # acceptanceDateTime is the real wall-clock moment the filing became public; filingDate is a
        # bare date. Prefer the former, fall back to the latter at midnight UTC.
        published_at = normalise_timestamp(
            acceptance[index] if index < len(acceptance) else ""
        ) or normalise_timestamp(date)
        if not published_at:
            dropped += 1
            continue
        accession = (accessions[index] if index < len(accessions) else "").replace("-", "")
        primary = documents[index] if index < len(documents) else ""
        document_url = f"https://www.sec.gov/Archives/edgar/data/{cik_int}/{accession}/{primary}"

        # Phase 2D's optional body. `official` is the ONE tier D-29 entitles us to store a full
        # body for, and `documents.py` enforces that independently — this only decides whether to
        # spend a request retrieving one. With the limit at its default 0 this loop is byte-for-byte
        # the request pattern the collector had before Phase 2.
        body = None
        if primary and bodies_remaining > 0:
            bodies_remaining -= 1
            body_response = _get(conn, SEC_EDGAR, document_url, headers={"User-Agent": agent})
            if body_response.ok:
                # No second cap here. `_get` already truncates at RESPONSE_BYTE_CAP, and D-29's
                # body policy is `documents.storable_body`'s alone — a size constant in this file
                # would be a second, drifting home for a rule that has exactly one (§9.29).
                body = _strip_markup(body_response.text)

        result.documents.append(_document(
            provider=SEC_EDGAR,
            url=document_url,
            title=_sec_headline(date, items[index] if index < len(items) else ""),
            excerpt=_item_labels(items[index] if index < len(items) else ""),
            body=body,
            published_at=published_at,
            # The filer is the subject of its own filing and MUST be first:
            # `entities.resolve_tickers` reads `raw_tickers[0]` as the filer identity for
            # sec-edgar and nothing else in the pipeline can recover that ordering.
            raw_tickers=[ticker.strip().upper()],
        ))
    if dropped:
        result.degraded.append(degraded(SEC_EDGAR, REASON_UNPARSEABLE_TIME))
    return result


# =================================================================================================
# Google News RSS — gateway/research.go::fetchRSSNews
# =================================================================================================


def fetch_rss_news(conn, *, ticker: str, limit: int = DEFAULT_ITEM_LIMIT) -> FetchResult:
    """Google News RSS. Keyless, but gated on `EVENTS_CONTACT_UA` (§9.42, §9.45).

    This provider needs no API key, which made it the ONE provider still opening a socket on a
    `docker compose up` with an empty `.env`. That is measured behaviour, not a hypothesis: before
    this gate, a full pass against an empty environment fetched and stored 60 real articles.

    The gate is a contact address rather than a key because there is no key to require — and it is
    the EVENTS service's own variable, not the gateway's `SEC_USER_AGENT`, because compose hands one
    `.env` to every service and the gateway's copy must stay populated. See `CONTACT_UA_ENV`. No
    contact address configured ⇒ skip with a `degraded` entry, exactly like the five keyed
    providers.
    """
    result = FetchResult()
    agent = contact_user_agent()
    if not agent:
        result.degraded.append(degraded(GOOGLE_NEWS_RSS, REASON_NO_KEY))
        return result

    response = _get(
        conn,
        GOOGLE_NEWS_RSS,
        "https://news.google.com/rss/search",
        params={"q": f"{ticker} stock", "hl": "en-US", "gl": "US", "ceid": "US:en"},
        headers={"User-Agent": agent},
    )
    if not response.ok:
        result.degraded.extend(response.degraded)
        return result

    try:
        feed = ElementTree.fromstring(response.text)
    except ElementTree.ParseError:
        result.degraded.append(degraded(GOOGLE_NEWS_RSS, REASON_ERROR))
        return result

    dropped = 0
    for item in feed.iter("item"):
        if len(result.documents) >= limit:
            break
        published_at = normalise_timestamp(item.findtext("pubDate"))
        if not published_at:
            dropped += 1
            continue
        result.documents.append(_document(
            provider=GOOGLE_NEWS_RSS,
            url=_clean(item.findtext("link")),
            title=_clean(item.findtext("title")),
            excerpt=_clean(item.findtext("description")),
            published_at=published_at,
            # Google News reports no entities at all. An EMPTY list is the honest answer: inferring
            # the ticker from the query we happened to send would be exactly the inference §3.1
            # forbids in this column. `entities.resolve_tickers` recovers the company from the
            # title via its alias table, which is a documented, auditable rule.
            raw_tickers=[],
        ))
    if dropped:
        result.degraded.append(degraded(GOOGLE_NEWS_RSS, REASON_UNPARSEABLE_TIME))
    return result


# =================================================================================================
# Alpha Vantage — gateway/providers.go::fetchEarnings + calendar.go::fetchEarningsCalendar
# =================================================================================================


def _float(value) -> float | None:
    if value in (None, "", "None"):
        return None
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def fetch_alphavantage_earnings(conn, *, ticker: str, limit: int = 8) -> FetchResult:
    """Reported quarterly EPS as documents, and the forward calendar as `scheduled_events` (§9.27).

    Two Alpha Vantage endpoints, two reservations — EARNINGS (a JSON history of what was reported)
    and EARNINGS_CALENDAR (a CSV of what is scheduled). §9.27 names EARNINGS_CALENDAR explicitly as
    a source of `scheduled_events`, so both live here rather than in two half-providers.
    """
    result = FetchResult()
    key = alphavantage_key()
    if not key:
        result.degraded.append(degraded(ALPHAVANTAGE, REASON_NO_KEY))
        return result

    symbol = ticker.strip().upper()
    response = _get(
        conn,
        ALPHAVANTAGE,
        "https://www.alphavantage.co/query",
        params={"function": "EARNINGS", "symbol": symbol, "apikey": key},
    )
    if not response.ok:
        result.degraded.extend(response.degraded)
    else:
        try:
            payload = response.json()
        except (ValueError, TypeError):
            payload = None
        if not isinstance(payload, dict) or payload.get("Note") or payload.get("Information") \
                or payload.get("Error Message"):
            # Alpha Vantage signals a rate limit with HTTP 200 and a "Note" field, exactly as
            # `gateway/providers.go::fetchEarnings` documents.
            result.degraded.append(degraded(ALPHAVANTAGE, REASON_ERROR))
        else:
            dropped = 0
            for quarter in (payload.get("quarterlyEarnings") or [])[:limit]:
                if not isinstance(quarter, dict):
                    continue
                published_at = normalise_timestamp(quarter.get("reportedDate"))
                if not published_at:
                    dropped += 1
                    continue
                reported = _float(quarter.get("reportedEPS"))
                estimated = _float(quarter.get("estimatedEPS"))
                surprise = _float(quarter.get("surprise"))
                fiscal = str(quarter.get("fiscalDateEnding") or "").strip()
                verdict = "beat" if (surprise is not None and surprise >= 0) else "missed"
                if surprise is None:
                    verdict = "reported"
                result.documents.append(_document(
                    provider=ALPHAVANTAGE,
                    url=f"https://www.alphavantage.co/query?function=EARNINGS&symbol={symbol}",
                    # Written so `entities.guess_event_type`'s "earnings" keyword rule types it
                    # `earnings_result` — the phrasing is factual and carries no direction word.
                    title=f"{symbol} quarterly earnings for {fiscal}: reported EPS "
                          f"{reported if reported is not None else 'n/a'} vs estimate "
                          f"{estimated if estimated is not None else 'n/a'} ({verdict})",
                    excerpt=(
                        f"Fiscal period ending {fiscal}. Reported EPS "
                        f"{reported if reported is not None else 'n/a'}, estimated EPS "
                        f"{estimated if estimated is not None else 'n/a'}, surprise "
                        f"{surprise if surprise is not None else 'n/a'}."
                    ),
                    published_at=published_at,
                    raw_tickers=[symbol],
                ))
            if dropped:
                result.degraded.append(degraded(ALPHAVANTAGE, REASON_UNPARSEABLE_TIME))

    result.extend(_fetch_alphavantage_calendar(conn, symbol=symbol))
    return result


def _fetch_alphavantage_calendar(conn, *, symbol: str) -> FetchResult:
    """EARNINGS_CALENDAR (a CSV, not JSON) → `scheduled_events` rows (§9.27)."""
    result = FetchResult()
    response = _get(
        conn,
        ALPHAVANTAGE,
        "https://www.alphavantage.co/query",
        params={
            "function": "EARNINGS_CALENDAR",
            "symbol": symbol,
            "horizon": "3month",
            "apikey": alphavantage_key(),
        },
    )
    if not response.ok:
        result.degraded.extend(response.degraded)
        return result
    text = response.text.strip()
    if text.startswith("{"):
        # A JSON body where a CSV was promised is Alpha Vantage's rate-limit signal.
        result.degraded.append(degraded(ALPHAVANTAGE, REASON_ERROR))
        return result
    try:
        rows = list(csv.DictReader(io.StringIO(text)))
    except csv.Error:
        result.degraded.append(degraded(ALPHAVANTAGE, REASON_ERROR))
        return result
    for row in rows:
        scheduled_at = normalise_timestamp((row.get("reportDate") or "").strip())
        row_symbol = (row.get("symbol") or "").strip().upper()
        if not scheduled_at or row_symbol != symbol:
            continue
        company = _clean(row.get("name")) or symbol
        fiscal_period = _clean(row.get("fiscalDateEnding"))
        result.scheduled.append(_scheduled(
            kind="earnings",
            ticker=symbol,
            series=None,
            scheduled_at=scheduled_at,
            # Alpha Vantage's calendar is an estimate until the company confirms it, and the CSV
            # carries no confirmation flag. Claiming confirmed=1 would be inventing a fact.
            confirmed=False,
            source=ALPHAVANTAGE,
            title=f"{company} earnings",
            description=(
                "Quarterly results can update reported fundamentals and management's outlook; "
                "the date remains tentative until the company confirms it."
            ),
            importance="high",
            status="tentative",
            source_url="https://www.alphavantage.co/documentation/#earnings-calendar",
            expected=_number(row.get("estimate")),
            unit=_clean(row.get("currency")),
            occurrence_key=(
                f"earnings|{symbol}|{fiscal_period}" if fiscal_period else None
            ),
        ))
    return result


# =================================================================================================
# Federal Reserve — direct official FOMC calendar
# =================================================================================================


_FOMC_YEAR = re.compile(r"\b(20\d{2})\s+FOMC Meetings\b", re.IGNORECASE)
_FOMC_DAYS = re.compile(r"\b(\d{1,2})(?:\s*-\s*(\d{1,2}))?\b")
_MONTH_NUMBER = {
    "jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3,
    "apr": 4, "april": 4, "may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7,
    "aug": 8, "august": 8, "sep": 9, "sept": 9, "september": 9, "oct": 10,
    "october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12,
}


class _FOMCCalendarParser(HTMLParser):
    """Extract only the year/month/date cells from the Fed's meeting-calendar HTML.

    The page contains statement, minutes and press-conference links inside each row. None of that
    prose participates in occurrence identity, so the parser deliberately ignores it. Class-name
    matching is resilient to the Fed's shaded-row classes and Bootstrap layout changes.
    """

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.year: int | None = None
        self.month = ""
        self.rows: list[tuple[int, str, str]] = []
        self._capture: str | None = None
        self._capture_depth = 0
        self._parts: list[str] = []

    def handle_starttag(self, tag: str, attrs) -> None:
        classes = set(dict(attrs).get("class", "").split())
        field = None
        if "fomc-meeting__month" in classes:
            field = "month"
        elif "fomc-meeting__date" in classes:
            field = "date"
        if field:
            self._capture = field
            self._capture_depth = 1
            self._parts = []
        elif self._capture:
            self._capture_depth += 1

    def handle_endtag(self, tag: str) -> None:
        if not self._capture:
            return
        self._capture_depth -= 1
        if self._capture_depth:
            return
        value = _clean(" ".join(self._parts))
        if self._capture == "month":
            self.month = value
        elif self.year is not None and self.month and value:
            self.rows.append((self.year, self.month, value))
        self._capture = None
        self._parts = []

    def handle_data(self, data: str) -> None:
        match = _FOMC_YEAR.search(data)
        if match:
            self.year = int(match.group(1))
        if self._capture:
            self._parts.append(data)


def _fomc_decision_date(year: int, month_text: str, date_text: str) -> tuple[str, bool] | None:
    """Return the decision-day date and whether the row carries an SEP asterisk.

    FOMC rows describe meetings as ranges. The policy statement is released on the final day, so
    `Apr/May 30-1` becomes May 1. Notation votes are distinct actions, not regular meetings, and
    are intentionally excluded from this calendar series.
    """
    if "notation vote" in date_text.casefold():
        return None
    days = _FOMC_DAYS.search(date_text)
    if not days:
        return None
    month_parts = [part.strip().casefold() for part in month_text.split("/") if part.strip()]
    if not month_parts:
        return None
    start_month = _MONTH_NUMBER.get(month_parts[0])
    end_month = _MONTH_NUMBER.get(month_parts[-1])
    if start_month is None or end_month is None:
        return None
    day = int(days.group(2) or days.group(1))
    decision_year = year + (1 if end_month < start_month else 0)
    try:
        decision = datetime(decision_year, end_month, day)
    except ValueError:
        return None
    return decision.strftime("%Y-%m-%d"), "*" in date_text


def parse_fomc_calendar(html: str) -> list[dict]:
    """Parse regular FOMC meetings with a stable sequence identity inside each calendar year."""
    parser = _FOMCCalendarParser()
    parser.feed(html or "")
    sequence: dict[int, int] = {}
    meetings: list[dict] = []
    for year, month_text, date_text in parser.rows:
        parsed = _fomc_decision_date(year, month_text, date_text)
        if parsed is None:
            continue
        decision_date, has_projections = parsed
        sequence[year] = sequence.get(year, 0) + 1
        meetings.append({
            "date": decision_date,
            "has_projections": has_projections,
            # The regular-meeting sequence survives a date correction or reschedule. It is only
            # assigned after notation votes are removed, so an inserted notation vote cannot move
            # the identity of every later policy decision.
            "occurrence_key": f"fomc|{year}|regular-{sequence[year]:02d}",
        })
    return sorted(meetings, key=lambda item: (item["date"], item["occurrence_key"]))


def fetch_federal_reserve_calendar(
    conn, *, from_date: str, to_date: str, now: datetime | None = None
) -> FetchResult:
    """Official FOMC decision dates, fetched only when corpus ingestion is explicitly enabled.

    The Fed says future dates are tentative until confirmed at the immediately preceding meeting.
    Accordingly, completed meetings and only the first upcoming regular meeting are marked
    confirmed; later future meetings remain tentative even though they appear on the official
    planning calendar.
    """
    result = FetchResult()
    if not federal_reserve_enabled():
        result.degraded.append(degraded(FEDERAL_RESERVE, REASON_DISABLED))
        return result

    contact = contact_user_agent()

    response = _get(
        conn,
        FEDERAL_RESERVE,
        FOMC_CALENDAR_URL,
        headers={"User-Agent": contact} if contact else None,
    )
    if not response.ok:
        result.degraded.extend(response.degraded)
        return result

    meetings = parse_fomc_calendar(response.text)
    if not meetings:
        result.degraded.append(degraded(FEDERAL_RESERVE, REASON_ERROR))
        return result

    moment = (now or datetime.now(timezone.utc)).astimezone(timezone.utc)
    upcoming = [meeting["date"] for meeting in meetings if meeting["date"] > moment.date().isoformat()]
    next_date = min(upcoming) if upcoming else None
    dropped = 0
    for meeting in meetings:
        date = meeting["date"]
        if not (from_date <= date <= to_date):
            continue
        scheduled_at = eastern_release_timestamp(date, FOMC_RELEASE_CLOCK)
        if not scheduled_at:
            dropped += 1
            continue
        confirmed = date <= moment.date().isoformat() or date == next_date
        description = MACRO_RELEVANCE["FOMC"]
        if meeting["has_projections"]:
            description += " This meeting includes the Summary of Economic Projections."
        result.scheduled.append(_scheduled(
            kind="central_bank",
            ticker=None,
            series="FOMC",
            scheduled_at=scheduled_at,
            confirmed=confirmed,
            source=FEDERAL_RESERVE,
            title="FOMC policy decision",
            description=description,
            importance="high",
            status="confirmed" if confirmed else "tentative",
            source_url=FOMC_CALENDAR_URL,
            timezone_name="America/New_York",
            # Regular FOMC statements are listed at 2:00 p.m. ET on the Fed's event calendar.
            local_time=FOMC_RELEASE_CLOCK,
            occurrence_key=meeting["occurrence_key"],
        ))
    if dropped:
        result.degraded.append(degraded(FEDERAL_RESERVE, REASON_UNPARSEABLE_TIME))
    return result


# =================================================================================================
# FRED — gateway/calendar.go::fetchFREDReleases
# =================================================================================================


def fetch_fred_releases(
    conn, *, from_date: str, to_date: str, now: datetime | None = None
) -> FetchResult:
    """Allowlisted macro release dates. Official tier (Federal Reserve data).

    Produces BOTH a document per release (so `events.assemble` can build a `macro_release` event
    from it) and a `scheduled_events` row (§9.27, so `GET /calendar` can answer "what was scheduled
    as of this cutoff"). The same occurrence in two tables is deliberate: one is evidence, one is a
    calendar entry, and the retention sweep may delete the first without touching the second.
    """
    result = FetchResult()
    key = fred_key()
    if not key:
        result.degraded.append(degraded(FRED, REASON_NO_KEY))
        return result

    response = _get(
        conn,
        FRED,
        "https://api.stlouisfed.org/fred/releases/dates",
        params={
            "api_key": key,
            "file_type": "json",
            "sort_order": "asc",
            # Without this, FRED returns only releases that have ALREADY published — no good for a
            # forward calendar. `gateway/calendar.go` documents the same flag.
            "include_release_dates_with_no_data": "true",
            "realtime_start": from_date,
            "realtime_end": to_date,
        },
    )
    if not response.ok:
        result.degraded.extend(response.degraded)
        return result

    try:
        payload = response.json()
    except (ValueError, TypeError):
        result.degraded.append(degraded(FRED, REASON_ERROR))
        return result
    if not isinstance(payload, dict) or payload.get("error_message"):
        result.degraded.append(degraded(FRED, REASON_ERROR))
        return result

    dropped = 0
    for entry in payload.get("release_dates") or []:
        if not isinstance(entry, dict):
            continue
        meta = FRED_RELEASES.get(entry.get("release_id"))
        if not meta:
            continue
        name, series, importance, release_clock = meta
        date = str(entry.get("date") or "").strip()
        if not (from_date <= date <= to_date):
            continue
        published_at = eastern_release_timestamp(date, release_clock)
        if not published_at:
            dropped += 1
            continue
        source_url = f"https://fred.stlouisfed.org/releases/{entry.get('release_id')}"
        document = _document(
            provider=FRED,
            url=source_url,
            title=f"{name} release scheduled for {date}",
            excerpt=f"FRED release {entry.get('release_id')} ({series}) dated {date}.",
            published_at=published_at,
            # A macro release is market-wide: it has no subject company, and naming one would be
            # an inference. `entities.primary_ticker` returns None, which is the correct answer.
            raw_tickers=[],
        )
        document["macro_key"] = macro_key(series, published_at)
        result.documents.append(document)
        # §9.41.4. `releases/dates` is a CALENDAR endpoint: it carries the date and nothing else, so
        # the row is written with `actual`, `expected` and `previous` all NULL and no expectation
        # source. That is honest — the release is known to exist and its numbers are not known here.
        # An FMP pass over the same series and date fills them in through the natural key.
        result.macro.append(_macro(series=series, release_at=published_at))
        result.scheduled.append(_scheduled(
            kind="macro_release",
            ticker=None,
            series=series,
            scheduled_at=published_at,
            # FRED publishes its release calendar; a date on it is a confirmed schedule.
            confirmed=True,
            source=FRED,
            title=name,
            description=macro_relevance(series),
            importance=importance,
            status="confirmed",
            source_url=source_url,
            timezone_name="America/New_York",
            local_time=release_clock,
        ))
    if dropped:
        result.degraded.append(degraded(FRED, REASON_UNPARSEABLE_TIME))
    return result


# =================================================================================================
# FMP — gateway/calendar.go::fetchFMPCalendar
# =================================================================================================


def fetch_fmp_calendar(
    conn, *, from_date: str, to_date: str, now: datetime | None = None
) -> FetchResult:
    """US macro events with previous/consensus/actual. Paid plan; fails soft to nothing."""
    result = FetchResult()
    key = fmp_key()
    if not key:
        result.degraded.append(degraded(FMP, REASON_NO_KEY))
        return result

    response = _get(
        conn,
        FMP,
        "https://financialmodelingprep.com/stable/economic-calendar",
        params={"from": from_date, "to": to_date, "apikey": key},
    )
    if not response.ok:
        result.degraded.extend(response.degraded)
        return result

    text = response.text.strip()
    if not text.startswith("["):
        # FMP signals an error or a paywall with a JSON OBJECT; success is a JSON array.
        result.degraded.append(degraded(FMP, REASON_ERROR))
        return result
    try:
        payload = response.json()
    except (ValueError, TypeError):
        result.degraded.append(degraded(FMP, REASON_ERROR))
        return result

    dropped = 0
    for entry in payload:
        if not isinstance(entry, dict):
            continue
        if str(entry.get("country") or "").strip().upper() != "US":
            continue
        raw_date = str(entry.get("date") or "").strip()
        # FMP's timestamp is preserved according to the provider contract and normalised to UTC.
        # It is not relabelled as US/Eastern merely because the row's country is US.
        published_at = normalise_timestamp(raw_date)
        if not published_at:
            dropped += 1
            continue
        if not (from_date <= published_at[:10] <= to_date):
            continue
        name = _clean(entry.get("event"))
        series = canonical_macro_series(name)
        source_url = "https://financialmodelingprep.com/economic-calendar"
        expected = _number(entry.get("estimate"))
        actual = _number(entry.get("actual"))
        previous = _number(entry.get("previous"))
        unit = _clean(entry.get("unit"))
        importance = {
            "high": "high", "medium": "medium", "moderate": "medium", "low": "low",
        }.get(_clean(entry.get("impact")).casefold(), "low")
        document = _document(
            provider=FMP,
            url=source_url,
            title=f"{name} scheduled for {published_at[:10]}",
            excerpt=(
                f"{name}. Previous {entry.get('previous')}, estimate {entry.get('estimate')}, "
                f"actual {entry.get('actual')} {unit}".strip()
            ),
            published_at=published_at,
            raw_tickers=[],
        )
        if series:
            document["macro_key"] = macro_key(series, published_at)
        result.documents.append(document)
        result.scheduled.append(_scheduled(
            kind="macro_release",
            ticker=None,
            series=series or None,
            scheduled_at=published_at,
            confirmed=True,
            source=FMP,
            title=name or series,
            description=macro_relevance(series),
            importance=importance,
            status="released" if actual is not None else "confirmed",
            source_url=source_url,
            timezone_name="UTC",
            local_time="",
            previous=previous,
            expected=expected,
            actual=actual,
            unit=unit,
        ))
        # §9.41.4. FMP's economic calendar IS the authoritative structured source for these three
        # numbers — they are taken from the response, never re-parsed out of the excerpt above.
        # `expectation_source` is `fmp` only when an estimate actually came back; `none` would claim
        # we looked and found no consensus, which is a different fact from not having one.
        if series:
            result.macro.append(_macro(
                series=series,
                release_at=published_at,
                actual=actual,
                expected=expected,
                previous=previous,
                unit=unit,
                expectation_source=FMP_EXPECTATION_SOURCE if expected is not None else None,
            ))
    if dropped:
        result.degraded.append(degraded(FMP, REASON_UNPARSEABLE_TIME))
    return result


# =================================================================================================
# Phase 2 — the official statistical agencies (BLS, BEA)
# =================================================================================================
#
# WHY THESE EXIST WHEN FRED ALREADY WORKS
# ---------------------------------------
# FRED redistributes these releases; BLS and BEA PUBLISH them. For a calendar date that difference
# is the whole point: the agency's own schedule page is the authority on its own dates, it is
# keyless, and it carries the REFERENCE PERIOD ("July 2026") that FRED's `releases/dates` does not.
# `entities.scheduled_precedence` ranks them above every redistributor accordingly.
#
# WHAT THEY DO NOT DO
# -------------------
# They emit a DATE and a reference period and nothing else. A schedule page carries no previous, no
# consensus and no actual, so those stay NULL rather than being filled from somewhere else and
# attributed to the agency. `surprise` is never emitted by any provider — `macro.compute_surprise`
# owns it, behind its timing guards.
#
# No model is involved at any point. A date that cannot be parsed unambiguously is DROPPED with a
# labelled degradation; it is never approximated, and Qwen is never asked what a date might be.


def macro_occurrence_key(series: str, period: str) -> str:
    """The canonical identity of one macro release: its series and its REFERENCE PERIOD.

    This is what makes two providers converge. `bls|CPI|2026-07` and `bea|GDP|2026Q2` name the
    release itself, not the day it happens to be scheduled for — so an agency that moves a date
    updates the existing row and appends a revision, instead of creating a second calendar entry
    for the same print.

    A provider that cannot honestly supply a reference period must NOT call this. `_scheduled`'s
    fallback key (kind|ticker|series|scheduled_at) is the conservative answer for those, and
    `store_scheduled_events` adopts such a row when a higher-precedence source later names it.
    """
    return f"macro|{canonical_macro_series(series)}|{period}"


class _ScheduleTableParser(HTMLParser):
    """Collect every HTML table row as a list of cell texts, plus the first link in each row.

    Content-driven rather than position-driven on purpose. Both agencies periodically restyle their
    schedule pages, and a parser that hard-codes "column 2 is the time" silently starts reading the
    wrong column when a column is inserted. This hands the caller the whole row and lets it find
    the date, the time and the release name by their SHAPE — so a reordered table still parses, and
    an unrecognisable row is dropped rather than misread.
    """

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.rows: list[tuple[list[str], str]] = []
        self._cells: list[str] | None = None
        self._buffer: list[str] = []
        self._href = ""
        self._in_cell = False

    def handle_starttag(self, tag, attrs):
        if tag == "tr":
            self._cells, self._href = [], ""
        elif tag in ("td", "th") and self._cells is not None:
            self._in_cell, self._buffer = True, []
        elif tag == "a" and self._cells is not None and not self._href:
            for name, value in attrs:
                if name == "href" and value:
                    self._href = value
                    break

    def handle_endtag(self, tag):
        if tag in ("td", "th") and self._in_cell and self._cells is not None:
            self._cells.append(_clean("".join(self._buffer)))
            self._in_cell, self._buffer = False, []
        elif tag == "tr" and self._cells is not None:
            if any(cell for cell in self._cells):
                self.rows.append((self._cells, self._href))
            self._cells, self._href = None, ""

    def handle_data(self, data):
        if self._in_cell:
            self._buffer.append(data)


_MONTH_NAMES = {
    "jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
    "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}
# "August 12, 2026" / "Aug. 12, 2026" / "Aug 12 2026"
_LONG_DATE = re.compile(
    r"\b([A-Z][a-z]{2,8})\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\b"
)
# BEA publishes the schedule year once in the table header, then uses month/day cells in its rows.
_PARTIAL_DATE = re.compile(r"\b([A-Z][a-z]{2,8})\.?\s+(\d{1,2})(?:st|nd|rd|th)?\b")
_PAGE_YEAR = re.compile(r"\bYear\s+(\d{4})\b", re.IGNORECASE)
# "08/12/2026"
_SLASH_DATE = re.compile(r"\b(\d{1,2})/(\d{1,2})/(\d{4})\b")
# "08:30 AM" / "8:30 a.m." / "10:00 AM ET"
_CLOCK = re.compile(r"\b(\d{1,2}):(\d{2})\s*([AaPp])\.?[Mm]\.?")
# A reference period a release covers: "July 2026", "2026 Q2", "Q2 2026", "Second Quarter 2026".
_PERIOD_MONTH = re.compile(r"\b([A-Z][a-z]{2,8})\.?\s+(\d{4})\b")
_PERIOD_QUARTER = re.compile(r"\b(?:Q([1-4])\s*(\d{4})|(\d{4})\s*Q([1-4]))\b", re.IGNORECASE)
_PERIOD_QUARTER_WORDS = re.compile(
    r"\b(first|second|third|fourth)\s+quarter[, ]+(\d{4})\b", re.IGNORECASE
)
_PERIOD_QUARTER_ORDINAL = re.compile(
    r"\b([1-4])(?:st|nd|rd|th)\s+quarter[, ]+(\d{4})\b", re.IGNORECASE
)
_QUARTER_WORD = {"first": 1, "second": 2, "third": 3, "fourth": 4}


def parse_schedule_date(text: str, *, default_year: int | None = None) -> str | None:
    """`YYYY-MM-DD` from an agency schedule cell, or None. Never a guess.

    Full dates are accepted directly. A month/day spelling is accepted only with the year published
    in the same schedule table's header. Anything else — a relative phrase, a range, a footnote
    marker — yields None and the row is dropped, which is the only safe direction: a wrong calendar
    date is worse than a missing one.
    """
    value = _clean(text)
    match = _LONG_DATE.search(value)
    if match:
        month = _MONTH_NAMES.get(match.group(1)[:3].casefold())
        if month:
            try:
                return date(int(match.group(3)), month, int(match.group(2))).isoformat()
            except ValueError:
                return None
    match = _SLASH_DATE.search(value)
    if match:
        try:
            return date(int(match.group(3)), int(match.group(1)), int(match.group(2))).isoformat()
        except ValueError:
            return None
    if default_year:
        match = _PARTIAL_DATE.search(value)
        if match:
            month = _MONTH_NAMES.get(match.group(1)[:3].casefold())
            if month:
                try:
                    return date(int(default_year), month, int(match.group(2))).isoformat()
                except ValueError:
                    return None
    if _DATE_ONLY.match(value):
        return value
    return None


def parse_schedule_clock(text: str) -> str | None:
    """`HH:MM` (24h) from an agency schedule cell, or None."""
    match = _CLOCK.search(_clean(text))
    if not match:
        return None
    hour, minute, meridiem = int(match.group(1)), int(match.group(2)), match.group(3).casefold()
    if not (1 <= hour <= 12 and 0 <= minute <= 59):
        return None
    if meridiem == "p" and hour != 12:
        hour += 12
    elif meridiem == "a" and hour == 12:
        hour = 0
    return f"{hour:02d}:{minute:02d}"


def parse_reference_period(text: str) -> str | None:
    """The period a release COVERS, as `YYYY-MM` or `YYYYQn`. None when it is not stated.

    This is the release's identity (see `macro_occurrence_key`), so it is read only from what the
    agency actually printed. When the page does not state one, the caller falls back to the
    conservative date-based key rather than inventing a period from the release date — a July CPI
    print released in August would otherwise be filed under August.
    """
    value = _clean(text)
    match = _PERIOD_QUARTER_WORDS.search(value)
    if match:
        return f"{match.group(2)}Q{_QUARTER_WORD[match.group(1).casefold()]}"
    match = _PERIOD_QUARTER_ORDINAL.search(value)
    if match:
        return f"{match.group(2)}Q{match.group(1)}"
    match = _PERIOD_QUARTER.search(value)
    if match:
        if match.group(1):
            return f"{match.group(2)}Q{match.group(1)}"
        return f"{match.group(3)}Q{match.group(4)}"
    match = _PERIOD_MONTH.search(value)
    if match:
        month = _MONTH_NAMES.get(match.group(1)[:3].casefold())
        if month:
            return f"{int(match.group(2)):04d}-{month:02d}"
    return None


#: The BLS releases this collector recognises, matched against the release-name cell. Ordered:
#: the first match wins, so a more specific name is listed before the family it belongs to.
#: `(needles, canonical series, importance, fallback clock)`.
#:
#: The fallback clock is used ONLY when the schedule row states no time of its own. Both are the
#: agencies' long-published standard release hours; a row that states a different time overrides it.
BLS_RELEASES: tuple[tuple[tuple[str, ...], str, str, str], ...] = (
    (("employment situation",), "NFP", "high", "08:30"),
    (("consumer price index",), "CPI", "high", "08:30"),
    (("producer price index",), "PPI", "medium", "08:30"),
    (("employment cost index",), "ECI", "medium", "08:30"),
)

#: The BEA releases this collector recognises. Same shape and the same rules.
BEA_RELEASES: tuple[tuple[tuple[str, ...], str, str, str], ...] = (
    (("personal income and outlays", "personal income & outlays"), "PCE", "high", "08:30"),
    (("gross domestic product", "gdp"), "GDP", "high", "08:30"),
)


def _match_release(name: str, table) -> tuple[str, str, str] | None:
    lowered = _clean(name).casefold()
    for needles, series, importance, clock in table:
        if any(needle in lowered for needle in needles):
            return series, importance, clock
    return None


def _absolute_url(href: str, base: str) -> str:
    href = _clean(href)
    if not href:
        return base
    if href.startswith(("http://", "https://")):
        return href
    origin = "/".join(base.split("/")[:3])
    return origin + ("" if href.startswith("/") else "/") + href


def _agency_schedule(
    conn,
    *,
    provider: str,
    url: str,
    table,
    from_date: str,
    to_date: str,
) -> FetchResult:
    """One agency schedule page → canonical `scheduled_events` rows. Shared by BLS and BEA.

    Both agencies publish the same shape of artefact (a table of "when, at what time, which
    release, covering which period"), so they share one parse and one set of rules:

    * a row whose release name is not in `table` is IGNORED — the collector has a closed vocabulary
      and does not invent a series for an unrecognised name;
    * a row whose date will not parse unambiguously is DROPPED with a labelled degradation;
    * the reference period gives the row its stable identity when the agency states one;
    * values (previous/expected/actual) are absent, because a schedule page has none.
    """
    result = FetchResult()
    contact = contact_user_agent()

    response = _get(
        conn, provider, url, headers={"User-Agent": contact} if contact else None,
    )
    if not response.ok:
        result.degraded.extend(response.degraded)
        return result

    parser = _ScheduleTableParser()
    try:
        parser.feed(response.text or "")
    except Exception:  # noqa: BLE001 — a malformed page is a degradation, never an exception
        result.degraded.append(degraded(provider, REASON_ERROR))
        return result

    page_year = None
    for cells, _href in parser.rows:
        match = _PAGE_YEAR.search(" ".join(cells))
        if match:
            page_year = int(match.group(1))
            break

    dropped = 0
    matched = 0
    for cells, href in parser.rows:
        joined = " ".join(cells)
        release = _match_release(joined, table)
        if not release:
            continue
        series, importance, fallback_clock = release
        matched += 1

        release_date = None
        for cell in cells:
            release_date = parse_schedule_date(cell, default_year=page_year)
            if release_date:
                break
        if not release_date or not (from_date <= release_date <= to_date):
            if not release_date:
                dropped += 1
            continue

        clock = None
        for cell in cells:
            clock = parse_schedule_clock(cell)
            if clock:
                break
        clock = clock or fallback_clock

        scheduled_at = eastern_release_timestamp(release_date, clock)
        if not scheduled_at:
            dropped += 1
            continue

        # The period is looked for in every cell EXCEPT the one that produced the release date, so
        # "Aug. 12, 2026" cannot be misread as the period "August 2026".
        period = None
        for cell in cells:
            if parse_schedule_date(cell, default_year=page_year):
                continue
            period = parse_reference_period(cell)
            if period:
                break

        occurrence_key = macro_occurrence_key(series, period) if period else None
        described = macro_relevance(series)
        if period:
            described += f" This release covers {period}."

        result.scheduled.append(_scheduled(
            kind="macro_release",
            ticker=None,
            series=series,
            scheduled_at=scheduled_at,
            # An agency's own published schedule is a confirmed date by definition.
            confirmed=True,
            source=provider,
            title=_release_title(cells, series),
            description=described,
            importance=importance,
            status="confirmed",
            source_url=_absolute_url(href, url),
            timezone_name="America/New_York",
            local_time=clock,
            occurrence_key=occurrence_key,
        ))

    if dropped:
        result.degraded.append(degraded(provider, REASON_UNPARSEABLE_TIME))
    if matched == 0:
        # The page loaded and contained none of the releases we recognise. That is a real
        # degradation — most likely the page was restyled — and must not read as "nothing is
        # scheduled" (§9.44: absence of a result is not a result).
        result.degraded.append(degraded(provider, REASON_ERROR))
    return result


def _release_title(cells, series: str) -> str:
    """The agency's own release name, taken from the cell that carried it. Never composed."""
    for cell in cells:
        if _match_release(cell, BLS_RELEASES + BEA_RELEASES):
            return _clean(cell)[:160]
    return series


def fetch_bls_schedule(
    conn, *, from_date: str, to_date: str, now: datetime | None = None
) -> FetchResult:
    """The Bureau of Labor Statistics' own release schedule (CPI, PPI, Employment Situation, ECI).

    Keyless and therefore gated: `BLS_ENABLED` is read with no default, so an empty `.env` opens no
    socket (§9.42). One page per calendar year, so a window that spans a year boundary fetches
    both — and each page costs one budget reservation, like every other request in this module.
    """
    result = FetchResult()
    if not bls_enabled():
        result.degraded.append(degraded(BLS, REASON_DISABLED))
        return result

    for year in _schedule_years(from_date, to_date):
        result.extend(_agency_schedule(
            conn,
            provider=BLS,
            url=BLS_SCHEDULE_URL.format(year=year),
            table=BLS_RELEASES,
            from_date=from_date,
            to_date=to_date,
        ))
    return result


def fetch_bea_schedule(
    conn, *, from_date: str, to_date: str, now: datetime | None = None
) -> FetchResult:
    """The Bureau of Economic Analysis' own release schedule (GDP, Personal Income and Outlays).

    One page covering the current schedule, so one reservation per pass. Same gate, same rules.
    """
    result = FetchResult()
    if not bea_enabled():
        result.degraded.append(degraded(BEA, REASON_DISABLED))
        return result

    return _agency_schedule(
        conn,
        provider=BEA,
        url=BEA_SCHEDULE_URL,
        table=BEA_RELEASES,
        from_date=from_date,
        to_date=to_date,
    )


def _schedule_years(from_date: str, to_date: str) -> list[int]:
    """The calendar years a window touches, capped so a malformed window cannot fan out."""
    try:
        start, end = int(from_date[:4]), int(to_date[:4])
    except (TypeError, ValueError):
        return []
    if end < start:
        return []
    return list(range(start, min(end, start + 1) + 1))


# =================================================================================================
# Phase 2C — company investor relations
# =================================================================================================
#
# The company is the authority on its own earnings date. An aggregator's date is an estimate until
# the company says otherwise, which is why `_fetch_alphavantage_calendar` writes `confirmed=0,
# status="tentative"` and why `entities.scheduled_precedence` ranks `company-ir` above it. This
# collector is the thing that turns the estimate into a confirmed date — and `store_scheduled_events`
# keeps the prior date in append-only history when it does.
#
# Coverage is DATA, in `ir_registry.py`. A company with no configured feed is reported as missing
# coverage; it is never served an aggregator date dressed up as a confirmation.


def _ir_local_timestamp(day: str, clock: str, zone: str) -> str | None:
    """A company-local date and time converted to canonical UTC. None on anything unparseable."""
    try:
        local = datetime.strptime(f"{day} {clock}", "%Y-%m-%d %H:%M").replace(
            tzinfo=ZoneInfo(zone)
        )
    except (TypeError, ValueError, ZoneInfoNotFoundError):
        return None
    return local.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


_ICS_DATETIME = re.compile(r"^(\d{8})(?:T(\d{2})(\d{2})(\d{2})(Z)?)?$")


def parse_ics_events(text: str) -> list[dict]:
    """VEVENTs as `{title, startsAt, allDay, url}`. Unfolds continuation lines; ignores the rest.

    Deliberately small: this reads DTSTART, SUMMARY and URL and nothing else. Recurrence rules,
    alarms and attendees have no meaning for a company earnings date, and a partial RRULE
    implementation would silently invent occurrences.
    """
    unfolded: list[str] = []
    for raw in (text or "").splitlines():
        if raw[:1] in (" ", "\t") and unfolded:
            unfolded[-1] += raw[1:]
        else:
            unfolded.append(raw)

    events: list[dict] = []
    current: dict | None = None
    for line in unfolded:
        stripped = line.strip()
        if stripped.upper() == "BEGIN:VEVENT":
            current = {"title": "", "startsAt": "", "allDay": False, "url": ""}
            continue
        if stripped.upper() == "END:VEVENT":
            if current and current["title"] and current["startsAt"]:
                events.append(current)
            current = None
            continue
        if current is None or ":" not in stripped:
            continue
        name, _, value = stripped.partition(":")
        key = name.split(";")[0].strip().upper()
        if key == "SUMMARY":
            current["title"] = _clean(value)
        elif key == "URL":
            current["url"] = _clean(value)
        elif key == "DTSTART":
            match = _ICS_DATETIME.match(value.strip())
            if not match:
                continue
            day = match.group(1)
            current["startsAt"] = f"{day[:4]}-{day[4:6]}-{day[6:]}"
            if match.group(2):
                current["clock"] = f"{match.group(2)}:{match.group(3)}"
                current["utc"] = bool(match.group(5))
            else:
                current["allDay"] = True
    return events


_ANNOUNCED_EVENT_DATE = re.compile(
    r"\b(?:will\s+host\s+(?:a\s+)?conference\s+call\s+on|conference\s+call\s+on)\s+"
    r"(?:Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday)?,?\s*"
    r"([A-Z][a-z]{2,8})\.?\s+(\d{1,2})(?:st|nd|rd|th)?(?:,?\s+(\d{4}))?\b",
    re.IGNORECASE,
)


def _announcement_day(description: str, published_at: str) -> str | None:
    """Date explicitly attached to a conference-call announcement, bounded by publication time."""
    published = normalise_timestamp(published_at)
    match = _ANNOUNCED_EVENT_DATE.search(_clean(description))
    if not published or not match:
        return None
    published_day = datetime.strptime(published[:10], "%Y-%m-%d").date()
    month = _MONTH_NAMES.get(match.group(1)[:3].casefold())
    if not month:
        return None
    year = int(match.group(3) or published_day.year)
    try:
        candidate = date(year, month, int(match.group(2)))
        if not match.group(3) and candidate < published_day:
            candidate = date(year + 1, month, int(match.group(2)))
    except ValueError:
        return None
    delta = (candidate - published_day).days
    return candidate.isoformat() if 0 <= delta <= 180 else None


def parse_feed_events(text: str, *, announcement_dates: bool = False) -> list[dict]:
    """RSS 2.0 `<item>` or Atom `<entry>` as `{title, startsAt, clock, url}`.

    IR feeds put the event's date in whichever element their platform chose, so several are
    accepted — but only elements that hold a DATE. The publication timestamp of the feed entry is
    NOT one of them: when an IR site publishes an announcement on Monday about an event in three
    weeks, reading `pubDate` as the event date is off by three weeks, and it is the kind of error
    that looks completely plausible on the screen.
    """
    try:
        root = ElementTree.fromstring(text or "")
    except ElementTree.ParseError:
        return []

    def _text(node, *names) -> str:
        for name in names:
            for child in node:
                tag = child.tag.split("}")[-1].casefold()
                if tag == name:
                    return _clean(child.text or "")
        return ""

    def _link(node) -> str:
        for child in node:
            if child.tag.split("}")[-1].casefold() != "link":
                continue
            href = (child.attrib.get("href") or "").strip()
            return _clean(href or (child.text or ""))
        return ""

    out: list[dict] = []
    for node in root.iter():
        tag = node.tag.split("}")[-1].casefold()
        if tag not in ("item", "entry"):
            continue
        title = _text(node, "title", "summary")
        # Event-date elements only. `pubDate`/`updated` describe the ANNOUNCEMENT, not the event.
        raw_date = _text(node, "eventdate", "startdate", "start", "dtstart", "date")
        announced_day = None
        if not raw_date and announcement_dates:
            announced_day = _announcement_day(
                _text(node, "description", "summary"), _text(node, "pubdate", "published")
            )
        if not title or (not raw_date and not announced_day):
            continue
        day = announced_day or parse_schedule_date(raw_date) or (
            (normalise_timestamp(raw_date) or "")[:10] or None
        )
        if not day:
            continue
        out.append({
            "title": title,
            "startsAt": day,
            "clock": parse_schedule_clock(raw_date) or "",
            "url": _link(node) or _text(node, "link", "guid"),
        })
    return out


def fetch_company_ir(conn, *, ticker: str, limit: int = DEFAULT_ITEM_LIMIT) -> FetchResult:
    """The company's OWN investor-relations schedule. Official, keyless, and gated.

    Three refusals, all of them labelled rather than silent:

    * `COMPANY_IR_ENABLED` unset ⇒ `company-ir:disabled`. No default (§9.42).
    * no contact address (`EVENTS_CONTACT_UA`) ⇒ `company-ir:no-key`, exactly as SEC EDGAR and
      Google News RSS behave. This is a keyless provider writing to the persistent cross-user
      corpus, which is the posture D-29 requires an operator to opt into.
    * no registry entry for the ticker ⇒ `company-ir:no-coverage`. This is the honest answer the
      product surfaces; it is never replaced by an aggregator date relabelled as confirmed.
    """
    result = FetchResult()
    if not company_ir_enabled():
        result.degraded.append(degraded(COMPANY_IR, REASON_DISABLED))
        return result

    contact = contact_user_agent()
    if not contact:
        result.degraded.append(degraded(COMPANY_IR, REASON_NO_KEY))
        return result

    symbol = (ticker or "").strip().upper()
    entry = ir_registry.entry_for(symbol)
    if not entry:
        result.degraded.append(degraded(COMPANY_IR, REASON_NO_COVERAGE))
        return result

    response = _get(conn, COMPANY_IR, entry["feedUrl"], headers={"User-Agent": contact})
    if not response.ok:
        result.degraded.extend(response.degraded)
        return result

    if entry["feedKind"] == ir_registry.FEED_ICS:
        entries = parse_ics_events(response.text)
    else:
        entries = parse_feed_events(
            response.text,
            announcement_dates=entry["feedKind"] == ir_registry.FEED_RSS_ANNOUNCEMENT,
        )

    if not entries:
        # The feed answered and yielded nothing we could read. A restyled or moved feed must read
        # as a degradation, not as "this company has no scheduled events" (§9.44).
        result.degraded.append(degraded(COMPANY_IR, REASON_ERROR))
        return result

    dropped = 0
    for item in entries:
        if len(result.scheduled) >= limit:
            break
        kind = ir_registry.classify(item.get("title"), entry["eventKinds"])
        if not kind:
            continue

        day = str(item.get("startsAt") or "")
        clock = str(item.get("clock") or "")
        if item.get("utc") and clock:
            scheduled_at = normalise_timestamp(f"{day}T{clock}:00Z")
            zone_name, local_clock = "UTC", clock
        else:
            zone_name = entry["timezone"]
            local_clock = clock or entry["defaultClock"]
            if not local_clock:
                # A date with no time and no configured default. The company published a day, so
                # the day is what we know; midnight company-local is recorded and the absent time
                # is left absent rather than invented as a plausible-looking 16:30.
                local_clock = ""
                scheduled_at = _ir_local_timestamp(day, "00:00", zone_name)
            else:
                scheduled_at = _ir_local_timestamp(day, local_clock, zone_name)
        if not scheduled_at:
            dropped += 1
            continue

        result.scheduled.append(_scheduled(
            kind=kind,
            ticker=symbol,
            series=None,
            scheduled_at=scheduled_at,
            # The company publishing its own event IS the confirmation. This is the whole reason
            # the collector exists, and it is the only source in the cascade entitled to say so.
            confirmed=True,
            source=COMPANY_IR,
            title=_clean(item.get("title"))[:160] or f"{entry['company']} event",
            description=(
                f"Confirmed by {entry['sourceLabel']}. Quarterly results can update reported "
                "fundamentals and management's outlook."
                if kind == ir_registry.KIND_EARNINGS
                else f"Scheduled company event confirmed by {entry['sourceLabel']}."
            ),
            importance="high" if kind == ir_registry.KIND_EARNINGS else "medium",
            status="confirmed",
            source_url=_clean(item.get("url")) or entry["homeUrl"],
            timezone_name=zone_name,
            local_time=local_clock,
            # No occurrence key: an IR feed states the EVENT, not the fiscal period, so this
            # collector cannot honestly produce the aggregator's `earnings|SYM|fiscalEnd` key.
            # `store_scheduled_events` adopts the tentative row instead, under a stated rule.
            occurrence_key=None,
        ))

    if dropped:
        result.degraded.append(degraded(COMPANY_IR, REASON_UNPARSEABLE_TIME))
    return result


# ---- the registry the scheduler walks --------------------------------------------------------------

# `per_ticker` says which loop a provider belongs in: news providers are queried once per ticker,
# calendar providers once per pass over a date window. One list, walked once — not one loop per
# provider.
PER_TICKER_PROVIDERS: dict[str, object] = {
    MARKETAUX: fetch_marketaux,
    SEC_EDGAR: fetch_sec_filings,
    GOOGLE_NEWS_RSS: fetch_rss_news,
    ALPHAVANTAGE: fetch_alphavantage_earnings,
    # Phase 2C. Per-ticker because coverage is per-company: a ticker with no registry entry costs
    # no request at all, it costs one labelled `company-ir:no-coverage`.
    COMPANY_IR: fetch_company_ir,
}

WINDOW_PROVIDERS: dict[str, object] = {
    FEDERAL_RESERVE: fetch_federal_reserve_calendar,
    # Phase 2A/2B. Listed BEFORE the redistributors so that within a single pass the agency's own
    # row is written first and the weaker source merges onto it, rather than the other way round.
    # (`store_scheduled_events` gets the same answer either way — precedence, not arrival order,
    # decides — but the history reads more sensibly when the stronger source created the row.)
    BLS: fetch_bls_schedule,
    BEA: fetch_bea_schedule,
    FRED: fetch_fred_releases,
    FMP: fetch_fmp_calendar,
}

ALL_PROVIDERS: tuple[str, ...] = tuple(PER_TICKER_PROVIDERS) + tuple(WINDOW_PROVIDERS)
