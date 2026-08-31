"""Market Brief — prompt + validation + deterministic stub.

The brief uses the LLM for what it is genuinely good at: READING and COMPRESSING the day's
ALREADY-COLLECTED facts (price change, technical regime, headlines, filings, earnings, crowd
sentiment) into a plain-language digest. It is NOT numeric prediction.

Same discipline as the structured read (prompt.py):
  * Grounded ONLY in the facts the gateway passes in — the model must not invent numbers, prices,
    or events; if a fact isn't supplied it says so.
  * DESCRIPTIVE, never prescriptive — no buy/sell/verdict, no price target. (Reuses the read's BANNED
    phrase list.)
  * When the model is unreachable, a deterministic stub built from the SAME facts keeps the pipeline
    whole (a plain, bulletized fact-list brief).
"""
from __future__ import annotations

import json

from .prompt import BANNED  # reuse the exact banned-advice phrase list from the read

BRIEF_SYSTEM = (
    "You are a financial news summarizer, NOT an advisor. You are given a set of FACTS already "
    "collected about a stock (or the overall market) today: price change, technical regime, "
    "headlines, filings, earnings, crowd sentiment. Your job is to READ and COMPRESS those facts "
    "into a plain-language brief. You NEVER give a buy or sell recommendation, price target, or "
    "verdict. You ONLY reference numbers and events that appear in the FACTS — never invent, "
    "estimate, or recompute anything. If something is not in the FACTS, do not speculate about it. "
    "You reply with a single JSON object and nothing else."
)

# The exact schema the model must emit.
BRIEF_SCHEMA = {
    "headline": "string — one plain-language sentence summarizing the day",
    "whatHappened": ["string — 2-5 factual bullets, each grounded in the FACTS"],
    "whyItMoved": ["string — 0-4 bullets connecting facts to the move (only if the FACTS support it)"],
    "calendar": [{"date": "string from FACTS", "event": "string from FACTS"}],
    "watch": ["string — 2-4 things to watch next (descriptive, not prescriptive)"],
    "riskFlags": ["string — 0-4 risk / uncertainty notes grounded in the FACTS"],
    "summary": "string — 2-3 plain-language sentences, no advice",
}

BRIEF_REQUIRED_KEYS = [
    "headline", "whatHappened", "whyItMoved", "calendar", "watch", "riskFlags", "summary",
]


def build_brief_prompt(ticker: str, context: dict, firmer: bool = False) -> list[dict]:
    subject = "the overall market" if str(ticker).upper() == "MARKET" else str(ticker).upper()
    firm = ""
    if firmer:
        firm = (
            "\n\nIMPORTANT: Your previous reply was invalid. Return ONLY a JSON object with EXACTLY "
            "these keys: headline, whatHappened, whyItMoved, calendar, watch, riskFlags, summary. "
            "No prose, no markdown, no buy/sell language."
        )
    user = (
        f"Write a market brief for {subject}, grounded ONLY in these FACTS (treat them as the "
        f"complete, authoritative set — add nothing that is not present here):\n"
        f"{json.dumps(context, indent=2, default=str)}\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n"
        f"{json.dumps(BRIEF_SCHEMA, indent=2)}\n\n"
        "Rules: quote only numbers and events that appear in the FACTS; if something is not in the "
        "FACTS, say it isn't available rather than guessing. Be descriptive — explain what happened "
        "and what to watch. Do NOT include any buy/sell/hold recommendation, price target, or verdict."
        + firm
    )
    return [{"role": "system", "content": BRIEF_SYSTEM}, {"role": "user", "content": user}]


def validate_brief(obj: dict | None) -> list[str]:
    """Loose validation mirroring validate_structured: missing keys, empty sections, sneaked-in
    advice. Structural failures (missing keys / not-an-object) trigger the one firmer retry."""
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    warnings = [f"missing key: {k}" for k in BRIEF_REQUIRED_KEYS if k not in obj]
    if isinstance(obj.get("headline"), str) and not obj["headline"].strip():
        warnings.append("empty headline")
    if isinstance(obj.get("whatHappened"), list) and not obj["whatHappened"]:
        warnings.append("empty whatHappened")
    if "summary" in obj and not str(obj.get("summary") or "").strip():
        warnings.append("empty summary")
    cal = obj.get("calendar")
    if cal is not None and not isinstance(cal, list):
        warnings.append("calendar is not a list")
    blob = json.dumps(obj, default=str).lower()
    for b in BANNED:
        if b in blob:
            warnings.append(f"contains advice phrase: '{b}'")
    return warnings


def needs_brief_retry(warnings: list[str]) -> bool:
    """Retry only on structural failure — not on a soft advice warning we can just flag."""
    return any(w.startswith(("missing key", "output is not")) for w in warnings)


