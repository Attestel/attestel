// captureItems.js — builds the list of savable objects for each Evidence category.
//
// Pure functions over the dashboard payload the section is ALREADY rendering. They produce SELECTORS
// plus a preview of what the user is looking at; they never produce provenance. The server resolves
// each selector against its own data and stamps `provider`, `dataState`, `sourceTime` and
// `attribution` — so if the payload on screen has gone stale, the save either records the current
// truth or is refused (`source_stale` / `value_unavailable`). It never records the stale value with a
// fresh-looking timestamp.
//
// The `dataState` on each item is the badge shown BEFORE the save, derived from the same response
// that produced the value. It is mirrored, not authoritative: the stored record carries the server's.

// ---- chart context (deterministic) --------------------------------------------------------------

// SAVABLE_INDICATORS mirrors the gateway's closed list. Keeping the two in step is deliberate: a
// selector the server will not resolve should never be offered.
const SAVABLE_INDICATORS = [
  { field: "rsi", label: "RSI(14)", unit: "" },
  { field: "macdHist", label: "MACD histogram", unit: "" },
  { field: "adx", label: "ADX(14)", unit: "" },
  { field: "stochK", label: "Stochastic %K", unit: "" },
  { field: "atr", label: "ATR(14)", unit: "USD" },
  { field: "sma50", label: "SMA(50)", unit: "USD" },
  { field: "sma200", label: "SMA(200)", unit: "USD" },
  { field: "ema20", label: "EMA(20)", unit: "USD" },
  { field: "vwap", label: "VWAP", unit: "USD" },
];

const SAVABLE_REGIME_FIELDS = [
  { field: "trend", label: "Trend" },
  { field: "shortTermTrend", label: "Short-term trend" },
  { field: "momentum", label: "Momentum" },
  { field: "macdState", label: "MACD state" },
  { field: "volatility", label: "Volatility" },
  { field: "trendStrength", label: "Trend strength" },
  { field: "priceVsBands", label: "Price vs Bollinger bands" },
  { field: "volumeTrend", label: "Volume trend" },
];

const CONFLUENCE_FIELDS = [
  { field: "trendAlignment", label: "Trend alignment across timeframes" },
  { field: "conflicts", label: "Cross-timeframe conflicts" },
  { field: "note", label: "Confluence note" },
];

function priceState(data) {
  return data?.priceSourceIsSynthetic ? "synthetic" : "live";
}

function asOfLabel(data) {
  const raw = data?.price?.asOf || "";
  return raw.length >= 10 ? raw.slice(0, 10) : raw || "date unknown";
}

function flat(v) {
  if (Array.isArray(v)) return v.length ? v.join("; ") : "none";
  return v === null || v === undefined ? "" : String(v);
}

