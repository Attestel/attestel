"""Phase 2D — turning an explicitly stated future event in a filing into a Calendar date.

THE ONE RULE
------------
A date reaches the Calendar from a filing only when the filing STATES it unambiguously. Everything
else stays evidence: the 8-K is still stored, still searchable, still attached to its event — it
just does not become a scheduled date. That asymmetry is deliberate and it is the whole design:

    a missing Calendar entry is a gap someone notices;
    a wrong Calendar entry is a fact someone acts on.

So every rule below fails CLOSED. No inference, no "probably", and above all no model: Qwen is
never asked what a date might be, here or anywhere else in the Calendar path (invariant #7 —
"the LLM must not invent dates").

WHAT COUNTS AS UNAMBIGUOUS
--------------------------
All five of these, in one sentence, or the sentence is skipped:

1. **A scheduling phrase.** "will report", "will hold", "is scheduled for", "will announce", "will
   host", "conference call". Prose that merely mentions a date ("the agreement dated March 3,
   2026") is not a scheduled event.
2. **An absolute date WITH A YEAR.** "August 27, 2026" qualifies; "August 27" does not. A bare
   month-and-day is ambiguous across years, and picking the nearest one is exactly the guess this
   module exists to refuse.
3. **Exactly one candidate date in the sentence.** Two dates means we do not know which the phrase
   refers to. Two candidates, zero entries.
4. **A future date**, relative to the filing's own publication — a *planned* event. A date in the
   past is a historical reference, not a schedule.
5. **Inside a bounded horizon.** Beyond `MAX_HORIZON_DAYS` a date in a filing is far more likely a
   contract term or a maturity than a scheduled company event.

A TIME OF DAY IS OPTIONAL, AND UNTRUSTWORTHY WITHOUT A ZONE
-----------------------------------------------------------
A clock is used only when the sentence also names a timezone it can be read in. "at 2:00 p.m.
Pacific Time" gives an instant; "at 2:00 p.m." does not, and a filing's own timezone cannot be
assumed from the filer's address. Without one, the DAY is what was stated, so the day is what is
recorded — at midnight US/Eastern with an empty `local_time`, the same convention
`fetch_company_ir` uses for an all-day feed entry, so the Calendar renders a date with no time
rather than a confidently wrong hour.
"""
from __future__ import annotations

import hashlib
import re
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

from .db import Connection

#: How far ahead a filing may schedule something and still be read as an event.
MAX_HORIZON_DAYS = 400
#: How many stored filings one linkage pass examines. Bounded like every other pass in this service.
DEFAULT_SCAN_LIMIT = 200
#: Longest evidence sentence stored beside a derived date.
EVIDENCE_CAP = 400

#: The scheduling phrases. A closed, auditable table — not a similarity score.
SCHEDULING_PHRASES = (
    "will report", "will announce", "will release", "will hold", "will host",
    "will conduct", "will webcast", "is scheduled for", "are scheduled for",
    "has scheduled", "scheduled to be held", "scheduled to report", "conference call on",
    "will take place on", "to be held on",
)

#: Which of those are an earnings event rather than a generic company event.
EARNINGS_PHRASES = (
    "earnings", "financial results", "quarterly results", "results of operations",
    "fourth quarter results", "full year results", "annual results",
)

#: Timezone words a filing may use, mapped to a real zone. Nothing is inferred from the filer.
TIMEZONE_WORDS = {
    "eastern": "America/New_York", "et": "America/New_York",
    "est": "America/New_York", "edt": "America/New_York",
    "central": "America/Chicago", "ct": "America/Chicago",
    "cst": "America/Chicago", "cdt": "America/Chicago",
    "mountain": "America/Denver", "mt": "America/Denver",
    "pacific": "America/Los_Angeles", "pt": "America/Los_Angeles",
    "pst": "America/Los_Angeles", "pdt": "America/Los_Angeles",
    "utc": "UTC", "gmt": "UTC",
}