# --------------------------------------------------------------------------- deterministic stub

def _pct(v) -> str | None:
    try:
        f = float(v)
    except (TypeError, ValueError):
        return None
    return f"{'+' if f >= 0 else ''}{f:.2f}%"


def _mover_line(m: dict) -> str:
    bits: list[str] = [str(m.get("ticker", "?"))]
    p = _pct(m.get("changePct"))
    if p:
        bits.append(p)
    if m.get("trend"):
        bits.append(f"trend {m['trend']}")
    if m.get("sentiment"):
        bits.append(f"crowd {m['sentiment']}")
    line = " · ".join(bits)
    if m.get("topHeadline"):
        line += f" — {m['topHeadline']}"
    return line


def stub_brief(ticker: str, context: dict) -> dict:
    """Deterministic bulletized brief from the collected facts (no model). Used when the model server
    is unreachable or returns something unusable — so the brief pipeline always returns SOMETHING that
    is honestly grounded in the same facts. Descriptive only; never a buy/sell call."""
    ticker = str(ticker or "").upper()
    context = context or {}
    what: list[str] = []
    why: list[str] = []
    calendar: list[dict] = []
    watch: list[str] = []
    risk: list[str] = []

    if ticker == "MARKET":
        for ix in context.get("indices") or []:
            p = _pct(ix.get("changePct"))
            if p is not None:
                what.append(f"{ix.get('ticker')} {p} on the day.")
        for m in context.get("tickers") or []:
            what.append(_mover_line(m))
        headline = "Market snapshot from today's collected facts."
        summary = (
            "A factual roundup of index moves, per-name price changes, technical trend and crowd "
            "tone across the tracked universe. Descriptive context only — not investment advice."
        )
        watch = [
            "Names with the largest moves or a trend flip.",
            "Any new 8-K filings or upcoming earnings in the tracked names.",
        ]
    else:
        price = context.get("price") or {}
        p = _pct(price.get("changePct"))
        if p is not None:
            last = price.get("last")
            src = price.get("source", "n/a")
            at = f" at {last}" if last is not None else ""
            what.append(f"{ticker} changed {p}{at} ({src} data).")
        regime = context.get("regime") or {}
        reg_bits = [f"{k}: {regime[k]}" for k in ("trend", "momentum", "volatility") if regime.get(k)]
        if reg_bits:
            what.append("Technical regime — " + "; ".join(reg_bits) + ".")
        conf = context.get("confluence") or {}
        if conf.get("note"):
            why.append(f"Multi-timeframe: {conf['note']}")
        if conf.get("conflicts"):
            risk.append("Timeframe conflict: " + "; ".join(str(x) for x in conf["conflicts"]))
        for n in (context.get("news") or [])[:5]:
            hl = n.get("headline")
            if hl:
                src = f" ({n['source']})" if n.get("source") else ""
                why.append(f"{hl}{src}")
        for f in context.get("filingsToday") or []:
            calendar.append({"date": f.get("date"), "event": f.get("headline") or "8-K filing"})
        earn = context.get("earnings")
        if isinstance(earn, dict) and earn:
            beat = earn.get("beat")
            label = earn.get("label") or earn.get("fiscalDateEnding") or "latest quarter"
            if beat is not None:
                sp = _pct(earn.get("surprisePercentage"))
                sp_txt = f", surprise {sp}" if sp else ""
                what.append(f"Most recent earnings ({label}): {'beat' if beat else 'miss'} on EPS{sp_txt}.")
        sent = context.get("sentiment")
        if isinstance(sent, dict) and sent.get("available") is not False:
            label = sent.get("label") or sent.get("sentimentLabel")
            if label:
                buzz = sent.get("buzz", "n/a")
                why.append(f"Crowd sentiment on StockTwits reads {label} (buzz {buzz}).")
            if sent.get("spike"):
                risk.append("Unusual social buzz spike — crowd extremes can be contrarian.")
        headline = f"{ticker} {p} today — a factual digest of the attestel, news and crowd tone." if p \
            else f"{ticker} — today's brief from the collected facts."
        summary = (
            f"A plain-language roundup of what the collected facts show for {ticker}: price change, "
            "technical regime, headlines and filings, earnings, and crowd sentiment. Descriptive "
            "context only; not investment advice."
        )
        watch = [
            "Follow-through on the technical trend noted above.",
            "Any new filings, upcoming earnings, or shifts in the news flow.",
        ]

    if not what:
        what = ["No collected facts were available to summarize (providers offline / no keys)."]

    return {
        "headline": headline,
        "whatHappened": what,
        "whyItMoved": why,
        "calendar": calendar,
        "watch": watch,
        "riskFlags": risk,
        "summary": summary,
        "_stub": True,
    }