export function chartItems(data, ticker, timeframe) {
  if (!data) return [];
  const latest = data.indicators?.latest || {};
  const regime = data.regime || {};
  const dataState = priceState(data);
  const asOf = asOfLabel(data);
  const items = [];

  const factPreview = (label, value, unit) => [
    { label: "Value", value: `${label} = ${flat(value)}${unit ? ` ${unit}` : ""}`, mono: true },
    { label: "Computed", value: `${asOf} · ${timeframe} chart` },
    { label: "Source", value: `analysis service · ${data.priceSource || "unknown"} price data` },
  ];

  SAVABLE_INDICATORS.forEach(({ field, label, unit }) => {
    const value = latest[field];
    if (value === null || value === undefined) return; // not enough bars yet — nothing to claim
    items.push({
      key: `ind:${field}`,
      kind: "deterministic_fact",
      selector: { field },
      title: `${label} = ${flat(value)}${unit ? ` ${unit}` : ""}`,
      subtitle: `computed ${asOf} · ${timeframe}`,
      dataState,
      preview: factPreview(label, value, unit),
    });
  });

  SAVABLE_REGIME_FIELDS.forEach(({ field, label }) => {
    const value = regime[field];
    if (value === null || value === undefined) return;
    items.push({
      key: `reg:${field}`,
      kind: "deterministic_fact",
      selector: { field },
      title: `${label}: ${flat(value)}`,
      subtitle: `classified ${asOf} · ${timeframe}`,
      dataState,
      preview: factPreview(label, value, ""),
    });
  });

  ["support", "resistance"].forEach((side) => {
    (regime.keyLevels?.[side] || []).forEach((level, index) => {
      items.push({
        key: `lvl:${side}:${index}`,
        kind: "chart_context",
        selector: { level: side, index },
        title: `Computed ${side} at ${level}`,
        subtitle: `swing level · ${asOf} · ${timeframe}`,
        dataState,
        preview: [
          { label: "Level", value: `${side} = ${level} USD`, mono: true },
          { label: "Computed", value: `${asOf} · ${timeframe} chart` },
          {
            label: "How",
            value: "Derived from recent local swing highs and lows by the analysis service — never model-generated.",
          },
        ],
      });
    });
  });

  if (data.confluence) {
    CONFLUENCE_FIELDS.forEach(({ field, label }) => {
      const value = data.confluence[field];
      if (value === null || value === undefined) return;
      if (Array.isArray(value) && value.length === 0) return;
      items.push({
        key: `conf:${field}`,
        kind: "chart_context",
        selector: { field },
        title: `${label}: ${flat(value)}`,
        subtitle: "cross-timeframe observation",
        dataState,
        preview: [
          { label: "Observation", value: flat(value) },
          { label: "Computed", value: `${asOf} · compared across 1D, 1H, 15m` },
          {
            label: "Note",
            value:
              "A cross-timeframe reading has no single timeframe. It is filed against the chart you are on, and the compared set is recorded with it.",
          },
        ],
      });
    });
  }

  return items;
}

// ---- fundamentals ------------------------------------------------------------------------------

// SAVABLE_QUARTER_FIELDS is a curated set of the metrics worth citing, by dotted path into a seeded
// quarter. Anything absent from the payload is simply not offered.
const SAVABLE_QUARTER_FIELDS = [
  { path: "revenue.actual", label: "Revenue" },
  { path: "revenue.yoyPct", label: "Revenue YoY %" },
  { path: "dataCenter.actual", label: "Data center revenue" },
  { path: "dataCenter.yoyPct", label: "Data center YoY %" },
  { path: "epsNonGaap.actual", label: "EPS (non-GAAP)" },
  { path: "epsGaap.actual", label: "EPS (GAAP)" },
  { path: "grossMargin.nonGaap", label: "Gross margin (non-GAAP)" },
  { path: "nextGuide.revenue", label: "Next-quarter revenue guide" },
];

function dotted(obj, path) {
  return path.split(".").reduce((acc, k) => (acc && typeof acc === "object" ? acc[k] : undefined), obj);
}

export function fundamentalsItems(data, ticker) {
  const items = [];
  const quarters = data?.fundamentals?.quarters || [];
  const latest = quarters.find((q) => q.isLatest) || quarters[0];

  if (latest) {
    SAVABLE_QUARTER_FIELDS.forEach(({ path, label }) => {
      const value = dotted(latest, path);
      if (value === null || value === undefined) return;
      const unit = dotted(latest, `${path.split(".")[0]}.unit`) || "";
      items.push({
        key: `fund:${latest.label}:${path}`,
        kind: "fundamental_fact",
        selector: { quarter: latest.label, field: path },
        title: `${latest.label} · ${label} = ${flat(value)}${unit ? ` ${unit}` : ""}`,
        subtitle: `period ended ${latest.periodEnded} · reported ${latest.reported}`,
        dataState: "seed",
        preview: [
          { label: "Value", value: `${path} = ${flat(value)}${unit ? ` ${unit}` : ""}`, mono: true },
          { label: "Period", value: `${latest.label} · ended ${latest.periodEnded}` },
          { label: "Source", value: "Verified reference dataset (docs/NVIDIA_RESEARCH.md)" },
        ],
      });
    });
  }

  // The EPS beat/miss series, live when a key is present and seed-derived otherwise.
  const earnings = data?.earnings || [];
  const earningsLive = String(data?.earningsSource || "").includes("alphavantage");
  const point = earnings[0];
  if (point) {
    [
      { field: "reportedEPS", label: "Reported EPS" },
      { field: "estimatedEPS", label: "Consensus EPS estimate" },
      { field: "surprisePercentage", label: "EPS surprise %" },
    ].forEach(({ field, label }) => {
      const value = point[field];
      if (value === null || value === undefined) return;
      items.push({
        key: `eps:${point.fiscalDateEnding}:${field}`,
        kind: "fundamental_fact",
        selector: { earnings: "latest", field },
        title: `${label} = ${flat(value)} (${point.fiscalDateEnding})`,
        subtitle: `reported ${point.reportedDate || "date unknown"}`,
        dataState: earningsLive ? "live" : "seed",
        preview: [
          { label: "Value", value: `earnings.${field} = ${flat(value)}`, mono: true },
          { label: "Fiscal period", value: String(point.fiscalDateEnding || "unknown") },
          { label: "Source", value: earningsLive ? "Alpha Vantage EARNINGS" : "Verified reference dataset" },
        ],
      });
    });
  }

  return items;
}