#: The day is what was stated when no clock survives the zone rule. Recorded in this zone so a US
#: filer's date renders as that date rather than shifting a day in either direction.
FALLBACK_ZONE = "America/New_York"

_MONTHS = {
    "january": 1, "february": 2, "march": 3, "april": 4, "may": 5, "june": 6,
    "july": 7, "august": 8, "september": 9, "october": 10, "november": 11, "december": 12,
    "jan": 1, "feb": 2, "mar": 3, "apr": 4, "jun": 6, "jul": 7, "aug": 8,
    "sept": 9, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}
# A YEAR IS MANDATORY in both spellings. That is the rule, expressed in the regex rather than
# checked afterwards, so there is no path that can reach a date without one.
_DATE_WORDS = re.compile(
    r"\b(" + "|".join(sorted(_MONTHS, key=len, reverse=True)) +
    r")\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(\d{4})\b",
    re.IGNORECASE,
)
_DATE_SLASH = re.compile(r"\b(\d{1,2})/(\d{1,2})/(\d{4})\b")
_DATE_ISO = re.compile(r"\b(\d{4})-(\d{2})-(\d{2})\b")
_CLOCK = re.compile(r"\b(\d{1,2}):(\d{2})\s*([ap])m\b", re.IGNORECASE)
_ZONE = re.compile(
    r"\b(" + "|".join(sorted(TIMEZONE_WORDS, key=len, reverse=True)) + r")\b(?:\s+time)?",
    re.IGNORECASE,
)
_SENTENCE = re.compile(r"(?<=[.;!?])\s+|\n+")

# "2:00 p.m." contains two sentence-ending periods, so a naive split severs the clock from its
# timezone and the date from both. Normalise the two meridiem spellings BEFORE splitting — a
# purely textual substitution that changes no meaning and is applied to the evidence string too,
# so what is stored is what was matched.
_MERIDIEM_DOTS = re.compile(r"\b([ap])\.\s?m\.", re.IGNORECASE)


def _normalise_prose(text: str) -> str:
    return _MERIDIEM_DOTS.sub(lambda m: f"{m.group(1).lower()}m", " ".join(str(text or "").split()))


def _iso(moment: datetime) -> str:
    return moment.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _candidate_dates(sentence: str) -> list[str]:
    """Every absolute, year-bearing date in the sentence, as `YYYY-MM-DD`, de-duplicated.

    De-duplicated because one date written twice is still one date; two DIFFERENT dates is what
    makes the sentence ambiguous.
    """
    found: list[str] = []

    def add(year: int, month: int, day: int) -> None:
        try:
            value = datetime(year, month, day).date().isoformat()
        except ValueError:
            return
        if value not in found:
            found.append(value)

    for match in _DATE_WORDS.finditer(sentence):
        month = _MONTHS.get(match.group(1).casefold())
        if month:
            add(int(match.group(3)), month, int(match.group(2)))
    for match in _DATE_SLASH.finditer(sentence):
        add(int(match.group(3)), int(match.group(1)), int(match.group(2)))
    for match in _DATE_ISO.finditer(sentence):
        add(int(match.group(1)), int(match.group(2)), int(match.group(3)))
    return found


def _clock_and_zone(sentence: str) -> tuple[str, str]:
    """`("HH:MM", "Area/City")`, or `("", "")` when either half is missing or ambiguous.

    Both halves or neither. A time with no zone is not a time — see the module docstring.
    """
    clocks = _CLOCK.findall(sentence)
    zones = _ZONE.findall(sentence)
    if len(clocks) != 1 or not zones:
        return "", ""
    named = {TIMEZONE_WORDS[z.casefold()] for z in zones}
    if len(named) != 1:
        # Two different zones in one sentence: which one governs the clock is a judgement call, so
        # the clock is dropped and the day is kept.
        return "", ""
    hour, minute, meridiem = int(clocks[0][0]), int(clocks[0][1]), clocks[0][2].casefold()
    if not (1 <= hour <= 12 and 0 <= minute <= 59):
        return "", ""
    if meridiem == "p" and hour != 12:
        hour += 12
    elif meridiem == "a" and hour == 12:
        hour = 0
    return f"{hour:02d}:{minute:02d}", named.pop()


