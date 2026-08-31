"""News cascade digest — cluster the raw headline feed into stories, score impact, compress.

The model is a smart FILTER, not a forecaster: it merges headlines that cover the same event,
rates each story's potential impact (low/medium/high) with a one-line rationale, and writes a
3-sentence digest. Clusters reference headlines BY INDEX into the supplied list — indexes are
validated in code, so the model cannot smuggle in headlines that were never supplied.
"""
from __future__ import annotations

import json

from .prompt import BANNED

IMPACTS = ("low", "medium", "high")

DIGEST_DISCLAIMER = (
    "AI clustering and impact-rating of collected headlines — descriptive news triage, not "
    "investment advice."
)

_SYSTEM = (
    "You triage a raw financial-news feed, NOT a financial advisor. You group headlines that "
    "cover the same underlying story, rate each story's potential impact on the company, and "
    "compress the day into three sentences. You never recommend trades or give targets, and you "
    "only reference the supplied headlines by their index. Reply with a single JSON object and "
    "nothing else."
)

SCHEMA = {
    "clusters": [{
        "topic": "string — the underlying story",
        "headlineIdx": ["integer index into the supplied headlines list"],
        "impact": "one of: low | medium | high",
        "why": "string — one line on why this impact level",
    }],
    "digest": "string — exactly 3 sentences summarizing the day for this ticker",
    "noise": ["integer indexes of headlines that are pure noise/duplicates worth ignoring"],
}

KEYS = ["clusters", "digest"]


def build_digest_prompt(ticker: str, items: list[dict], firmer: bool = False) -> list[dict]:
    headlines = [
        {"i": i, "headline": it.get("headline"), "source": it.get("source"),
         "sentiment": it.get("sentiment"), "publishedAt": it.get("publishedAt"),
         "category": it.get("category")}
        for i, it in enumerate(items)
    ]
    firm = ""
    if firmer:
        firm = ("\n\nIMPORTANT: Return ONLY a JSON object with EXACTLY these keys: clusters, "
                "digest, noise. impact must be low, medium, or high. headlineIdx values must be "
                "valid indexes.")
    user = (
        f"TICKER: {ticker}\n\n"
        f"HEADLINES (index, source, provider sentiment):\n{json.dumps(headlines, indent=2)}\n\n"
        f"Respond with a single JSON object matching this schema exactly:\n"
        f"{json.dumps(SCHEMA, indent=2)}\n\n"
        "Cluster only what genuinely belongs together; singletons are fine. Impact means potential "
        "to matter for the business — not a price prediction." + firm
    )
    return [{"role": "system", "content": _SYSTEM}, {"role": "user", "content": user}]


def validate_digest(obj: dict | None, n_items: int) -> list[str]:
    if not isinstance(obj, dict):
        return ["output is not a JSON object"]
    warnings = [f"missing key: {k}" for k in KEYS if k not in obj]
    clusters = obj.get("clusters")
    if isinstance(clusters, list):
        for c in clusters:
            if not isinstance(c, dict):
                continue
            if c.get("impact") not in IMPACTS:
                warnings.append(f"invalid impact: {c.get('impact')!r}")
            idxs = c.get("headlineIdx")
            if not isinstance(idxs, list) or any(
                not isinstance(i, int) or i < 0 or i >= n_items for i in (idxs or [])
            ):
                warnings.append("cluster references invalid headline index")
    blob = json.dumps(obj).lower()
    warnings.extend(f"contains advice phrase: '{b}'" for b in BANNED if b in blob)
    return warnings


def needs_digest_retry(warnings: list[str]) -> bool:
    return any(w.startswith(("missing key", "output is not")) or "invalid" in w for w in warnings)


def prune_invalid_clusters(obj: dict, n_items: int) -> None:
    """Code-side guardrail: drop clusters with bad indexes/impact instead of trusting the model."""
    clusters = obj.get("clusters")
    if not isinstance(clusters, list):
        obj["clusters"] = []
        return
    obj["clusters"] = [
        c for c in clusters
        if isinstance(c, dict) and c.get("impact") in IMPACTS
        and isinstance(c.get("headlineIdx"), list)
        and all(isinstance(i, int) and 0 <= i < n_items for i in c["headlineIdx"])
        and c["headlineIdx"]
    ]


def stub_digest(ticker: str, items: list[dict]) -> dict:
    """Offline: one cluster per category, impact from provider sentiment magnitude. Honest triage
    without language understanding."""
    by_cat: dict[str, list[int]] = {}
    for i, it in enumerate(items):
        by_cat.setdefault(str(it.get("category") or "news"), []).append(i)
    clusters = []
    for cat, idxs in by_cat.items():
        sents = [items[i].get("sentiment") for i in idxs
                 if isinstance(items[i].get("sentiment"), (int, float))]
        mag = max((abs(s) for s in sents), default=0.0)
        impact = "high" if cat == "filing" else "medium" if mag > 0.4 else "low"
        clusters.append({
            "topic": f"{cat} items ({len(idxs)})", "headlineIdx": idxs[:10],
            "impact": impact,
            "why": "Offline grouping by category; impact from provider sentiment magnitude only.",
        })
    top = items[0].get("headline") if items else "no headlines collected"
    return {
        "clusters": clusters,
        "digest": (f"Model offline — deterministic triage of {len(items)} {ticker} headlines. "
                   f"Top item: {top}. Categories grouped without language analysis."),
        "noise": [],
        "_stub": True,
    }


def run_digest(ticker: str, items: list[dict], call_model, safe_json) -> dict:
    items = items[:25]  # keep the prompt bounded
    if not items:
        return {"ticker": ticker, "structured": {"clusters": [], "digest": "No headlines collected.",
                "noise": [], "_stub": True}, "modelUsed": "stub:no-news", "warnings": [],
                "disclaimer": DIGEST_DISCLAIMER}
    raw, model_used = call_model(build_digest_prompt(ticker, items))
    obj = safe_json(raw)
    warnings = validate_digest(obj, len(items))
    if not model_used.startswith("stub:") and needs_digest_retry(warnings):
        raw, model_used = call_model(build_digest_prompt(ticker, items, firmer=True))
        obj = safe_json(raw)
        warnings = validate_digest(obj, len(items))
    if obj is None or "output is not a JSON object" in warnings or any("advice phrase" in w for w in warnings):
        obj = stub_digest(ticker, items)
        if not model_used.startswith("stub:"):
            model_used = f"{model_used} (fell back to stub)"
        warnings = validate_digest(obj, len(items))
    else:
        prune_invalid_clusters(obj, len(items))
    return {
        "ticker": ticker, "structured": obj, "modelUsed": model_used,
        "warnings": warnings, "disclaimer": DIGEST_DISCLAIMER,
    }