// ---- news and filings ---------------------------------------------------------------------------

function newsItemState(item) {
  if (item.category === "filing") return "live";
  if (String(item.source || "").toLowerCase().includes("(seed)")) return "seed";
  return "live";
}

export function newsItems(data, limit = 12) {
  const items = data?.news || [];
  return items
    .filter((it) => it.url) // no URL means nothing citable, so it is not offered
    .slice(0, limit)
    .map((it, i) => {
      const isFiling = it.category === "filing";
      return {
        key: `news:${it.url}:${i}`,
        kind: isFiling ? "filing" : "news",
        selector: { url: it.url },
        title: it.headline || it.url,
        subtitle: `${it.source || "source unknown"} · ${it.publishedAt || "date unknown"}`,
        dataState: newsItemState(it),
        preview: [
          { label: "Headline", value: it.headline || "(none)" },
          { label: "Published", value: it.publishedAt || "unknown — will be stored as unknown" },
          { label: "Outlet", value: it.source || "unknown" },
          { label: "Link", value: it.url },
          {
            label: "Stored text",
            value: isFiling
              ? "The filing reference and its accession number. The document body is not fetched or stored."
              : "The headline and the link. The article body is not fetched or stored.",
          },
        ],
        excerptNote:
          "Only the link, the headline and the provider's own metadata are stored. The gateway does not fetch article text, and fetched page text may never be persisted as evidence.",
      };
    });
}

// ---- catalysts and roadmap ----------------------------------------------------------------------

export function catalystItems(data) {
  const out = [];
  const events = data?.catalysts?.events || [];
  const roadmap = data?.catalysts?.roadmap || [];

  events.forEach((e, index) => {
    if (!e?.title) return;
    out.push({
      key: `cat:events:${index}`,
      kind: "catalyst",
      selector: { group: "events", index },
      title: `${e.date || "date unknown"} · ${e.title}`,
      subtitle: `${e.type || "event"} · impact ${e.impact || "unrated"}`,
      dataState: "seed",
      preview: [
        { label: "Catalyst", value: e.title },
        { label: "Date", value: e.date || "unknown" },
        { label: "Impact", value: e.impact || "unrated" },
        { label: "Note", value: e.note || "" },
        { label: "Source", value: "Verified reference dataset (docs/NVIDIA_RESEARCH.md)" },
      ],
    });
  });

  roadmap.forEach((r, index) => {
    if (!r?.gen) return;
    out.push({
      key: `cat:roadmap:${index}`,
      kind: "catalyst",
      selector: { group: "roadmap", index },
      title: `${r.gen} · ${r.status || "status unknown"} (${r.year || "year unknown"})`,
      subtitle: r.products || "",
      dataState: "seed",
      preview: [
        { label: "Generation", value: `${r.gen} — ${r.products || ""}` },
        { label: "Status", value: r.status || "unknown" },
        { label: "Year", value: String(r.year || "unknown") },
        { label: "Note", value: r.note || "" },
        {
          label: "Dating",
          value:
            "The roadmap is dated by year only, so this is stored with year-level dating and no precise event time.",
        },
      ],
    });
  });

  return out;
}

export default { chartItems, fundamentalsItems, newsItems, catalystItems };