def _instant(day: str, clock: str, zone: str) -> str | None:
    try:
        local = datetime.strptime(f"{day} {clock or '00:00'}", "%Y-%m-%d %H:%M").replace(
            tzinfo=ZoneInfo(zone)
        )
    except (TypeError, ValueError, ZoneInfoNotFoundError):
        return None
    return _iso(local)


def extract_planned_events(text: str, *, published_at: str) -> list[dict]:
    """Every unambiguously stated future event in `text`. Pure, deterministic, no I/O.

    Returns `{kind, day, clock, timezone, scheduledAt, evidence}` per qualifying sentence. An
    ambiguous sentence contributes nothing and says nothing — the document remains evidence, which
    is what it already was.
    """
    try:
        filed = datetime.strptime(published_at, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
    except (TypeError, ValueError):
        return []
    horizon = filed + timedelta(days=MAX_HORIZON_DAYS)

    out: list[dict] = []
    seen: set[tuple[str, str]] = set()
    for raw in _SENTENCE.split(_normalise_prose(text)):
        sentence = raw.strip()
        if not sentence:
            continue
        lowered = sentence.casefold()
        if not any(phrase in lowered for phrase in SCHEDULING_PHRASES):
            continue

        days = _candidate_dates(sentence)
        if len(days) != 1:
            # Zero: nothing absolute was stated. Two or more: we do not know which one the
            # scheduling phrase refers to. Both stay evidence only.
            continue
        day = days[0]

        clock, zone = _clock_and_zone(sentence)
        scheduled_at = _instant(day, clock, zone or FALLBACK_ZONE)
        if not scheduled_at:
            continue

        moment = datetime.strptime(scheduled_at, "%Y-%m-%dT%H:%M:%SZ").replace(
            tzinfo=timezone.utc
        )
        if moment <= filed or moment > horizon:
            # Not a plan: either it already happened when the filing was made, or it is far enough
            # out to be a contract term rather than a scheduled event.
            continue

        kind = "earnings" if any(word in lowered for word in EARNINGS_PHRASES) else "company_event"
        key = (kind, day)
        if key in seen:
            continue
        seen.add(key)
        out.append({
            "kind": kind,
            "day": day,
            "clock": clock,
            "timezone": zone or FALLBACK_ZONE,
            "scheduledAt": scheduled_at,
            "evidence": sentence[:EVIDENCE_CAP],
        })
    return out


def planned_occurrence_key(document_id: str, kind: str, day: str) -> str:
    """Stable identity for a date derived from one filing.

    Keyed on the DOCUMENT, so re-scanning the same filing converges on the same row rather than
    creating a second one — the whole idempotency story for this path. A different filing stating
    the same date produces a different key and merges through
    `ingest._adoptable_row`'s precedence rules, exactly like any other pair of sources.
    """
    return f"planned|{document_id}|{kind}|{day}"


def _document_text(row) -> str:
    """Title, excerpt and body as SEPARATE sentences, in the order a filing states things.

    The separator is load-bearing. Concatenating them with a space merges the headline into the
    first sentence of the body — and this service's own 8-K headline contains the filing date
    ("NVDA 8-K filed 2026-08-20"). That extra date then makes every first sentence look like it
    holds two candidate dates, and the ambiguity rule correctly but uselessly rejects the whole
    document. Measured, not hypothetical: it rejected the one filing this was written for.
    """
    return ". ".join(
        str(part).rstrip(". ")
        for part in (row.get("title"), row.get("excerpt"), row.get("body"))
        if part
    )


def link_planned_events(
    conn: Connection,
    *,
    now: datetime | None = None,
    providers=("sec-edgar",),
    limit: int = DEFAULT_SCAN_LIMIT,
) -> dict:
    """Scan stored filings, upsert what they unambiguously scheduled, and record the link.

    Bounded (`limit` most-recent filings), deterministic, and idempotent: the occurrence key is a
    function of the document, so running this twice writes the same rows and appends no spurious
    revision. It performs NO network call — it reads documents this service already stored.

    Returns `{examined, extracted, linked, skipped}`.
    """
    from .ingest import store_scheduled_events  # local: ingest imports this module's caller path

    moment = (now or datetime.now(timezone.utc)).astimezone(timezone.utc)
    stamp = _iso(moment)
    marks = ",".join("?" for _ in providers)
    rows = conn.execute(
        f"SELECT id, provider, url, title, excerpt, body, published_at, source_tier "
        f"FROM source_documents WHERE provider IN ({marks}) "
        f"ORDER BY published_at DESC, id DESC LIMIT ?",
        tuple(providers) + (max(1, int(limit)),),
    ).fetchall()

    report = {"examined": len(rows), "extracted": 0, "linked": 0, "skipped": 0}
    pending: list[tuple[dict, dict]] = []
    for row in rows:
        found = extract_planned_events(
            _document_text(row), published_at=str(row["published_at"] or "")
        )
        if not found:
            report["skipped"] += 1
            continue
        for item in found:
            report["extracted"] += 1
            pending.append((dict(row), item))

    if not pending:
        return report

    scheduled_rows = []
    for row, item in pending:
        ticker = _subject_ticker(conn, row["id"])
        scheduled_rows.append({
            "kind": item["kind"],
            "ticker": ticker,
            "series": None,
            "scheduled_at": item["scheduledAt"],
            # A filing states an intention; the company's own IR schedule confirms it. Marking this
            # confirmed would put a filing ahead of the company on the company's own date, which
            # the precedence order deliberately does not do.
            "confirmed": 0,
            "status": "scheduled",
            "source": row["provider"],
            "source_tier": row["source_tier"] or "official",
            "source_url": row["url"],
            "title": _derived_title(item, ticker),
            "description": (
                "Stated in a filing. The date is quoted from the filing text; it is not confirmed "
                "by the company's investor-relations schedule."
            ),
            "importance": "high" if item["kind"] == "earnings" else "medium",
            "timezone": item["timezone"],
            "local_time": item["clock"],
            "occurrence_key": planned_occurrence_key(row["id"], item["kind"], item["day"]),
        })

    store_scheduled_events(conn, scheduled_rows, now=stamp)

    for (row, item), scheduled in zip(pending, scheduled_rows):
        found = conn.execute(
            "SELECT id FROM scheduled_events WHERE occurrence_key = ?",
            (scheduled["occurrence_key"],),
        ).fetchone()
        if found is None:
            continue
        conn.execute(
            "INSERT INTO scheduled_event_documents "
            "(event_id, document_id, document_url, provider, evidence, linked_at) "
            "VALUES (?,?,?,?,?,?) ON CONFLICT (event_id, document_id) DO UPDATE SET "
            "document_url = EXCLUDED.document_url, evidence = EXCLUDED.evidence",
            (found["id"], row["id"], row["url"] or "", row["provider"] or "",
             item["evidence"], stamp),
        )
        report["linked"] += 1
    conn.commit()
    return report


def _derived_title(item: dict, ticker: str | None) -> str:
    subject = f"{ticker} " if ticker else ""
    if item["kind"] == "earnings":
        return f"{subject}earnings (stated in a filing)"
    return f"{subject}scheduled company event (stated in a filing)"


def _subject_ticker(conn: Connection, document_id: str) -> str | None:
    """The company the filing is about, from the resolved mapping this service already computed.

    Read, never inferred. If the corpus has not resolved a ticker for the document, the derived
    event carries none — a market-wide-looking row is wrong, but so is a guessed company.
    """
    row = conn.execute(
        "SELECT et.ticker FROM event_tickers et "
        "JOIN event_documents ed ON ed.event_id = et.event_id "
        "WHERE ed.document_id = ? ORDER BY et.relevance DESC, et.ticker LIMIT 1",
        (document_id,),
    ).fetchone()
    return row["ticker"] if row else None
