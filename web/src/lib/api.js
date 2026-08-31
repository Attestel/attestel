const BASE = import.meta.env.VITE_GATEWAY_URL || "";

export const TIMEFRAMES = ["1D", "1H", "15m", "5m"];

// ---- Auth-aware fetch --------------------------------------------------------------------------
// Sessions ride in an httpOnly cookie, so identity-carrying requests MUST send credentials so the
// browser attaches the cookie (and accepts Set-Cookie) — same-origin (Vite proxy) or cross-origin
// (direct localhost). A 401 on a WRITE means "sign in required": we surface it as a typed
// AuthRequiredError so the UI can route the user to the sign-in screen instead of hitting a dead 401.

// AuthRequiredError is thrown when a mutating request is rejected for want of a session.
export class AuthRequiredError extends Error {
  constructor(message = "sign in required") {
    super(message);
    this.name = "AuthRequiredError";
  }
}

// request wraps fetch with credentials:'include'. On a 401 to a non-GET it reads the server message
// and throws AuthRequiredError; everything else passes through unchanged for each caller's own
// ok-handling.
async function request(url, opts = {}) {
  const res = await fetch(url, { credentials: "include", ...opts });
  const method = (opts.method || "GET").toUpperCase();
  if (res.status === 401 && method !== "GET") {
    let msg = "sign in required";
    try {
      const b = await res.clone().json();
      if (b?.error) msg = b.error;
    } catch {
      /* ignore */
    }
    throw new AuthRequiredError(msg);
  }
  return res;
}

// ---- Accounts (auth service on :8099; the whole stack still runs zero-key / offline) -----------
// Email/password + session cookie. Google is deferred (the button hits authGoogle -> 501 -> toast).

const AUTH_BASE = import.meta.env.VITE_AUTH_URL || "http://localhost:8099";

// Auth endpoints use a plain credentialed fetch (NOT `request`): here a 401 is a normal login
// failure ("invalid email or password"), not an AuthRequiredError to route away on.
async function authJSON(path, opts) {
  const res = await fetch(`${AUTH_BASE}${path}`, { credentials: "include", ...opts });
  if (!res.ok) {
    let msg = `auth ${res.status}`;
    try {
      const b = await res.json();
      if (b?.error) msg = b.error;
    } catch {
      /* ignore */
    }
    throw new Error(msg);
  }
  return res.status === 204 ? null : res.json();
}

// authMe hydrates the current session. NEVER throws for a guest — returns { user: null }.
export async function authMe() {
  try {
    return await authJSON("/auth/me");
  } catch {
    return { user: null }; // auth service down -> treat as guest, app stays usable
  }
}

export async function authLogin({ email, password, remember = false }) {
  const data = await authJSON("/auth/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email, password, remember }),
  });
  return data.user;
}

export async function authSignup({ email, password }) {
  const data = await authJSON("/auth/signup", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email, password }),
  });
  return data.user;
}

export async function authLogout() {
  return authJSON("/auth/logout", { method: "POST" });
}

// authGoogle probes whether real Google OAuth is wired (both GOOGLE_CLIENT_ID + GOOGLE_CLIENT_SECRET
// set on the auth service). Side-effect-free, credentialed, NEVER throws — resolves { configured:false }
// when unset/unreachable so the UI can fall back to the "coming soon" toast.
export async function authGoogle() {
  try {
    const res = await fetch(`${AUTH_BASE}/auth/google/config`, { credentials: "include" });
    if (!res.ok) return { configured: false };
    const d = await res.json();
    return { configured: Boolean(d?.configured) };
  } catch {
    return { configured: false };
  }
}

// googleStartURL is the top-level navigation target that BEGINS the OAuth flow (a full-page redirect,
// not a fetch — OAuth needs a browser navigation). returnTo is the app view to land on after login;
// the auth service sanitizes it server-side, so an odd value just falls back to the terminal.
// The default mirrors lib/routes.js DEFAULT_VIEW; it must stay a single slash-free destination id
// because the auth service rejects anything else (see the callers, which pass baseViewOf(...)).
export function googleStartURL(returnTo = "today") {
  return `${AUTH_BASE}/auth/google?returnTo=${encodeURIComponent(returnTo)}`;
}

// ---- Per-user settings (auth service; signal, risk and experience preferences) -----------------
// Server-side per-user preferences so they sync across devices (not localStorage). Credentialed: the
// session cookie scopes them to the signed-in user. Both are guest-gated (the auth service 401s a
// guest); the UI only calls these when signed in. `trainIntervalMinutes` is retained for stored-row
// compatibility but no longer drives a browser timer; model training is an explicit admin action.

