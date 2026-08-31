// Number/format helpers for the terminal UI. Kept UI-only (no API coupling). The api.js module
// still owns fmtPct/fmtMoney/fmtB used by existing panels; these are additive.

// Price with thousands separators and fixed decimals — "$1,234.56".
export function fmtPrice(n, d = 2) {
  if (n == null || !Number.isFinite(Number(n))) return "—";
  return `$${Number(n).toLocaleString("en-US", { minimumFractionDigits: d, maximumFractionDigits: d })}`;
}

// Bare number with thousands separators.
export function fmtNum(n, d = 2) {
  if (n == null || !Number.isFinite(Number(n))) return "—";
  return Number(n).toLocaleString("en-US", { minimumFractionDigits: d, maximumFractionDigits: d });
}

// Signed percent — "+1.2%" / "-0.4%". decimals default 2.
export function fmtSignedPct(n, d = 2) {
  if (n == null || !Number.isFinite(Number(n))) return "—";
  const v = Number(n);
  return `${v >= 0 ? "+" : ""}${v.toFixed(d)}%`;
}

// Compact volume — 1.2K / 3.4M / 5.6B.
export function fmtVol(n) {
  if (n == null || !Number.isFinite(Number(n))) return "—";
  const v = Number(n);
  const abs = Math.abs(v);
  if (abs >= 1e9) return `${(v / 1e9).toFixed(2)}B`;
  if (abs >= 1e6) return `${(v / 1e6).toFixed(2)}M`;
  if (abs >= 1e3) return `${(v / 1e3).toFixed(1)}K`;
  return String(Math.round(v));
}

// Fraction (0..1) → "62%".
export function fmtRatioPct(n, d = 0) {
  if (n == null || !Number.isFinite(Number(n))) return "—";
  return `${(Number(n) * 100).toFixed(d)}%`;
}

// Tailwind text-color class from a signed number (or null → muted).
export function deltaColor(n) {
  if (n == null || !Number.isFinite(Number(n))) return "text-muted";
  return Number(n) >= 0 ? "text-up" : "text-down";
}

// -1 / 0 / +1 sign for flash direction.
export function sign(n) {
  if (n == null || !Number.isFinite(Number(n))) return 0;
  return Number(n) > 0 ? 1 : Number(n) < 0 ? -1 : 0;
}

// relFromMs — coarse relative age from an epoch-milliseconds value. Source age matters on the Today
// workspace (a 4-hour-old brief is a different thing from a fresh one), so it is rendered next to every
// dated item rather than left implicit.
export function relFromMs(ms) {
  if (!ms || !Number.isFinite(Number(ms))) return "";
  const s = Math.max(0, Math.floor((Date.now() - Number(ms)) / 1000));
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

// relFromUnix — same, for the unix-SECONDS timestamps the Go services use (alert events, thesis checks).
export function relFromUnix(sec) {
  return sec ? relFromMs(Number(sec) * 1000) : "";
}

// relFromISO — same, for the RFC3339 strings the providers and gateway return (news publishedAt, asOf).
export function relFromISO(iso) {
  if (!iso) return "";
  const t = Date.parse(iso);
  return Number.isNaN(t) ? "" : relFromMs(t);
}

// daysUntil — whole days from today (local) to a "YYYY-MM-DD" date. Negative for the past, null if
// unparseable. Used for "in 3 days" on upcoming events.
export function daysUntil(ymd) {
  const [y, m, d] = String(ymd || "").split("-").map(Number);
  if (!y || !m || !d) return null;
  const then = new Date(y, m - 1, d);
  const now = new Date();
  const midnight = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  return Math.round((then - midnight) / 86400000);
}

// relDay — "today" / "tomorrow" / "in 4 days" / "4 days ago" from a "YYYY-MM-DD" date.
export function relDay(ymd) {
  const n = daysUntil(ymd);
  if (n == null) return "";
  if (n === 0) return "today";
  if (n === 1) return "tomorrow";
  if (n === -1) return "yesterday";
  return n > 0 ? `in ${n} days` : `${-n} days ago`;
}
