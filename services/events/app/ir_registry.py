"""Phase 2C — the company investor-relations source registry.

WHY A REGISTRY AND NOT `if ticker == "NVDA"`
--------------------------------------------
Company IR schedules are the top of the Calendar's precedence order — the company is the authority
on its own earnings date — but there is no standard for them. Each issuer publishes its own feed,
at its own URL, in its own timezone, and changes it without notice. Encoding that as branching
logic inside the collector would mean a code change, a review and a deploy every time one company
moves a URL, and it would make "which companies do we actually cover?" unanswerable without reading
the collector.

So coverage is DATA. This module holds it, exposes it, and answers the coverage question directly:
`coverage()` names every configured company and `missing()` names the followed tickers that have
none — which is what the product surfaces instead of guessing a date (Calendar §"missing coverage
stays missing").

THE ENTRY IS CONFIGURATION, NOT A FACT CLAIM
--------------------------------------------
Every field below — the feed URL above all — is operator-editable configuration. Set
`COMPANY_IR_REGISTRY_PATH` to a JSON file to add companies or correct a URL with no code change;
entries there are merged over the built-ins by ticker. A feed URL that turns out to be wrong
produces `company-ir:error` and falls back to whatever the weaker sources already said. It never
produces a guessed date, and it never silently disables the company.

NOTHING HERE OPENS A SOCKET. This module is a table and three pure functions.
"""
from __future__ import annotations

import json
import os
from pathlib import Path

REGISTRY_PATH_ENV = "COMPANY_IR_REGISTRY_PATH"

#: Feed formats the collector can parse. Both are standards with stdlib parsers; an entry naming
#: anything else is rejected at load time rather than attempted and mis-parsed.
FEED_RSS = "rss"      # RSS 2.0 or Atom — <item>/<entry> with a title, link and date
FEED_RSS_ANNOUNCEMENT = "rss_announcement"  # first-party release naming a future event date
FEED_ICS = "ics"      # iCalendar — VEVENT with DTSTART and SUMMARY
FEED_KINDS = (FEED_RSS, FEED_RSS_ANNOUNCEMENT, FEED_ICS)

#: The event vocabulary an IR feed may contribute to the Calendar. Closed on purpose: an IR feed
#: also carries conference appearances and webcast replays, and those are not catalysts.
KIND_EARNINGS = "earnings"
KIND_COMPANY_EVENT = "company_event"
EVENT_KINDS = (KIND_EARNINGS, KIND_COMPANY_EVENT)

#: Title keywords that make an IR calendar entry an EARNINGS event. Deterministic and auditable —
#: this is the one classification step in the IR path, and it is a table, not a model.
EARNINGS_KEYWORDS = (
    "earnings", "financial results", "quarterly results", "results conference call",
    "q1 fy", "q2 fy", "q3 fy", "q4 fy", "fourth quarter results", "full year results",
)

REQUIRED_FIELDS = ("ticker", "company", "feedUrl", "feedKind")


# ── the built-in production registry ─────────────────────────────────────────────────────────────
#
# NVIDIA is the one production configuration this repository ships, matching the rest of the
# codebase's NVDA-first scope. `feedUrl` is the company's own investor-relations events feed and is
# CONFIGURATION: it is the operator's to verify against the live site and to correct through
# COMPANY_IR_REGISTRY_PATH without touching this file. The collector fails soft if it is wrong.
#
# `timezone` is the company's stated release timezone (NVIDIA reports after the US market close,
# Pacific). `defaultClock` is used ONLY when a feed entry carries a date with no time of day — a
# feed that states a time always wins, and a date that cannot be parsed at all is dropped.
BUILTIN_REGISTRY: tuple[dict, ...] = (
    {
        "ticker": "NVDA",
        "company": "NVIDIA Corporation",
        # The Q4-hosted event feed is the link NVIDIA publishes, but it returns 403 to server-side
        # clients. NVIDIA's own newsroom RSS publishes the earnings-call announcement and is
        # reachable without a browser; the collector extracts only the explicitly announced date.
        "feedUrl": "https://nvidianews.nvidia.com/rss.xml",
        "feedKind": FEED_RSS_ANNOUNCEMENT,
        "sourceLabel": "NVIDIA Newsroom",
        "timezone": "America/Los_Angeles",
        "defaultClock": "14:00",
        "eventKinds": [KIND_EARNINGS],
        "homeUrl": "https://investor.nvidia.com/events-and-presentations/events-and-presentations/default.aspx",
    },
)


