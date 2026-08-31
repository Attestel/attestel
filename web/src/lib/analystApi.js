// analystApi.js — the ANALYST-specific frontend adapter (Wave 4 Lane 4C).
//
// A separate module for the same reason monitoringApi.js, thesisApi.js, evidenceApi.js, lensApi.js
// and eventsApi.js are: `lib/api.js` is on PARALLEL_CONTRACTS.md's reserved shared-file list and
// three Wave 4 lanes build concurrently, so a lane that needs a new call creates a `*Api.js` beside
// it instead. This module owns the analyst surface and nothing else.
//
// ONE upstream, deliberately: the GATEWAY. `POST /api/analyst/{ticker}` and its status read
// `GET /api/analyst/runs/{runId}` (gateway/analyst.go, gateway/analystjobs.go). Never :8002, never
// :8004 (contract §9.3).
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// INVARIANT #4 IS THIS MODULE'S WHOLE DESIGN, SO IT IS STATED BEFORE THE CODE.
//
// There is exactly ONE function here that can reach the model, and it is called `startAnalystRun`.
// It is a POST, it is expensive (up to eight sequential local-model generations, each holding the
// cross-process model lease), and it is USER-INITIATED ONLY. No `useEffect`, no mount, no ticker
// change, no poll, no interval, no prefetch may call it. `gateway/analyst.go`'s own header says the
// same thing for the same reason, and it refuses non-POST explicitly so a browser prefetch cannot
// reach it either.
//
// `fetchAnalystRun` is the OTHER half and is a different animal: it reads the status of a run that
// has ALREADY been started, it reaches a map lookup in the gateway and no upstream at all, and it
// therefore MAY be polled and MAY be called from an effect. The gateway enforces this split —
// the run route refuses a GET, the status route starts nothing.
//
// WHY ANY OF THIS IS ASYNCHRONOUS. It used to be one POST that returned the finished envelope.
// A run outlives an HTTP request: nginx closes the connection at `proxy_read_timeout 120s`, so in
// production the POST returned 504, the card silently fell back to "Not yet assessed", and the run
// kept going with nobody watching. Now the POST returns a `runId` at once and the client polls the
// status read, so a slow model costs the proxy nothing, a failure is a REPORTED result rather than
// a vanished request, and a reload or a navigation resumes the same run (see `recallAnalystRun`).
//
// There is still deliberately NO `fetchStoredAnalyst`. The finished envelope lives in the gateway's
// in-memory `ttlCache` with no persistence and no read route of its own, so there is no way to ask
// "is there already an answer?" without either a POST or a run id you already hold. That is why
// "not yet assessed" is the ordinary opening state of the Attestel view rather than an error or an
// empty state.
//
// What this module cannot do, by construction:
//   * it cannot compute a level — `support`, `resistance` and `invalidation` are server-computed
//     and re-imposed after the model replies (invariant #3). Nothing here rounds or re-derives one;
//   * it cannot turn `confidenceBucket` into a number — it is an ORDINAL, not a probability
//     (contract §7, AD-8). There is no scale, no percentage and no gauge in this file;
//   * it cannot open the direction gate — see `directionGate` below;
//   * it cannot leak an audit-only field to a renderer — see `renderableThesis` below.
// ─────────────────────────────────────────────────────────────────────────────────────────────

import { AuthRequiredError } from "./api.js";

export { AuthRequiredError };

const BASE = import.meta.env.VITE_GATEWAY_URL || "";

// AnalystError carries the server's machine code so a caller can explain a refusal instead of
// showing "request failed". Byte-for-byte the MonitoringError / EventsError contract.
export class AnalystError extends Error {
  constructor(message, { code = "", status = 0 } = {}) {
    super(message);
    this.name = "AnalystError";
    this.code = code;
    this.status = status;
  }
}

// request is the shared fetch wrapper: session cookie always sent, 401 raised as AuthRequiredError
// so the shared guest-routing UI can react to it, everything else as AnalystError.
async function request(url, opts = {}) {
  const res = await fetch(url, { credentials: "include", ...opts });
  if (res.status === 401) throw new AuthRequiredError("sign in required");
  let body = null;
  try {
    body = await res.json();
  } catch {
    body = null;
  }
  if (!res.ok) {
    throw new AnalystError(body?.error || `request failed (${res.status})`, {
      code: body?.code || "",
      status: res.status,
    });
  }
  return body ?? {};
}