// experienceLevel + ackRealityCheck are stored for the beginner-onboarding layer (surfaced by later
// pieces); accountEquity + defaultRiskPct back the position sizer. updateSettings PUTs the whole
// object, so these new keys ride along with no other change.
export const DEFAULT_SETTINGS = {
  trainIntervalMinutes: 0,
  allowShort: false,
  defaultHorizon: 5,
  browserNotifications: false,
  experienceLevel: "beginner",
  ackRealityCheck: false,
  accountEquity: 0,
  defaultRiskPct: 1,
  activeTicker: "NVDA",
};

// getSettings reads the user's saved prefs. Throws AuthRequiredError on a 401 (guest) so callers can
// route to sign-in; the SettingsContext falls back to DEFAULT_SETTINGS for a guest / on any error.
export async function getSettings() {
  const res = await request(`${AUTH_BASE}/auth/settings`);
  if (res.status === 401) throw new AuthRequiredError();
  if (!res.ok) throw new Error(`auth ${res.status}`);
  const data = await res.json();
  return data.settings || DEFAULT_SETTINGS;
}

// updateSettings PUTs the FULL settings object (the server validates every field, so send them all —
// the SettingsContext merges any patch into current state before calling this). A 401 on the PUT
// throws AuthRequiredError from request(); other failures surface the server's {error}.
export async function updateSettings(settings) {
  const res = await request(`${AUTH_BASE}/auth/settings`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(settings),
  });
  if (!res.ok) {
    let msg = `auth ${res.status}`;
    try { const b = await res.json(); if (b?.error) msg = b.error; } catch { /* ignore */ }
    throw new Error(msg);
  }
  const data = await res.json();
  return data.settings;
}

