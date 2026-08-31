// eventsApi.js — the EVENTS-specific frontend adapter (Wave 0 seam; Wave 4 Lane A owns it next).
//
// A separate module for the same reason monitoringApi.js, thesisApi.js, evidenceApi.js and
// lensApi.js are: `lib/api.js` is on PARALLEL_CONTRACTS.md's reserved shared-file list, and Wave 4
// Lanes A and B build concurrently. This module owns the event surface and nothing else. It is
// created empty in Wave 0 so that `api.js` is never touched by either lane.
//
// ONE upstream, deliberately: the GATEWAY, for every route.
//   * `/api/following`, `/api/explore`, `/api/events/{id}` — contract §8 and §9.2;
//   * `/api/subscriptions*` and `/api/event-state` — also the gateway, per contract §9.3. Journal
//     verifies the session cookie itself, but the frontend still does not call :8096 directly:
//     the gateway is the sole aggregator and the only service documented as wrapping withCORS.
//     The Wave 1 integrator builds `gateway/subscriptions.go` as a cookie-forwarding proxy to
//     JOURNAL_URL, exactly as handleEvidenceProxy already does for journal evidence.
//   * `/api/changed` — contract §9.1: owned by Wave 5 Lane B alone (LANDED), and it returns
//     `gateway/changes.go`'s existing ChangeItem vocabulary. No eighth kind, no parallel struct.
// There is deliberately NO journal base URL here. monitoringApi.js has an ALERTS_BASE because
// alerts is reached directly; events is not that shape.
//
// What this module cannot do, by construction:
//   * it cannot write an event — the corpus is global and the only writer is the ingester in
//     services/events; nothing here POSTs to the event store;
//   * it cannot invent a score — `importance`, `novelty` and `relevance` are computed
//     deterministically server-side (contract §3.6, §9.15) and are read-only to the browser;
//   * it must never call a model on a page load (CLAUDE.md invariant #4). Enrichment is a
//     background, off-by-default worker (D-25); no function here may trigger one.
//
// Wave 0 shipped the plumbing and ZERO endpoint functions. Wave 4 Lane 4A adds them below:
// the three feed reads, the subscription and event-state writes (all through the gateway proxy),
// and the pure display helpers that keep every event surface speaking one vocabulary.

import { AuthRequiredError } from "./api.js";

export { AuthRequiredError };

const BASE = import.meta.env.VITE_GATEWAY_URL || "";

// EventsError carries the server's machine code so a caller can explain a refusal instead of
// showing "request failed". Byte-for-byte the MonitoringError contract.
export class EventsError extends Error {
  constructor(message, { code = "", status = 0 } = {}) {
    super(message);
    this.name = "EventsError";
    this.code = code;
    this.status = status;
  }
}

// request is the shared fetch wrapper: session cookie always sent, 401 raised as AuthRequiredError
// so the shared guest-routing UI can react to it, everything else as EventsError.
export async function request(url, opts = {}) {
  const res = await fetch(url, { credentials: "include", ...opts });
  if (res.status === 401) throw new AuthRequiredError("sign in required");
  let body = null;
  try {
    body = await res.json();
  } catch {
    body = null;
  }
  if (!res.ok) {
    throw new EventsError(body?.error || `request failed (${res.status})`, {
      code: body?.code || "",
      status: res.status,
    });
  }
  return body ?? {};
}

// json builds a POST init. Kept here rather than in each caller so every write in this module sends
// the same headers.
export const json = (body) => ({
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify(body),
});

export { BASE };

// ---- query building -----------------------------------------------------------------------------

// qs omits empty values ENTIRELY rather than sending them as "". The gateway ignores an unknown
// filter either way, but `?types=&since=` and `?` are different cache keys for the same question,
// and fragmenting the cache is how a feed starts serving two answers.
function qs(params) {
  const out = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v == null) continue;
    const s = Array.isArray(v) ? v.filter(Boolean).join(",") : String(v);
    if (s === "" || s === "0") continue; // 0 is every filter's "no filter" in this module
    out.set(k, s);
  }
  const str = out.toString();
  return str ? `?${str}` : "";
}

// ---- feeds (gateway) ----------------------------------------------------------------------------

// fetchFollowing returns the body VERBATIM — items, counts, tickers, cursor, asOf and degraded.
// Nothing is normalised away: `counts` describes the whole matched set before paging, `degraded`
// is the honesty signal the UI is required to render, and Lane 4C reads fields this lane does not.
export function fetchFollowing({
  tickers = [],
  types = [],
  minImportance = 0,
  since = 0,
  limit = 50,
  cursor = "",
} = {}) {
  return request(`${BASE}/api/following${qs({ tickers, types, minImportance, since, limit, cursor })}`);
}

