// positionSize.js — the 1–2%-per-trade position sizer as pure, side-effect-free math
// (docs/BEGINNER.md Piece 2). Turns {accountEquity, riskPct, entry, stop, target} into
// shares / position value / dollars at risk / reward:risk, plus plain-language flags.
//
// DESCRIPTIVE RISK MATH ONLY. It emits no buy/sell/hold, connects to nothing, and executes nothing
// (invariant #2). It never invents a price level — a suggested stop comes ONLY from analysis-computed
// ATR / support that the dashboard already carries (see suggestStops), so invariant #3 holds trivially.
// No LLM, no network. The repo has no web test runner, so this stays a plain module: computeSize and
// suggestStops are trivially checkable by hand against the formulas below.

// num coerces anything (including "" from a form input) to a finite number, else 0 — so a blank field
// never produces NaN/Infinity downstream.
const num = (x) => {
  const v = Number(x);
  return Number.isFinite(v) ? v : 0;
};

const round2 = (v) => Math.round(v * 100) / 100;

// computeSize is the core sizer. Formula (docs/BEGINNER.md Piece 2):
//   riskPerShare  = |entry − stop|
//   dollarRisk    = accountEquity × (riskPct / 100)
//   shares        = riskPerShare > 0 ? floor(dollarRisk / riskPerShare) : 0
//   positionValue = shares × entry
//   rr            = target ? |target − entry| / riskPerShare : null
// Divide-by-zero / NaN are guarded: shares falls back to 0 and rr to null rather than Infinity.
export function computeSize({ accountEquity, riskPct, entry, stop, target } = {}) {
  const equity = Math.max(0, num(accountEquity));
  const pct = num(riskPct);
  const entryN = num(entry);
  const stopN = num(stop);
  const targetN = target === "" || target == null ? null : num(target);

  const riskPerShare = Math.abs(entryN - stopN);
  const dollarRisk = equity * (pct / 100);
  const shares = riskPerShare > 0 ? Math.floor(dollarRisk / riskPerShare) : 0;
  const positionValue = shares * entryN;
  const rr = targetN != null && riskPerShare > 0 ? Math.abs(targetN - entryN) / riskPerShare : null;

  const flags = {
    stopMissing: !(stopN > 0),
    riskPctOverCap: pct > 2,
    positionExceedsEquity: positionValue > equity && equity > 0,
    rrBelowFloor: rr != null && rr < 1.5,
  };

  return { riskPerShare, dollarRisk, shares, positionValue, rr, flags };
}

// suggestStops derives candidate stops from numbers the analysis service ALREADY computed — a
// 1.5×ATR(14) stop and the nearest computed support/resistance beyond entry. It never invents a level
// (invariant #3): if the dashboard carries neither, it returns [] and the caller hides the affordance
// so the user types their own stop. Returns an ordered array of { stop, basis, detail } (ATR first).
//   - ATR:               dashboard.indicators.latest.atr  (numeric ATR(14), analysis main.py)
//   - support/resistance: dashboard.regime.keyLevels.{support,resistance}  (regime.py swing_levels)
export function suggestStops(dashboard, entry, side = "long") {
  const entryN = num(entry);
  if (!(entryN > 0)) return [];

  const atr = num(dashboard?.indicators?.latest?.atr);
  const kl = dashboard?.regime?.keyLevels || {};
  const support = (Array.isArray(kl.support) ? kl.support : []).map(num);
  const resistance = (Array.isArray(kl.resistance) ? kl.resistance : []).map(num);

  const out = [];
  if (side === "short") {
    // A short's stop sits ABOVE entry: 1.5×ATR above, or the nearest computed resistance above.
    if (atr > 0) out.push({ stop: round2(entryN + 1.5 * atr), basis: "1.5×ATR", detail: `entry + 1.5 × ATR(${round2(atr)})` });
    const above = resistance.filter((r) => r > entryN).sort((a, b) => a - b)[0];
    if (above != null) out.push({ stop: round2(above), basis: "resistance", detail: `nearest resistance $${round2(above)}` });
  } else {
    // A long's stop sits BELOW entry: 1.5×ATR below, or the nearest computed support below.
    if (atr > 0) out.push({ stop: round2(entryN - 1.5 * atr), basis: "1.5×ATR", detail: `entry − 1.5 × ATR(${round2(atr)})` });
    const below = support.filter((s) => s < entryN).sort((a, b) => b - a)[0];
    if (below != null) out.push({ stop: round2(below), basis: "support", detail: `nearest support $${round2(below)}` });
  }
  return out.filter((c) => c.stop > 0);
}