export async function fetchDashboard(ticker = "NVDA", timeframe = "1D") {
  const res = await fetch(`${BASE}/api/dashboard/${ticker}?timeframe=${encodeURIComponent(timeframe)}`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

export async function fetchTickers() {
  const res = await fetch(`${BASE}/api/tickers`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  const data = await res.json();
  return data.tickers || [];
}

export async function searchTickers(query = "", { signal } = {}) {
  const params = new URLSearchParams({ q: query, limit: "7" });
  const res = await fetch(`${BASE}/api/tickers/search?${params}`, { signal });
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  const data = await res.json();
  return data.tickers || [];
}

export async function fetchQuote(ticker = "NVDA") {
  const res = await fetch(`${BASE}/api/quote/${ticker}`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// Catalyst Calendar — a store-backed window of scheduled and observed events. The gateway never
// calls a provider or generates seed dates on this read; degraded responses carry an empty list.
export async function fetchCalendar(from = "", to = "") {
  const q = new URLSearchParams();
  if (from) q.set("from", from);
  if (to) q.set("to", to);
  const qs = q.toString();
  const res = await fetch(`${BASE}/api/calendar${qs ? `?${qs}` : ""}`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// fetchAutomationStatus — Phase 1 operational health for the Settings surface.
//
// READ ONLY, and there is deliberately no companion "run now" call: starting a background job from
// a page would be a model call caused by a UI event, which invariant #4 forbids. Production may
// clock the events service's model-free lanes; model lanes still require `make automate-once`.
//
// Signed-in only; a guest gets 401 and the panel says so rather than rendering a blank green board.
export async function fetchAutomationStatus() {
  const res = await request(`${BASE}/api/automation/status`);
  if (res.status === 401) return { signedOut: true, lanes: [], recentRuns: [] };
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// Read-only ledger for the separate deterministic prediction controller. A read never trains,
// evaluates, shadows or promotes anything; those transitions are owned by the supervised process.
export async function fetchPredictionAutomationStatus() {
  const res = await request(`${BASE}/api/prediction-automation/status`);
  if (res.status === 401) return { signedOut: true, trials: [] };
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// ── Phase 4: stored reactions and empirical sensitivity ──────────────────────────────────────────
//
// Both READ what a capture pass already recorded from stored bars. Neither fetches a bar, and
// neither calls a model.

// fetchReactions — the before/after evidence for one stored event (or a company's recent events).
// Every number arrives with the bars it came from, the bar source, the synthetic flag and the
// calculation version.
export async function fetchReactions({ eventId = "", ticker = "", limit = 20 } = {}) {
  const q = new URLSearchParams();
  if (eventId) q.set("eventId", eventId);
  if (ticker) q.set("ticker", ticker);
  if (limit) q.set("limit", String(limit));
  const res = await fetch(`${BASE}/api/reactions?${q.toString()}`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// fetchSensitivity — the aggregate over matured, non-synthetic reactions.
//
// It can legitimately answer `sufficient: false`, and when it does it carries NO statistic at all.
// A caller must render the refusal, not fall back to something. There is no provisional mode by
// design: a percentage is acted on whatever is printed beside it.
export async function fetchSensitivity({ ticker = "", kind = "", series = "", horizon = "1d" } = {}) {
  const q = new URLSearchParams();
  if (ticker) q.set("ticker", ticker);
  if (kind) q.set("kind", kind);
  if (series) q.set("series", series);
  if (horizon) q.set("horizon", horizon);
  const res = await fetch(`${BASE}/api/sensitivity?${q.toString()}`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// Standalone confluence fetch. The dashboard already embeds `confluence`; this is for a direct
// refresh of just the multi-timeframe picture.
export async function fetchConfluence(ticker = "NVDA") {
  const res = await fetch(`${BASE}/api/confluence/${ticker}`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// StockTwits crowd sentiment (keyless, fail-silent). The dashboard already embeds `sentiment`;
// this is for a direct fetch. Returns { available:false } when the source is down/disabled.
// DESCRIPTIVE crowd color — never a buy/sell signal.
export async function fetchSentiment(ticker = "NVDA") {
  const res = await fetch(`${BASE}/api/sentiment/${ticker}`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// AI Market Brief — the LLM reads + compresses the day's already-collected facts into a structured,
// plain-language digest. AI SYNTHESIS, grounded in those facts; descriptive, NOT a prediction or
// buy/sell recommendation. Returns { structured, modelUsed, warnings, disclaimer, contextUsed }.
export async function fetchBrief(ticker = "NVDA") {
  // Credentialed: the brief merges in the signed-in user's per-thesis checks (guests get none).
  const res = await request(`${BASE}/api/brief/${ticker}`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// Market-wide "what's moving today" brief, aggregated across the tracked universe.
export async function fetchMarketBrief() {
  const res = await request(`${BASE}/api/brief/market`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// Analyst committee — the multi-agent pipeline (3 persona analysts -> bull/bear debate -> judge).
// Stances are analytical LEANS, never buy/sell verdicts; consensus is a deterministic vote computed
// in code, not by the model. On-demand only (never polled) and slow on first run: it makes many
// sequential local-model calls. `refresh=true` busts the gateway cache for an explicit re-run.
export async function fetchCommittee(ticker = "NVDA", refresh = false) {
  const q = refresh ? "?refresh=1" : "";
  const res = await fetch(`${BASE}/api/committee/${ticker}${q}`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// Persisted daily read snapshots (one per ticker per calendar day, 1D only). A plain directory read of
// what was already stored — NO model call — so the Research History section can show how the computed
// regime and the analytical read moved without re-running anything.
export async function fetchReads(ticker = "NVDA", limit = 10) {
  const res = await fetch(`${BASE}/api/reads/${ticker}?limit=${limit}`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// Persisted committee snapshots (one per day) — the track record the prediction service may
// eventually consume as features once enough history exists.
export async function fetchCommitteeHistory(ticker = "NVDA") {
  const res = await fetch(`${BASE}/api/committee/${ticker}/history`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// AI assistant situational note for a (ticker, timeframe): { structured:{situation, whatToWatch,
// wouldChangeIt, rangeComment}, modelUsed, warnings, disclaimer }. Grounded in the live context incl.
// the COMPUTED expected-close range; descriptive, non-oracular — NOT financial advice.
export async function fetchAssistant(ticker = "NVDA", timeframe = "1D") {
  const res = await fetch(`${BASE}/api/assistant/${ticker}?timeframe=${encodeURIComponent(timeframe)}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: "{}",
  });
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// Grounded chat: send the running message history (+ optional persona), get { reply, modelUsed,
// personaId, personaName, disclaimer }. Not cached.
export async function sendChat(ticker = "NVDA", timeframe = "1D", messages = [], personaId = null) {
  // Credentialed so the gateway can resolve the signed-in user's custom persona (guests: built-ins).
  const res = await request(`${BASE}/api/chat/${ticker}?timeframe=${encodeURIComponent(timeframe)}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ messages, personaId }),
  });
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// ---- Chat personas ("instructors" / Gems) --------------------------------------------------------
// User-editable custom-instruction profiles that shape the Copilot's role/tone/teaching style. They
// LAYER ON TOP of the chat's grounding rules — never a way to unlock a prediction or a buy/sell call.
// Built-in presets (id starts with "builtin:") are read-only; user personas support full CRUD.

async function personaJSON(path, opts) {
  const res = await request(`${BASE}/api/personas${path}`, opts);
  if (!res.ok) {
    let msg = `gateway ${res.status}`;
    try { const b = await res.json(); if (b?.error) msg = b.error; } catch { /* ignore */ }
    throw new Error(msg);
  }
  return res.json();
}

export async function listPersonas() {
  const data = await personaJSON("", undefined);
  return data.personas || [];
}

export async function createPersona({ name, instructions, emoji = "" }) {
  const data = await personaJSON("", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, instructions, emoji }),
  });
  return data.persona;
}

export async function updatePersona(id, patch) {
  const data = await personaJSON(`/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  return data.persona;
}

export async function deletePersona(id) {
  return personaJSON(`/${id}`, { method: "DELETE" });
}

// ---- Alerts service (separate Go microservice on :8095; additive, fails silent if down) ----

const ALERTS_BASE = import.meta.env.VITE_ALERTS_URL || "http://localhost:8095";

// Rule types surfaced in the AlertsPanel form, with the params each one needs. Kept in sync with
// alerts/rules.go. Descriptive conditions only — never buy/sell actions.
export const ALERT_RULE_TYPES = [
  { type: "price_cross", label: "Price crosses level", fields: ["level", "direction"] },
  { type: "pct_move", label: "Big % move (day)", fields: ["pct"] },
  { type: "rsi_threshold", label: "RSI threshold", fields: ["level", "direction"] },
  { type: "macd_cross", label: "MACD histogram flip", fields: ["macdDirection"] },
  { type: "trend_flip", label: "Trend flips", fields: [] },
  { type: "confluence_conflict", label: "Confluence conflict", fields: ["mode"] },
  { type: "vwap_cross", label: "VWAP cross (intraday)", fields: ["direction"] },
  { type: "new_8k", label: "New SEC 8-K filing", fields: [] },
  { type: "calendar_event", label: "Calendar event (earnings / macro)", fields: ["kind", "lead"] },
];

async function alertsJSON(path, opts) {
  const res = await request(`${ALERTS_BASE}${path}`, opts);
  if (!res.ok) {
    let msg = `alerts ${res.status}`;
    try {
      const body = await res.json();
      if (body?.error) msg = body.error;
    } catch { /* ignore */ }
    throw new Error(msg);
  }
  return res.json();
}

export async function listRules() {
  const data = await alertsJSON("/rules");
  return data.rules || [];
}

export async function createRule(rule) {
  const data = await alertsJSON("/rules", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(rule),
  });
  return data.rule;
}

export async function updateRule(id, patch) {
  const data = await alertsJSON(`/rules/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  return data.rule;
}

export async function deleteRule(id) {
  return alertsJSON(`/rules/${id}`, { method: "DELETE" });
}

// Returns { events, unread }.
export async function listEvents(limit = 50) {
  return alertsJSON(`/events?limit=${limit}`);
}

export async function markEventsRead() {
  return alertsJSON("/events/read", { method: "POST" });
}

// Mark a SINGLE notification read (bell modal click-to-read). Returns { ok, unread } — the caller's
// remaining unread count so the badge can update at once. Requires sign-in (guests have no events).
export async function markEventRead(id) {
  return alertsJSON(`/events/${encodeURIComponent(id)}/read`, { method: "POST" });
}

// ---- Journal service (separate Go microservice on :8096; additive, fails silent if down) ----
// A manual RECORD of your own trades — it executes nothing and gives no advice.

const JOURNAL_BASE = import.meta.env.VITE_JOURNAL_URL || "http://localhost:8096";

async function journalJSON(path, opts) {
  const res = await request(`${JOURNAL_BASE}${path}`, opts);
  if (!res.ok) {
    let msg = `journal ${res.status}`;
    try {
      const body = await res.json();
      if (body?.error) msg = body.error;
    } catch { /* ignore */ }
    throw new Error(msg);
  }
  return res.json();
}

export async function listTrades(status = "all", mode = "all") {
  const data = await journalJSON(
    `/trades?status=${encodeURIComponent(status)}&mode=${encodeURIComponent(mode)}`
  );
  return data.trades || [];
}

export async function createTrade(trade) {
  const data = await journalJSON("/trades", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(trade),
  });
  return data.trade;
}

export async function updateTrade(id, patch) {
  const data = await journalJSON(`/trades/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  return data.trade;
}

// closeTrade is a convenience wrapper: closing a trade means recording its exit.
export async function closeTrade(id, exit) {
  return updateTrade(id, { exit });
}

export async function deleteTrade(id) {
  return journalJSON(`/trades/${id}`, { method: "DELETE" });
}

// Stats can be scoped by mode so simulated PAPER P&L never contaminates real/live stats.
export async function fetchJournalStats(mode = "all") {
  const q = mode && mode !== "all" ? `?mode=${encodeURIComponent(mode)}` : "";
  return journalJSON(`/stats${q}`);
}

// ---- Paper-trading validation service (:8097; additive, fails silent if down) ----
// SIMULATION ONLY — no execution, no broker, no money movement.

const PAPER_BASE = import.meta.env.VITE_PAPER_URL || "http://localhost:8097";

async function paperJSON(path, opts) {
  const res = await fetch(`${PAPER_BASE}${path}`, { credentials: "include", ...opts });
  if (!res.ok) {
    let msg = `paper ${res.status}`;
    let body = null;
    try { body = await res.json(); if (body?.error) msg = body.error; } catch { /* ignore */ }
    const err = new Error(msg);
    err.status = res.status;
    err.data = body;
    throw err;
  }
  return res.json();
}

// NOTE: `GET /paper/positions` (the JOURNAL's view of open paper trades, marked to /quote) has no
// client wrapper any more. The panel renders the LEDGER's lots instead — the book is what the
// experiment is scored on, and two differently-marked position tables side by side was one of the
// things that made the old panel unreadable. The endpoint is still served; add a wrapper back in one
// line if a surface ever needs the journal's marks specifically.

export async function paperComparison() {
  return paperJSON("/paper/comparison");
}

// The source-backed monitoring payload for Journal → Experiments. It is generation-scoped and
// combines the engine, ledger, comparison and settled-decision records under one `asOf` timestamp.
export async function paperDashboard(days = 252) {
  return paperJSON(`/paper/dashboard?days=${encodeURIComponent(days)}`);
}

// The ENGINE's own account of what it did and why: per config, the four gate verdicts with the
// refusing reason, the last decision (bar / target / action), the position, and the three-store
// `sync` block. The engine trades NOTHING today because no evaluator EDGE verdict exists — that is
// the design working, and this endpoint is where it says so.
export async function paperStatus() {
  return paperJSON("/paper/status");
}

// The launch checklist is evaluated on demand from current upstream evidence. It invokes the same
// four Go gates the engine spends; it never computes a second opinion in the browser.
export async function paperReadiness() {
  return paperJSON("/paper/readiness");
}

export async function setPaperConfig(configs) {
  return paperJSON("/paper/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ configs }),
  });
}

// The fake-money book (docs/PAPER_EXECUTION_CONTRACT.md §5): equity, cash, realized/unrealized,
// exposure, the daily snapshot series and its statistics, and the dates it could NOT mark.
// `fills=0` because the panel renders the book, not the attestel; ask for a tail when that changes.
export async function paperLedger(fills = 0) {
  return paperJSON(`/paper/ledger?fills=${encodeURIComponent(fills)}`);
}

export async function paperReset() {
  return paperJSON("/paper/reset?confirm=true", { method: "POST" });
}

// ---- Research features (qwen reads text; all on-demand via the gateway) ----

// Earnings-call transcript analysis: paste text (or give a URL the gateway will fetch). Returns
// key points, QUOTE-VERIFIED evasive moments, management tone, and a tone shift vs the stored
// prior quarter. Long transcripts take a while (sequential chunked model calls).
export async function submitTranscript(ticker, { label = "", text = "", url = "" } = {}) {
  const res = await fetch(`${BASE}/api/transcript/${ticker}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ label, text, url }),
  });
  if (!res.ok) {
    let msg = `gateway ${res.status}`;
    try { const b = await res.json(); if (b?.error) msg = b.error; } catch { /* ignore */ }
    throw new Error(msg);
  }
  return res.json();
}

export async function fetchTranscriptHistory(ticker) {
  const res = await fetch(`${BASE}/api/transcript/${ticker}`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// News-cascade digest: clusters + impact scores + 3-sentence day summary over the collected feed.
export async function fetchDigest(ticker, refresh = false) {
  const res = await fetch(`${BASE}/api/digest/${ticker}${refresh ? "?refresh=1" : ""}`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// Scenario mode: structured hypothetical reasoning grounded in the supply-chain/roadmap/risks
// facts. A thought experiment — NOT a prediction or advice.
export async function runScenario(ticker, question) {
  const res = await fetch(`${BASE}/api/scenario/${ticker}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ question }),
  });
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// One bounded investigation, not a chat turn. The gateway streams NDJSON progress only when real
// stages begin (source collection, then synthesis), followed by one structured report. `onEvent`
// lets the Research canvas show that work without inventing timer-based progress.
export async function runInvestigation(
  ticker,
  { question, addOns = [], onEvent = () => {}, signal } = {}
) {
  const res = await request(`${BASE}/api/investigate/${ticker}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/x-ndjson" },
    body: JSON.stringify({ question, addOns }),
    signal,
  });
  if (!res.ok) {
    let message = `gateway ${res.status}`;
    try {
      const body = await res.json();
      if (body?.error) message = body.error;
    } catch {
      /* ignore */
    }
    throw new Error(message);
  }

  if (!res.body?.getReader) {
    const data = await res.json();
    onEvent({ type: "result", result: data });
    return data;
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let pending = "";
  let result = null;

  const consume = (line) => {
    if (!line.trim()) return;
    const event = JSON.parse(line);
    onEvent(event);
    if (event.type === "result") result = event.result;
    if (event.type === "error") throw new Error(event.error || "investigation failed");
  };

  while (true) {
    const { value, done } = await reader.read();
    pending += decoder.decode(value || new Uint8Array(), { stream: !done });
    const lines = pending.split("\n");
    pending = lines.pop() || "";
    lines.forEach(consume);
    if (done) break;
  }
  if (pending.trim()) consume(pending);
  if (!result) throw new Error("research run ended before a report was returned");
  return result;
}

// Re-run the evidence check for one thesis on demand (checks also run automatically with the Brief).
export async function checkThesis(id) {
  // Credentialed: the gateway scopes the thesis lookup + result persistence to the signed-in user.
  const res = await request(`${BASE}/api/theses/${id}/check`, { method: "POST" });
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// Theses CRUD lives in the journal service (user records).
export async function listTheses(ticker = "", status = "") {
  const q = new URLSearchParams();
  if (ticker) q.set("ticker", ticker);
  if (status) q.set("status", status);
  const qs = q.toString();
  const data = await journalJSON(`/theses${qs ? `?${qs}` : ""}`);
  return data.theses || [];
}

export async function createThesis(ticker, text) {
  const data = await journalJSON("/theses", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ ticker, text }),
  });
  return data.thesis;
}

export async function updateThesis(id, patch) {
  const data = await journalJSON(`/theses/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  return data.thesis;
}

export async function deleteThesis(id) {
  return journalJSON(`/theses/${id}`, { method: "DELETE" });
}

// ---- Feedback service (separate Go microservice on :8098; additive, fails silent if down) ----
// Tester submissions (rating + category + message + auto context snapshot) and admin triage.
//
// Every route except /health requires a signed-in session (D-16), so these calls send credentials.
// Two failures are NOT "the service is down" and must not be rendered as such:
//   401 -> AuthRequiredError    the caller is a guest; route them to sign-in
//   403 -> AdminRequiredError   signed in, but not on the FEEDBACK_ADMIN_UIDS allow-list
// Anything else stays a plain Error so the panel can keep failing soft when :8098 is genuinely gone.

const FEEDBACK_BASE = import.meta.env.VITE_FEEDBACK_URL || "http://localhost:8098";

// AdminRequiredError is thrown when a signed-in user asks for the owner-triage surface.
export class AdminRequiredError extends Error {
  constructor(message = "admin access required") {
    super(message);
    this.name = "AdminRequiredError";
  }
}

async function feedbackJSON(path, opts) {
  const res = await fetch(`${FEEDBACK_BASE}${path}`, { credentials: "include", ...opts });
  if (!res.ok) {
    let msg = `feedback ${res.status}`;
    try { const b = await res.json(); if (b?.error) msg = b.error; } catch { /* ignore */ }
    if (res.status === 401) throw new AuthRequiredError(msg);
    if (res.status === 403) throw new AdminRequiredError(msg);
    throw new Error(msg);
  }
  return res.json();
}

export async function submitFeedback({ category, rating, message, name, context }) {
  const data = await feedbackJSON("/feedback", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ category, rating, message, name, context }),
  });
  return data.feedback;
}

// Returns { feedback: [...newest first], unread, admin }. The server decides `admin` from its own
// allow-list and scopes the list accordingly — a normal tester gets only their own submissions.
export async function listFeedback(status = "", category = "") {
  const q = new URLSearchParams();
  if (status) q.set("status", status);
  if (category) q.set("category", category);
  const qs = q.toString();
  return feedbackJSON(`/feedback${qs ? `?${qs}` : ""}`);
}

export async function updateFeedback(id, status) {
  const data = await feedbackJSON(`/feedback/${id}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ status }),
  });
  return data.feedback;
}

export async function deleteFeedback(id) {
  return feedbackJSON(`/feedback/${id}`, { method: "DELETE" });
}

export async function feedbackSummary() {
  return feedbackJSON("/feedback/summary");
}

// Directional signal (prediction service, via the gateway). Returns either a backtest-gated signal
// or { signal: null, reason }. Never a bare guess. The app suggests; the user executes.
export async function fetchPrediction(ticker = "NVDA", timeframe = "1D", horizon = 5, bust = false) {
  // `bust` sidesteps the gateway's ~2 min predict cache right after a retrain, so the fresh model
  // shows immediately instead of the stale cached signal.
  const b = bust ? `&_=${Date.now()}` : "";
  const res = await fetch(
    `${BASE}/api/predict/${ticker}?timeframe=${encodeURIComponent(timeframe)}&horizon=${horizon}${b}`
  );
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// Train one immutable challenger candidate. Serving changes only through the separate promotion API.
export async function trainModel(ticker = "NVDA", { timeframe = "1D", horizon = 5, allowShort = false } = {}) {
  const res = await request(
    `${BASE}/api/train/${ticker}?timeframe=${encodeURIComponent(timeframe)}&horizon=${horizon}&allowShort=${allowShort}`,
    { method: "POST" }
  );
  const body = await res.json().catch(() => null);
  if (res.status === 403) throw new AdminRequiredError(body?.error || "evaluation admin access required");
  if (!res.ok) throw new Error(body?.detail || body?.error || `gateway ${res.status}`);
  return body;
}

export async function fetchModelRegistry(ticker = "NVDA", { timeframe, horizon } = {}) {
  const qs = new URLSearchParams();
  if (timeframe) qs.set("timeframe", timeframe);
  if (horizon != null) qs.set("horizon", String(horizon));
  const res = await request(`${BASE}/api/models/${ticker}${qs.size ? `?${qs}` : ""}`);
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

async function deployModel(action, ticker, modelVersion, { timeframe = "1D", horizon = 5, reason = "" } = {}) {
  const qs = new URLSearchParams({ timeframe, horizon: String(horizon) });
  const res = await request(
    `${BASE}/api/models/${ticker}/${encodeURIComponent(modelVersion)}/${action}?${qs}`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ reason }),
    }
  );
  const body = await res.json().catch(() => null);
  if (res.status === 403) throw new AdminRequiredError(body?.error || body?.detail || "evaluation admin access required");
  if (res.status === 409) {
    const detail = typeof body?.detail === "string" ? body.detail : body?.detail?.error;
    const err = new Error(detail || `${action} was refused`);
    err.gates = body?.detail?.promotionGates || [];
    throw err;
  }
  if (!res.ok) throw new Error(body?.detail || body?.error || `gateway ${res.status}`);
  return body;
}

export function promoteModel(ticker, modelVersion, options) {
  return deployModel("promote", ticker, modelVersion, options);
}

export function rollbackModel(ticker, modelVersion, options) {
  return deployModel("rollback", ticker, modelVersion, options);
}

// ---- Edge evaluation (the offline harness, triggered from the app) ---------------------------
//
// Production is the single-container deploy: the operator has no shell, so `python -m app.evaluate`
// could not be run there at all. These two calls are the whole in-app path, and they are the ONLY
// thing this lane adds — the verdict logic, the four paper gates and the strategy-version machinery
// are untouched. Running the harness is not the same as influencing what it concludes.
//
// Three responses are NOT "the service is down" and must not be rendered as such:
//   401 -> AuthRequiredError    (writes) / { signedOut: true } (the status read)
//   403 -> AdminRequiredError   signed in, but not on this deployment's EVAL_ADMIN_UIDS allow-list
//   409 -> RunInFlightError     a run is already going; it carries that run's status

// RunInFlightError is thrown when a second run is requested while one is still going. `status` is
// the running job's own status payload.
export class RunInFlightError extends Error {
  constructor(message = "an evaluation run is already in flight", status = null) {
    super(message);
    this.name = "RunInFlightError";
    this.status = status;
  }
}

// runEvaluation starts ONE evaluation. It takes no arguments, deliberately and permanently: the run
// uses only the deployment's own EVAL_* environment, so a verdict minted from the app is always
// stamped with the deployment's strategy version and can never be a custom-parameter run
// masquerading as the default strategy (docs/PAPER_EXECUTION_CONTRACT.md §4.3). The server refuses
// parameters with 400 rather than ignoring them; do not add any here.
export async function runEvaluation() {
  const res = await request(`${BASE}/api/evaluate/run`, { method: "POST" });
  let body = null;
  try {
    body = await res.json();
  } catch {
    /* a body-less error still has a status */
  }
  if (res.status === 403) {
    throw new AdminRequiredError(body?.error || "evaluation admin access required");
  }
  if (res.status === 409) {
    throw new RunInFlightError(body?.error, body?.status || null);
  }
  if (!res.ok) throw new Error(body?.detail || body?.error || `gateway ${res.status}`);
  return body;
}

// fetchEvaluationStatus reads run state + the stored verdicts. Signed-in only; a guest gets 401 and
// the panel says so rather than rendering a blank board. It starts nothing and decides nothing.
export async function fetchEvaluationStatus() {
  const res = await request(`${BASE}/api/evaluate/status`);
  if (res.status === 401) return { signedOut: true, state: "idle", verdicts: [] };
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// Separate PEAD research runner. It shares the gateway's admin boundary but never writes the price
// evaluator verdict records that paper gate 4 consumes.
export async function runEventEvaluation() {
  const res = await request(`${BASE}/api/evaluate-events/run`, { method: "POST" });
  let body = null;
  try {
    body = await res.json();
  } catch {
    /* a body-less error still has a status */
  }
  if (res.status === 403) {
    throw new AdminRequiredError(body?.error || "evaluation admin access required");
  }
  if (res.status === 409) {
    throw new RunInFlightError(body?.error, body?.status || null);
  }
  if (!res.ok) throw new Error(body?.detail || body?.error || `gateway ${res.status}`);
  return body;
}

export async function fetchEventEvaluationStatus() {
  const res = await request(`${BASE}/api/evaluate-events/status`);
  if (res.status === 401) return { signedOut: true, state: "idle", verdicts: [], kind: "event" };
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// Bounded forward-estimate collector for the shell-less production deployment. The route uses the
// same fail-closed admin boundary as the evaluators and accepts no parameters.
export async function runEstimateSnapshots() {
  const res = await request(`${BASE}/api/estimate-snapshots/run`, { method: "POST" });
  let body = null;
  try {
    body = await res.json();
  } catch {
    /* a body-less error still has a status */
  }
  if (res.status === 403) {
    throw new AdminRequiredError(body?.error || "evaluation admin access required");
  }
  if (res.status === 409) {
    throw new RunInFlightError(body?.error, body?.status || null);
  }
  if (!res.ok) throw new Error(body?.detail || body?.error || `gateway ${res.status}`);
  return body;
}

export async function fetchEstimateSnapshotsStatus() {
  const res = await request(`${BASE}/api/estimate-snapshots/status`);
  if (res.status === 401) return { signedOut: true, state: "idle", kind: "estimate" };
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

export function fmtB(n) {
  if (n == null) return "—";
  return `$${Number(n).toFixed(1)}B`;
}

// fmtMoney renders a signed dollar amount (used for P&L).
export function fmtMoney(n) {
  if (n == null) return "—";
  const v = Number(n);
  return `${v >= 0 ? "+" : "-"}$${Math.abs(v).toFixed(2)}`;
}

export function fmtPct(n) {
  if (n == null) return "—";
  const v = Number(n);
  return `${v >= 0 ? "+" : ""}${v.toFixed(1)}%`;
}

// ---- Service health (UI-only; drives the top-bar HealthDot cluster) --------------------------
// Browser-reachable /health endpoints. analysis/llm/prediction sit behind the gateway (no CORS),
// so their liveness is derived from the dashboard payload in the app, not polled here.

export const HEALTH_TARGETS = [
  { key: "gateway", label: "Gateway", url: `${BASE}/health` },
  { key: "auth", label: "Auth", url: `${AUTH_BASE}/health` },
  { key: "alerts", label: "Alerts", url: `${ALERTS_BASE}/health` },
  { key: "journal", label: "Journal", url: `${JOURNAL_BASE}/health` },
  { key: "paper", label: "Paper", url: `${PAPER_BASE}/health` },
  { key: "feedback", label: "Feedback", url: `${FEEDBACK_BASE}/health` },
];

// pingHealth resolves true/false — never throws (a down service must not break the cluster).
export async function pingHealth(url) {
  try {
    const res = await fetch(url, { cache: "no-store" });
    return res.ok;
  } catch {
    return false;
  }
}