// fetchExplore is Lane 4B's read. It lives here because `eventsApi.js` is the one event adapter and
// 4B may not create a second one.
export function fetchExplore({ section = "", limit = 20 } = {}) {
  return request(`${BASE}/api/explore${qs({ section, limit })}`);
}

// Company-level research leads materialized by the model-free Discovery Scout. This read returns
// a stored snapshot; it never starts intake, analysis, prediction, or Qwen work.
export function fetchScout({ limit = 12 } = {}) {
  return request(`${BASE}/api/scout${qs({ limit })}`);
}

// Versioned, completed-bar research setups. This reads a PostgreSQL snapshot through the gateway;
// it never starts a scan, model call, prediction, or paper action from the browser.
export function fetchOpportunities({ limit = 12, ticker = "", state = "" } = {}) {
  return request(`${BASE}/api/opportunities${qs({ limit, ticker, state })}`);
}

// fetchChanged is the doc §16.5 object: change, not volume.
//
// WAVE 5B LANDED IT. This call used to 404 by design — §9.1 assigned `GET /api/changed` to Lane 5B
// alone and `gateway/routeseams_test.go` asserted that it did NOT resolve. That assertion is now
// inverted (same file, same clause, opposite direction), and the route is served by
// `gateway/changed.go`.
//
// The response carries `{documentsProcessed, materialEvents, companiesChanged, items, since,
// companies, counts, empty, truncated, asOf, degraded, materiality}`. Two fields are worth naming
// here because a caller will otherwise re-derive them:
//
//   * `since.basis` is `"requested"` or `"default24h"`. THE COPY RULE TURNS ON IT — a default
//     24-hour window may never be labelled "since your last check", and this is the server saying
//     which one it actually applied rather than the client inferring it.
//   * `materiality` carries the THRESHOLDS behind the counts (`importanceMin`, `movePct`). No
//     client keeps its own copy: two lanes hand-copied `importanceHighMin` into JavaScript in
//     Wave 4 and one copy had already drifted (§AD-8).
export function fetchChanged({ since = 0 } = {}) {
  return request(`${BASE}/api/changed${qs({ since })}`);
}

// changedUnavailable distinguishes a 404 on this path from a real failure.
//
// Its ORIGINAL reason is gone: the route exists, so a 404 is no longer the roadmap fact it was at
// Wave 4. The function stays because its CALLER-FACING meaning does not depend on why the route is
// missing — an absent change feed must never render as "nothing changed", which would be a false
// statement about the user's companies. It now means "the feed could not be reached", and
// `WhatChangedPanel` says exactly that.
export function changedUnavailable(err) {
  return err instanceof EventsError && err.status === 404;
}

// fetchEvent is the §9.2 read-through on the GATEWAY. Never :8004 — the frontend has no events base
// URL by construction, which is why one cannot be typed here by accident.
export function fetchEvent(eventId) {
  return request(`${BASE}/api/events/${encodeURIComponent(eventId)}`);
}

// ---- subscriptions (journal-backed, via the gateway proxy, credentialed) ---- contract §2.1, §9.3

export async function listSubscriptions() {
  const { subscriptions = [] } = await request(`${BASE}/api/subscriptions`);
  return subscriptions;
}

// followTicker is IDEMPOTENT AT THE SERVER: §2.1 returns 200 with the existing record when the
// ticker is already followed. There is deliberately no client-side dedup here — swallowing that
// response would hide the real subscription (its id, its notificationLevel) from the caller that
// asked for it.
export async function followTicker(ticker, { notificationLevel = "material", source = "manual" } = {}) {
  const { subscription } = await request(
    `${BASE}/api/subscriptions`,
    json({ ticker, notificationLevel, source }),
  );
  return subscription;
}

