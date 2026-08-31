// attention.js — pure ranking for the Today workspace's "Needs attention" list.
//
// Kept out of the component so the rule is inspectable and so Step 08 can extend it: the "what changed
// since my last review" engine will add signals here (an invalidation condition that fired, a material
// evidence change, a due review date) WITHOUT the Today component changing shape.
//
// What this deliberately does NOT do:
//   * invent a thesis field that does not exist yet — it reads only `status`, `updatedAt` and the single
//     `lastCheck` the journal actually stores today (docs/research-os/CURRENT_STATE_AUDIT.md §4.1);
//   * imply a trade action — every reason is a RESEARCH prompt ("re-read the evidence"), never "sell";
//   * rank by profit or by model direction.

// A check older than this is treated as stale enough to re-run. One week: long enough not to nag, short
// enough that a thesis does not drift unexamined through an earnings cycle.
export const STALE_CHECK_SECONDS = 7 * 24 * 3600;

// REASONS — the only reasons a thesis can appear. `tone` maps to the existing Tag tones.
const REASONS = {
  challenged: {
    priority: 0,
    tone: "down",
    label: "Evidence challenges it",
    action: "Re-read the evidence",
  },
  unchecked: {
    priority: 1,
    tone: "warn",
    label: "Never checked",
    action: "Run an evidence check",
  },
  stale: {
    priority: 2,
    tone: "outline",
    label: "Check is stale",
    action: "Re-check against today's facts",
  },
};

// rankTheses turns the journal's active theses into attention items, most-urgent first.
// `nowSec` is injected so the ordering is deterministic and testable.
// Returns [{ id, ticker, text, reason, tone, label, action, checkedAt, verdict }].
export function rankTheses(theses, nowSec = Math.floor(Date.now() / 1000)) {
  const items = [];
  for (const t of theses || []) {
    if (!t || t.status !== "active") continue;
    const check = t.lastCheck || null;
    let key = null;
    if (!check) key = "unchecked";
    else if (check.verdict === "challenged") key = "challenged";
    else if (nowSec - Number(check.at || 0) > STALE_CHECK_SECONDS) key = "stale";
    if (!key) continue; // confirming / neutral and recently checked -> nothing to do
    const r = REASONS[key];
    items.push({
      id: t.id,
      ticker: t.ticker,
      text: t.text || "",
      reason: key,
      tone: r.tone,
      label: r.label,
      action: r.action,
      checkedAt: check?.at || null,
      verdict: check?.verdict || null,
      summary: check?.summary || "",
    });
  }
  // Most urgent first; within a reason, the oldest check first (never-checked sorts before old checks).
  return items.sort(
    (a, b) =>
      REASONS[a.reason].priority - REASONS[b.reason].priority ||
      (a.checkedAt || 0) - (b.checkedAt || 0)
  );
}

// countActive is the denominator for "3 of 5 active theses need attention".
export function countActive(theses) {
  return (theses || []).filter((t) => t?.status === "active").length;
}

// recentEvents keeps the newest alert events within a window, capped. Alerts already carry a neutral,
// descriptive message (alerts/notify.go); Today only orders and trims them.
export function recentEvents(events, { limit = 4, withinSeconds = 3 * 24 * 3600, nowSec = Math.floor(Date.now() / 1000) } = {}) {
  return (events || [])
    .filter((e) => e && Number(e.ts) > nowSec - withinSeconds)
    .sort((a, b) => Number(b.ts) - Number(a.ts))
    .slice(0, limit);
}
