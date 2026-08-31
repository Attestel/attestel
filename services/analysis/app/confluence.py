"""Multi-timeframe confluence (deeper-analysis phase) — DETERMINISTIC rules only.

Given the regime computed on several timeframes (1D/1H/15m), summarize where they AGREE and where
they CONFLICT. This is descriptive evidence, never a recommendation: the LLM only comments on the
picture built here; it never invents it. No buy/sell language originates in this module.
"""
from __future__ import annotations

# Higher timeframe first, for stable output ordering.
TF_RANK = {"1D": 0, "1H": 1, "15m": 2, "5m": 3}

_EXTREMES = {"overbought", "oversold"}


def _trend_vote(trend: str | None) -> int:
    t = (trend or "").lower()
    if "uptrend" in t:
        return 1
    if "downtrend" in t:
        return -1
    return 0  # "insufficient history", "unknown", etc.


def _short_trend(trend: str | None) -> str:
    v = _trend_vote(trend)
    return "uptrend" if v > 0 else "downtrend" if v < 0 else "no clear trend"


def _momentum_label(momentum: str | None) -> str:
    t = (momentum or "").lower()
    if "overbought" in t:
        return "overbought"
    if "oversold" in t:
        return "oversold"
    if "neutral" in t:
        return "neutral"
    return "unknown"


def _build_note(tfs: list[str], alignment: str, mlabels: dict[str, str]) -> str:
    if not tfs:
        return "No timeframe data available."
    tfs_str = ", ".join(tfs)
    if alignment == "aligned up":
        base = f"{tfs_str} are all in an uptrend"
    elif alignment == "aligned down":
        base = f"{tfs_str} are all in a downtrend"
    elif alignment == "no clear trend":
        base = f"No timeframe ({tfs_str}) shows a clear trend"
    else:
        base = f"Timeframes disagree on trend across {tfs_str}"
    extremes = [f"{tf} {mlabels[tf]}" for tf in tfs if mlabels[tf] in _EXTREMES]
    if extremes:
        base += "; " + ", ".join(extremes)
    return base + "."


def build_confluence(regimes: dict[str, dict]) -> dict:
    """regimes: {timeframe -> regime dict}. Returns a compact, panel-ready + LLM-ready summary."""
    tfs = sorted(regimes.keys(), key=lambda t: TF_RANK.get(t, 99))

    trend_by = {tf: regimes[tf].get("trend") for tf in tfs}
    strength_by = {tf: regimes[tf].get("trendStrength") for tf in tfs}
    mom_by = {tf: regimes[tf].get("momentum") for tf in tfs}
    stoch_by = {tf: regimes[tf].get("stochastic") for tf in tfs}

    votes = {tf: _trend_vote(trend_by[tf]) for tf in tfs}
    ups = sum(1 for v in votes.values() if v > 0)
    downs = sum(1 for v in votes.values() if v < 0)

    if ups > 0 and downs == 0:
        alignment = "aligned up"
    elif downs > 0 and ups == 0:
        alignment = "aligned down"
    elif ups == 0 and downs == 0:
        alignment = "no clear trend"
    else:
        alignment = "mixed"

    # Explicit conflicts, most-significant first: trend disagreements, then momentum extremes.
    conflicts: list[str] = []
    for i in range(len(tfs)):
        for j in range(i + 1, len(tfs)):
            a, b = tfs[i], tfs[j]
            if votes[a] != 0 and votes[b] != 0 and votes[a] != votes[b]:
                conflicts.append(f"{a} {_short_trend(trend_by[a])} but {b} {_short_trend(trend_by[b])}")

    mlabels = {tf: _momentum_label(mom_by[tf]) for tf in tfs}
    for i in range(len(tfs)):
        for j in range(i + 1, len(tfs)):
            a, b = tfs[i], tfs[j]
            la, lb = mlabels[a], mlabels[b]
            if la != lb and (la in _EXTREMES or lb in _EXTREMES):
                conflicts.append(f"{a} {la} vs {b} {lb}")

    flags = [f"{tf} {mlabels[tf]}" for tf in tfs if mlabels[tf] in _EXTREMES]
    momentum_flags = "; ".join(flags) if flags else "no momentum extremes"

    return {
        "timeframes": tfs,
        "trendByTf": trend_by,
        "trendStrengthByTf": strength_by,
        "momentumByTf": mom_by,
        "stochByTf": stoch_by,
        "trendAlignment": alignment,
        "alignmentScore": ups - downs,  # net trend vote: +N net up, -N net down
        "alignmentDetail": {"up": ups, "down": downs},
        "momentumFlags": momentum_flags,
        "conflicts": conflicts,
        "note": _build_note(tfs, alignment, mlabels),
    }