export async function updateSubscription(id, patch) {
  const { subscription } = await request(`${BASE}/api/subscriptions/${encodeURIComponent(id)}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  return subscription;
}

export function unfollowTicker(id) {
  return request(`${BASE}/api/subscriptions/${encodeURIComponent(id)}`, { method: "DELETE" });
}

// ---- per-user event state (journal-backed, via the gateway proxy) ---------- contract §2.3, §9.3

// fetchEventStates reads read/saved/dismissed for a batch of ids. An empty id list short-circuits:
// `GET /event-state?ids=` would ask the server for the state of nothing.
export async function fetchEventStates(ids = []) {
  const list = (ids || []).filter(Boolean);
  if (!list.length) return [];
  const { states = [] } = await request(`${BASE}/api/event-state${qs({ ids: list })}`);
  return states;
}

// setEventState writes one event's state. Marking read is a USER ACTION — fired when the event is
// opened, never on scroll and never on render.
export async function setEventState(eventId, patch = {}) {
  const { state } = await request(`${BASE}/api/event-state`, json({ eventId, ...patch }));
  return state;
}

// ---- display helpers — pure, no fetch, no side effects ------------------------------------------

// EVENT_TYPE_LABELS covers all sixteen §3.4 values. `eventTypeLabel` falls through to the RAW
// STRING for anything else rather than hiding it: a type this UI cannot name is still an event, and
// silently dropping it is how a feed starts lying by omission. Same posture as
// monitoringApi.js::kindLabel.
export const EVENT_TYPE_LABELS = {
  earnings_result: "Earnings result",
  earnings_guidance: "Earnings guidance",
  analyst_revision: "Analyst revision",
  product_launch: "Product launch",
  management_change: "Management change",
  ma_transaction: "M&A transaction",
  regulatory_action: "Regulatory action",
  legal_action: "Legal action",
  supply_chain: "Supply chain",
  capital_return: "Capital return",
  financing: "Financing",
  partnership: "Partnership",
  macro_release: "Macro release",
  central_bank: "Central bank",
  sector_event: "Sector event",
  other: "Other",
};

export const eventTypeLabel = (type) => EVENT_TYPE_LABELS[type] || type || "";

export const SOURCE_TIER_LABELS = {
  official: "Official",
  professional: "Professional",
  discussion: "Discussion",
};

export const sourceTierLabel = (tier) => SOURCE_TIER_LABELS[tier] || tier || "";

export const HORIZON_LABELS = {
  intraday: "Intraday",
  short_term: "Short term",
  medium_term: "Medium term",
  long_term: "Long term",
};

export const horizonLabel = (h) => HORIZON_LABELS[h] || h || "";

// §3.8's eleven affected dimensions. Lives here rather than beside its one renderer because it is
// contract vocabulary, and contract vocabulary in a component file is the first copy of two.
export const DIMENSION_LABELS = {
  revenue_exposure: "Revenue exposure",
  margin: "Margin",
  growth: "Growth",
  regulation: "Regulation",
  management: "Management",
  litigation: "Litigation",
  valuation: "Valuation",
  demand: "Demand",
  supply: "Supply",
  rates: "Rates",
  currency: "Currency",
};

export const dimensionLabel = (d) => DIMENSION_LABELS[d] || String(d || "").replace(/_/g, " ");

// ── The importance band: SERVED, not re-derived ────────────────────────────────────────────────
//
// This block used to declare `IMPORTANCE_MATERIAL = 0.4` and `IMPORTANCE_HIGH = 0.7` — a second,
// hand-synced copy of `gateway/feeds.go`'s `importanceMediumMin` / `importanceHighMin`, kept in
// step across a language boundary by nothing but attention. Lane 4A stated the drift risk in this
// file: NOTHING FAILED IF ONE MOVED AND THE OTHER DID NOT. Lane 4B's copy had already drifted, to
// 0.45 / 0.75, which would have printed "High impact" on a card whose own server-authored reason
// string read "High market importance" at 0.72 — the surface disagreeing with the server that fed
// it.
//
// The gateway now serves the band it computes (`importanceBand` on every feed and explore item), so
// the constants are GONE and there is nothing left to drift. The client reads a word; it never sees
// a threshold.

// MIN_IMPORTANCE_* are REQUEST VALUES, not display bands, and the distinction is the whole point of
// this comment. `GET /api/following` filters on a `minImportance` FLOAT — there is no band-name
// form of that parameter — so a client offering "High impact only" has to name the number the
// server compares against. That is a genuine remaining coupling and it is declared here rather than
// hidden inside a filter list.
//
// It is a much smaller coupling than the one it replaced. Nothing RENDERS from these: a drift here
// changes which items a filter asks for, visibly and in one place, and can never make a card's band
// disagree with the header that counts it — which is what the old display-side copies could do.
// If `/api/following` ever accepts a band name, delete these and send the word.
export const MIN_IMPORTANCE_MATERIAL = 0.4;
export const MIN_IMPORTANCE_HIGH = 0.7;

// isScored says whether the server actually computed an importance for this item.
//
// `gateway/feeds.go` sends `importance`, `novelty`, `documentCount`, `relevance` and
// `potentialStrength` as POINTERS with `omitempty`: on the degraded cascade — where the events
// service that computes them is the thing that is down — the KEY IS ABSENT. Absent is not zero.
// Reading a missing importance as 0.0 would render "scored, and scored lowest" for an item nobody
// ever scored, so every caller asks this first.
export const isScored = (event) => typeof event?.importance === "number";

// BANDS is the server's vocabulary, not a translation of it. `gateway/feeds.go::importanceBand`
// returns exactly these three words and `counts.byImportance` is keyed by them.
export const BANDS = ["high", "medium", "low"];

// importanceBand READS the served band. It computes nothing.
//
// UNSCORED FALLS TO "low" — never to "high" — so a degraded feed can never manufacture a HIGH
// IMPACT band out of a missing number; pair it with `isScored` when the distinction between
// "scored lowest" and "never scored" matters to what is drawn.
//
// It takes the EVENT, never a float. There is deliberately no number-accepting overload: with the
// thresholds gone this module cannot band a bare float, and an overload that quietly returned "low"
// for 0.83 would be the drift it replaced, wearing a different hat. Two lanes called it with
// `event.importance`; both call sites now pass the event.
export function importanceBand(event) {
  const served = event?.importanceBand;
  if (typeof served === "string" && BANDS.includes(served)) return served;
  return "low";
}

// BAND_LABELS, BAND_TONE and BAND_STEP are keyed by the SERVER's three band words. They are
// presentation only — a word, a design-system tone and a 1..3 meter step. None of them is a
// threshold, and none of them can drift out of step with the gateway, because none of them knows
// what a threshold is.
export const BAND_LABELS = { high: "High impact", medium: "Material", low: "Routine" };
export const BAND_TONE = { high: "caution", medium: "info", low: "neutral" };
export const BAND_STEP = { high: 3, medium: 2, low: 1 };

// UNSCORED_BAND_LABEL is what a card says when the server never scored the item at all — a
// different statement from "scored, and scored lowest", and `isScored` is how a caller tells them
// apart.
export const UNSCORED_BAND_LABEL = "Impact not scored";

// directionTone maps a §3.8 potential_direction onto the design system's data colours.
//
// `neutral`, `unclear` and null ALL resolve to "muted", and that is deliberate: an absent direction
// means nobody could determine one, not that the effect is zero. Severity never enters here —
// `high impact` is `warn`, and a high-impact POSITIVE event that rendered red would be a bug.
export function directionTone(direction) {
  if (direction === "positive") return "up";
  if (direction === "negative") return "down";
  return "muted";
}

export const DIRECTION_LABELS = {
  positive: "Positive",
  negative: "Negative",
  neutral: "Neutral",
  unclear: "Unclear",
};

export const directionLabel = (d) => DIRECTION_LABELS[d] || (d ? String(d) : "");

// DIRECTION_STATUS_TONE bridges `directionTone`'s design-TOKEN vocabulary (`up`/`down`/`muted`, the
// Tailwind colour names) onto `components/ui/Status.jsx`'s TONES keys (`bull`/`bear`/`neutral`).
// They are the same three colours under two names, and `StatusPill` silently falls back to neutral
// on a name it does not know — so a positive direction handed the token name would have rendered
// GREY rather than green, with nothing failing anywhere. One rule, two vocabularies, one map.
export const DIRECTION_STATUS_TONE = { up: "bull", down: "bear", muted: "neutral" };

// strengthWord and relevanceWord turn a float into a WORD. There is no numeric escape hatch in this
// module by design (AD-8): `potentialStrength` and `relevance` are model- and code-authored scores
// with no calibration behind them, and rendering "0.8" or "80%" would claim a precision that does
// not exist. Absent ⇒ null ⇒ the caller renders nothing at all.
//
// UNLIKE `importance`, THESE TWO ARE STILL BANDED HERE, and that is a stated gap rather than a
// choice. The gateway bands importance and serves the word; it bands neither `relevance` nor
// `potentialStrength` and declares no threshold for either — measured, not assumed:
// `grep -rn '0\.66|0\.33|relevanceMin|strengthMin' gateway/ services/events/` returns nothing.
// So these thresholds have no server constant to defer to. They are 0.7 / 0.4 because Lane 4A's
// were (Lane 4B's second copy used 0.66 / 0.33; one definition survives, and it is this one), and
// they are named separately from importance so that nobody reads the shared number as a shared
// rule. **When a wave gives the server a relevance or strength band, delete these and read it.**
const STRENGTH_STRONG_MIN = 0.7;
const STRENGTH_MODERATE_MIN = 0.4;
const RELEVANCE_DIRECT_MIN = 0.7;
const RELEVANCE_RELATED_MIN = 0.4;

export function strengthWord(strength) {
  if (typeof strength !== "number") return null;
  if (strength >= STRENGTH_STRONG_MIN) return "strong";
  if (strength >= STRENGTH_MODERATE_MIN) return "moderate";
  return "slight";
}

export function relevanceWord(relevance) {
  if (typeof relevance !== "number") return null;
  if (relevance >= RELEVANCE_DIRECT_MIN) return "direct";
  if (relevance >= RELEVANCE_RELATED_MIN) return "related";
  return "peripheral";
}

// The 1..3 meter step that goes with each word, so a renderer that draws a StepMeter does not
// re-band the float it was handed. Keyed by the word, so there is one place the words are listed.
export const STRENGTH_STEP = { strong: 3, moderate: 2, slight: 1 };
export const RELEVANCE_STEP = { direct: 3, related: 2, peripheral: 1 };

// relativeTime accepts either an ISO string or the unix seconds `gateway/feeds.go` sends alongside
// it. Unparseable or absent ⇒ "" so a caller renders nothing rather than "Invalid Date".
export function relativeTime(value) {
  if (value == null || value === "") return "";
  const ms = typeof value === "number" ? value * 1000 : Date.parse(value);
  if (!Number.isFinite(ms) || ms === 0) return "";
  const secs = (Date.now() - ms) / 1000;
  if (secs < 0) return "just now"; // clock skew reads as now, never as "in 3 minutes"
  if (secs < 60) return "just now";
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins} min ago`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `${hrs} ${hrs === 1 ? "hour" : "hours"} ago`;
  const days = Math.floor(hrs / 24);
  if (days < 7) return `${days} ${days === 1 ? "day" : "days"} ago`;
  const weeks = Math.floor(days / 7);
  if (weeks < 5) return `${weeks} ${weeks === 1 ? "week" : "weeks"} ago`;
  const months = Math.floor(days / 30);
  if (months < 12) return `${months} ${months === 1 ? "month" : "months"} ago`;
  return `${Math.floor(days / 365)}y ago`;
}

// absoluteTime — `occurredAt` and `publishedAt` differ, and the difference is the point: an event
// that happened on Friday and surfaced on Monday is a different object from one that surfaced live.
// Accepts the same ISO-or-unix-seconds pair `relativeTime` does, for the same reason.
export function absoluteTime(value) {
  if (value == null || value === "") return "not recorded";
  const ms = typeof value === "number" ? value * 1000 : Date.parse(value);
  if (!Number.isFinite(ms)) return "not recorded";
  return new Date(ms).toISOString().replace("T", " ").slice(0, 16) + " UTC";
}

// ANALYST_DISCLAIMER IS DELIBERATELY NOT DECLARED IN THIS FILE.
//
// §9.18's string has two copies — `services/llm/app/analyst.py` and `gateway/explore.go`'s
// `analystDisclaimer` — and `services/llm/tests/test_analyst.py` asserts they agree BYTE FOR BYTE.
// Lane 4A shipped a third here and said so itself: nothing asserted it, and an unasserted copy of a
// disclosure string is drift with a deadline.
//
// It is gone. `/api/following` now carries `disclaimer` exactly as `/api/explore` items always did,
// and `GET /api/events/{id}` carries it as a gateway-owned sibling on the proxied envelope — so
// every surface that renders a direction is SENT the words that qualify it, by the one place that
// owns them. A client that composed the sentence itself would satisfy §9.18's letter and destroy
// its point.
//
// WHERE THE SERVER SENT NO DISCLOSURE, THE DIRECTION IS WITHHELD AND THE WITHHOLDING IS STATED.
// Never rendered bare, never silently dropped. `DISCLOSURE_WITHHELD_NOTICE` is that sentence — it
// is not a disclosure and does not stand in for one; it says a disclosure is missing.
export const DISCLOSURE_WITHHELD_NOTICE =
  "Directional read-throughs are hidden here: this response did not carry the required disclosure, " +
  "and a read-through without it would be presented as more than it is.";

// DEGRADED_NOTICES says what is ACTUALLY true for each signal `gateway/feeds.go:85-89` can raise.
// Every string names the consequence to the user's data, not the internal fault.
export const DEGRADED_NOTICES = {
  "events:unreachable":
    "Showing headlines only — the event service is unavailable, so these are not deduplicated into events.",
  "events:unconfigured":
    "Showing headlines only — the event service is not configured, so these are not deduplicated into events.",
  "subscriptions:guest": "Showing the default universe. Sign in to follow companies.",
  "subscriptions:unreachable":
    "Showing the default universe — your follow list could not be loaded.",
  "subscriptions:empty": "You are not following any companies yet.",
};

export const degradedNotice = (code) => DEGRADED_NOTICES[code] || code;
