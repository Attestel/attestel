// Educational one-liners + tone derivations shared by the Regime and Confluence panels. Purely
// descriptive — no advice. Tones drive color only (bull/bear/neutral/caution).

export const TOOLTIPS = {
  rsi: "RSI (0–100): momentum oscillator. <30 oversold (stretched low), >70 overbought (stretched high).",
  stoch: "Stochastic %K/%D (0–100): where price sits in its recent range. <20 oversold, >80 overbought.",
  adx: "ADX (0–100): trend STRENGTH, not direction. <20 weak/choppy, 20–40 developing, >40 strong.",
  atr: "ATR: Average True Range — the typical per-bar move in $. A volatility gauge, not direction.",
  macd: "MACD histogram: momentum shift. Positive/rising = bullish push; negative/falling = bearish.",
  bbands: "Bollinger Bands: price vs a 20-period mean ±2σ. Near the upper band = stretched high; near the lower = stretched low.",
  vwap: "VWAP (intraday): volume-weighted average price. Above = buyers in control; below = sellers.",
  trend: "Trend: SMA50 vs SMA200 structure (golden/death cross) and price location.",
  volume: "Volume vs its 20-day average; accumulation (net buying) vs distribution (net selling).",
  ma: "Moving averages: price relative to EMA20 (fast), SMA50 (mid), SMA200 (slow) — the trend ladder.",
  confluence: "Do the timeframes agree? Alignment across 1D/1H/15m; conflicts flag mixed signals.",
};

export const firstWord = (s) => (s ? String(s).split(/[\s(]/)[0] : "—");

const cap = (s) => (s ? s.charAt(0).toUpperCase() + s.slice(1) : s);
export const capFirst = (s) => cap(firstWord(s).toLowerCase());

// trend string → bull / bear / neutral
export function trendTone(s) {
  const t = String(s || "").toLowerCase();
  if (t.includes("uptrend")) return "bull";
  if (t.includes("downtrend")) return "bear";
  return "neutral";
}

// momentum / stochastic string → tone. overbought reads caution (stretched high), oversold reads
// bull (stretched low / demand), else neutral.
export function oscillatorTone(s) {
  const t = String(s || "").toLowerCase();
  if (t.includes("overbought")) return "caution";
  if (t.includes("oversold")) return "bull";
  return "neutral";
}

// ADX strength phrase → tone (muted → info → accent handled by callers; here neutral/info/bull-ish)
export function strengthTone(s) {
  const t = String(s || "").toLowerCase();
  if (t.includes("strong")) return "info";
  if (t.includes("developing")) return "info";
  return "neutral";
}

// volumeTrend "accumulation" (buying) → bull, "distribution" (selling) → bear
export function volumeTrendTone(s) {
  const t = String(s || "").toLowerCase();
  if (t.includes("accumulation")) return "bull";
  if (t.includes("distribution")) return "bear";
  return "neutral";
}