def _valid(entry: dict) -> bool:
    if not isinstance(entry, dict):
        return False
    if any(not str(entry.get(field) or "").strip() for field in REQUIRED_FIELDS):
        return False
    if entry.get("feedKind") not in FEED_KINDS:
        return False
    url = str(entry["feedUrl"]).strip()
    if not url.startswith(("http://", "https://")):
        return False
    kinds = entry.get("eventKinds") or [KIND_EARNINGS]
    return all(kind in EVENT_KINDS for kind in kinds)


def _normalise(entry: dict) -> dict:
    return {
        "ticker": str(entry["ticker"]).strip().upper(),
        "company": str(entry["company"]).strip(),
        "feedUrl": str(entry["feedUrl"]).strip(),
        "feedKind": entry["feedKind"],
        "sourceLabel": str(entry.get("sourceLabel") or entry["company"]).strip(),
        "timezone": str(entry.get("timezone") or "America/New_York").strip(),
        "defaultClock": str(entry.get("defaultClock") or "").strip(),
        "eventKinds": list(entry.get("eventKinds") or [KIND_EARNINGS]),
        "homeUrl": str(entry.get("homeUrl") or entry["feedUrl"]).strip(),
    }


def _overrides() -> list[dict]:
    """Entries from `COMPANY_IR_REGISTRY_PATH`, or none.

    Read at CALL time, and every failure mode is silent-but-visible in the same direction: an
    unreadable file, malformed JSON, or an entry missing a required field yields NO entry rather
    than a half-configured one. A company that does not appear in `coverage()` is reported as
    missing coverage, which is the honest outcome and the one the product surfaces.
    """
    path = os.getenv(REGISTRY_PATH_ENV, "").strip()
    if not path:
        return []
    try:
        payload = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return []
    if isinstance(payload, dict):
        payload = payload.get("companies") or []
    if not isinstance(payload, list):
        return []
    return [entry for entry in payload if _valid(entry)]


def registry() -> dict[str, dict]:
    """Ticker → entry. Overrides replace a built-in of the same ticker wholesale.

    Wholesale rather than field-by-field: a half-overridden entry (new URL, old timezone) is the
    configuration bug hardest to see, and an operator correcting a feed is describing one source,
    not patching ours.
    """
    out = {entry["ticker"]: entry for entry in (_normalise(e) for e in BUILTIN_REGISTRY)}
    for entry in _overrides():
        normalised = _normalise(entry)
        out[normalised["ticker"]] = normalised
    return out


def entry_for(ticker: str) -> dict | None:
    return registry().get((ticker or "").strip().upper())


def coverage(tickers=None) -> dict:
    """Which companies have an official IR source, and which do not.

    The second half is the point. A ticker with no IR feed is NOT quietly served from the
    aggregator as though the company had confirmed the date; it is reported here, and the Calendar
    labels it as having no official confirmation available.
    """
    table = registry()
    asked = [t.strip().upper() for t in (tickers or []) if str(t).strip()]
    covered = sorted(table) if not asked else sorted(t for t in asked if t in table)
    missing = [] if not asked else sorted(t for t in asked if t not in table)
    return {
        "covered": [
            {
                "ticker": table[t]["ticker"],
                "company": table[t]["company"],
                "sourceLabel": table[t]["sourceLabel"],
                "feedKind": table[t]["feedKind"],
                "homeUrl": table[t]["homeUrl"],
                "eventKinds": table[t]["eventKinds"],
            }
            for t in covered
        ],
        "missing": missing,
        "registrySource": "override" if _overrides() else "builtin",
    }


def classify(title: str, allowed_kinds) -> str | None:
    """The IR event kind for a feed entry title, or None when the entry is not a catalyst.

    A table lookup, in code, with a closed vocabulary. An IR feed also carries conference
    appearances, webcast replays and shareholder-meeting notices; those are not earnings and are
    not added to the Calendar as if they were.
    """
    lowered = " ".join(str(title or "").split()).casefold()
    if not lowered:
        return None
    if KIND_EARNINGS in allowed_kinds and any(word in lowered for word in EARNINGS_KEYWORDS):
        return KIND_EARNINGS
    if KIND_COMPANY_EVENT in allowed_kinds:
        return KIND_COMPANY_EVENT
    return None