// HORIZONS is §9.11's REQUEST-token vocabulary, exactly as `gateway/analyst.go::analystHorizons`
// validates it. The `1d -> 1_trading_day` expansion lives in ONE place — `analyst.py::HORIZON_FIELD`
// — and restating it here would be the third vocabulary §9.11 forbids. So this list is request
// tokens only, and the human label comes off the SERVED `horizon` field (see `horizonLabel`).
export const HORIZONS = ["1d", "5d", "20d", "60d"];

// RUN_RUNNING / RUN_DONE / RUN_FAILED are the gateway's three run states, spelled once. A failed
// run is a RESULT and is rendered as one — the card must never answer a failure by returning to the
// state it was in before the user clicked.
export const RUN_RUNNING = "running";
export const RUN_DONE = "done";
export const RUN_FAILED = "failed";

// startAnalystRun triggers ONE analyst run. THE ONLY CALL IN THIS LANE THAT CAN REACH THE MODEL.
//
// Wire it to a click and nothing else. See the invariant #4 block at the top of this file: this is
// not a cheap read, and the gateway route it hits exists in POST form precisely so that no
// speculative navigation, link prefetch or over-eager effect can reach it by accident.
//
// It resolves as soon as the run is REGISTERED, not when it finishes. Two shapes come back and the
// caller tells them apart on `status`:
//   * `{ status: "running", runId, pollAfterMs }` — 202, poll `fetchAnalystRun`;
//   * `{ status: "done", … }` — 200, the gateway already had this exact run cached.
export function startAnalystRun(ticker, { horizon = "" } = {}) {
  const body = {};
  if (horizon) body.horizon = horizon;
  return request(`${BASE}/api/analyst/${encodeURIComponent(ticker)}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

// fetchAnalystRun reads one run's status. CHEAP, MODEL-FREE, AND SAFE TO POLL — it cannot start,
// resume or extend a run. A run the gateway no longer knows about (reaped, or a restart) raises an
// AnalystError with `code === "unknown_run"`, which the UI reports as a lost run and offers to
// start again. It never re-POSTs on its own: that would make a page load capable of starting a run.
export function fetchAnalystRun(runId) {
  return request(`${BASE}/api/analyst/runs/${encodeURIComponent(runId)}`);
}

// pollAnalystRun follows a run to its terminal state and resolves with the served body — `done`
// with the envelope, or `failed` with the reason. `onTick` is called with every intermediate status
// so a caller can show elapsed time; `signal` aborts the loop when the component unmounts.
//
// The cadence comes from the SERVER (`pollAfterMs`), with a floor so a bad value cannot turn this
// into a hot loop against a route that is cheap but not free.
const POLL_FLOOR_MS = 2000;

export async function pollAnalystRun(runId, { onTick = null, signal = null } = {}) {
  let waitMs = POLL_FLOOR_MS;
  for (;;) {
    if (signal?.aborted) return null;
    await new Promise((resolve) => setTimeout(resolve, waitMs));
    if (signal?.aborted) return null;
    const body = await fetchAnalystRun(runId);
    if (body?.status !== RUN_RUNNING) return body;
    if (onTick) onTick(body);
    waitMs = Math.max(POLL_FLOOR_MS, Number(body?.pollAfterMs) || POLL_FLOOR_MS);
  }
}

// ─────────────────────────────────────────────────────────── surviving a reload or a navigation

// The run id is remembered PER TICKER in `sessionStorage`, so leaving the page and coming back
// rejoins the run in flight instead of abandoning it — the user paid for it and the model is still
// working. It is a run id and nothing else: no envelope, no model output, no user text is stored,
// and a tab close ends it.
const RUN_STORE_PREFIX = "attestel.analystRun.";

function runStoreKey(ticker) {
  return RUN_STORE_PREFIX + String(ticker || "").toUpperCase();
}

export function rememberAnalystRun(ticker, runId) {
  try {
    if (runId) sessionStorage.setItem(runStoreKey(ticker), runId);
  } catch {
    /* private mode, or storage disabled — resuming is a convenience, never a requirement */
  }
}

export function recallAnalystRun(ticker) {
  try {
    return sessionStorage.getItem(runStoreKey(ticker)) || "";
  } catch {
    return "";
  }
}

export function forgetAnalystRun(ticker) {
  try {
    sessionStorage.removeItem(runStoreKey(ticker));
  } catch {
    /* nothing to clean up */
  }
}

// ─────────────────────────────────────────────────────────── selectors over the served envelope

// The envelope `POST /api/analyst/{ticker}` returns (services/llm/app/analyst.py, plus the two keys
// gateway/analyst.go stamps):
//
//   { ticker, asOf, horizons: ["1d","5d","20d","60d"], theses: { "5d": <§7 object>, … },
//     forecast, specialists, consensus, evidence: { toolCalls, layersAvailable },
//     identity, promptVersions, modelsUsed, reasoningModes, disclaimer,
//     available, degraded }
//
// and one §7 object per horizon:
//
//   { ticker, asOf, horizon, direction, confidenceBucket, regime, evidence{5 layers},
//     support[], resistance[], thesis, invalidation, riskFlags[], whatChanged[], sources[],
//     directionReason, audit, disclaimer }
//
// plus SERVER-SIDE-ONLY keys this module refuses to pass on. See `renderableThesis`.

// EVIDENCE_LAYERS is the five-layer decomposition in the order doc §16.6's own table prints it,
// with the label each layer renders under. The keys are the SERVED keys (analyst.py's
// EVIDENCE_LAYERS); the labels are display strings and nothing keys off them.
export const EVIDENCE_LAYERS = [
  { key: "technical", label: "Technical" },
  { key: "companyNews", label: "News" },
  { key: "fundamentals", label: "Fundamentals" },
  { key: "macro", label: "Macro" },
  { key: "marketContext", label: "Market context" },
];

// DIRECTION_WORDS / CONFIDENCE_WORDS render an enum member as a word, never as a number.
//
// `neutral` and `unclear` are FIRST-CLASS outcomes (contract §7, locked decision 3): they get the
// same typographic weight as `bullish`, in `muted`. Not greyed out, not apologetic, not smaller. A
// pipeline that can say "I don't know" deserves a UI that can show it.
export const DIRECTION_WORDS = {
  bullish: "Bullish",
  bearish: "Bearish",
  neutral: "Neutral",
  unclear: "Unclear",
};

// AD-8 / doc §16.6: an ORDINAL rendered as a word. Never a percentage, never a 0–100 gauge, never
// a derived score. `low | medium | high` are the only three members contract §7 permits.
export const CONFIDENCE_WORDS = { low: "Low", medium: "Moderate", high: "High" };

// EVIDENCE_WORDS maps a per-layer stance to its display word. `unclear` is what analyst.py writes
// for a layer whose evidence the run could not reach, so it means "not reached", not "conflicting"
// — and the UI says so rather than inventing a neutral-looking word for an absence.
export const EVIDENCE_WORDS = {
  bullish: "Bullish",
  bearish: "Bearish",
  positive: "Positive",
  negative: "Negative",
  neutral: "Neutral",
  unclear: "Not reached",
};

// horizonLabel turns the SERVED horizon field ("5_trading_days") into prose ("5 trading days").
// It reads the served value and never reconstructs it from a request token, so §9.11's single
// vocabulary owner stays `analyst.py`.
export function horizonLabel(served) {
  return String(served || "").replace(/_/g, " ").trim();
}

// thesisFor picks one §7 object out of the envelope, preferring the requested horizon and falling
// back to the first horizon the run actually returned. Returns null when there is nothing to show.
export function thesisFor(envelope, horizonKey = "") {
  const theses = envelope?.theses;
  if (!theses || typeof theses !== "object") return null;
  if (horizonKey && theses[horizonKey]) return theses[horizonKey];
  const order = Array.isArray(envelope?.horizons) ? envelope.horizons : Object.keys(theses);
  for (const k of order) if (theses[k]) return theses[k];
  return null;
}

// ─────────────────────────────────────────────────────────────── the two rules that gate the view

// RENDERABLE_THESIS_KEYS is an ALLOW-LIST, and it is an allow-list on purpose.
//
// The served §7 object carries fields that are audit records and diagnostics, and the Wave 4
// addendum names two of them as "the two fields most dangerous to put on a screen": they hold the
// prose the server REPLACED, they are exempt from the served-number sweep precisely because
// sanitising them would destroy their purpose, and they are therefore the only prose in the payload
// guaranteed unchecked. A deny-list would need updating every time the server grows a field; an
// allow-list fails closed, which is the direction a rendering firewall must fail in.
//
// `scripts/check-web-bundle.sh` greps the BUILT bundle for those field names. It passes here
// because this file names none of them — a projection that listed what it excludes would put the
// forbidden literals into the bundle and fail the gate it exists to satisfy.
const RENDERABLE_THESIS_KEYS = [
  "ticker", "asOf", "horizon", "direction", "confidenceBucket", "regime", "evidence",
  "support", "resistance", "thesis", "invalidation", "riskFlags", "whatChanged", "sources",
  "audit", "disclaimer",
];

// renderableThesis projects a served §7 object down to exactly the keys a component may read.
//
// Every component in this lane consumes the OUTPUT of this function, never the raw object, so a
// renderer cannot reach an audit-only field even by typo: the key is not there to reach. This is
// the same posture `analyst.py` takes when it projects the model's reply onto THESIS_KEYS — a
// field that describes something the pipeline did not do is worse than a missing one.
export function renderableThesis(obj) {
  if (!obj || typeof obj !== "object") return null;
  const out = {};
  for (const k of RENDERABLE_THESIS_KEYS) if (k in obj) out[k] = obj[k];
  return out;
}

// directionGate — contract §9.20, and the single most consequential function in this lane.
//
// The `direction` WORD renders only when a stored ablation verdict exists for that rung and horizon
// under `data/eval/ablation`. Everything else in the §7 object — `evidence`, `confidenceBucket`,
// `riskFlags`, `invalidation`, `whatChanged` — is NEVER gated and renders unconditionally.
//
// MEASURED AT THE TREE THAT CARRIES THIS FILE: `data/eval/ablation` does not exist, no service
// reads it, and no field anywhere in the analyst envelope carries a verdict. The gate is therefore
// closed on every run, and it stays closed until Wave 5A runs the ablation ladder AND a served
// field carries the result. "Not yet validated" is not this lane's edge case — it is the only
// state the product can currently be in, which is exactly what the Wave 4 addendum means by "the
// abstention state is the primary state, not an empty state".
//
// It is written as a real lookup rather than `return false` for two reasons. It documents the shape
// Wave 5A has to serve, and it opens by itself the moment that field arrives — a hardcoded `false`
// would have to be found and edited by someone who did not write it.
//
// The verdict is read from the run that produced the thesis. The client cannot read a filesystem,
// so the SERVER must surface it. That is now CONTRACT §9.61 (allocated at Wave 4 integration from
// this lane's proposed text, per §9.57): the envelope carries
// `ablationVerdict: { validated, rung, horizon }` on the §7 object, the client MATCHES rung and
// horizon before it opens, `validated: false` is a real answer and is not the same as an absent
// verdict, and — binding 5, added at integration — **Wave 5A must SERVE it, not merely produce it**,
// or the gate stays closed for the same reason one wave later.
export function directionGate(envelope, thesis) {
  const verdict = thesis?.ablationVerdict ?? envelope?.ablation?.verdict ?? null;
  if (!verdict || typeof verdict !== "object") {
    return { validated: false, verdict: null };
  }
  // A verdict object that does not name the rung and horizon it validated is not a verdict for
  // THIS read, and §9.20 gates per rung and horizon. Fail closed.
  const sameHorizon = !thesis?.horizon || !verdict.horizon || verdict.horizon === thesis.horizon;
  if (!sameHorizon || !verdict.rung) return { validated: false, verdict: null };
  return { validated: verdict.validated === true, verdict };
}

// isAbstention — `neutral` and `unclear` are the two directions that assert no lean. It is used to
// choose which server-authored line the view is displaying, never to decide whether to render one:
// the server has already made that decision and written the text.
export function isAbstention(direction) {
  return direction === "neutral" || direction === "unclear";
}
