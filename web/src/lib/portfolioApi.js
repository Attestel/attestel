import { AuthRequiredError } from "./api.js";

const JOURNAL_BASE = import.meta.env.VITE_JOURNAL_URL || "http://localhost:8096";

export class PortfolioApiError extends Error {
  constructor(message, status = 0) {
    super(message);
    this.name = "PortfolioApiError";
    this.status = status;
  }
}

async function portfolioRequest(path, options = {}) {
  let response;
  try {
    response = await fetch(`${JOURNAL_BASE}${path}`, { credentials: "include", ...options });
  } catch {
    throw new PortfolioApiError("the portfolio service is unreachable");
  }
  let body = null;
  try {
    body = await response.json();
  } catch {
    // Status remains authoritative when an upstream returns a non-JSON error.
  }
  if (response.status === 401) {
    throw new AuthRequiredError(body?.error || "sign in required");
  }
  if (!response.ok) {
    throw new PortfolioApiError(body?.error || `portfolio request failed (${response.status})`, response.status);
  }
  return body;
}

const jsonBody = (body) => ({
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify(body),
});

export async function listPortfolios() {
  return (await portfolioRequest("/portfolios"))?.portfolios || [];
}

export async function createPortfolio(portfolio) {
  return (await portfolioRequest("/portfolios", { method: "POST", ...jsonBody(portfolio) }))?.portfolio;
}

export async function updatePortfolio(id, patch) {
  return (
    await portfolioRequest(`/portfolios/${encodeURIComponent(id)}`, {
      method: "PATCH",
      ...jsonBody(patch),
    })
  )?.portfolio;
}

export async function deletePortfolio(id) {
  return portfolioRequest(`/portfolios/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export async function getPortfolioIntelligence(id) {
  return (
    await portfolioRequest(`/portfolios/${encodeURIComponent(id)}/intelligence`)
  )?.intelligence;
}

export async function listPortfolioSnapshots(id, limit = 20) {
  return (
    await portfolioRequest(
      `/portfolios/${encodeURIComponent(id)}/snapshots?limit=${encodeURIComponent(limit)}`
    )
  )?.snapshots || [];
}

export async function createPortfolioSnapshot(id) {
  return portfolioRequest(`/portfolios/${encodeURIComponent(id)}/snapshots`, { method: "POST" });
}

export async function listPortfolioReviews(id, limit = 20) {
  return (
    await portfolioRequest(
      `/portfolios/${encodeURIComponent(id)}/reviews?limit=${encodeURIComponent(limit)}`
    )
  )?.reviews || [];
}

export async function createPortfolioReview(id) {
  return portfolioRequest(`/portfolios/${encodeURIComponent(id)}/review`, { method: "POST" });
}

export async function runPortfolioScenario(id, question) {
  return (
    await portfolioRequest(`/portfolios/${encodeURIComponent(id)}/scenario`, {
      method: "POST",
      ...jsonBody({ question }),
    })
  )?.scenario;
}

// ── Phase 3: event impact ────────────────────────────────────────────────────────────────────────
//
// Both of these are READS over records the server computed. Neither starts a job, and neither can
// reach a model: the journal answers them from its own per-user stores plus the events service's
// store-only relationship view. Opening Following or Portfolio therefore causes no provider call
// and no generation, which is the property Phase 3's acceptance list turns on.

// getPortfolioEventImpact — which scheduled events bear on this portfolio's holdings, with the
// exposed weight computed server-side in code. `days` bounds the forward window (server-capped).
export async function getPortfolioEventImpact(id, days = 14) {
  return portfolioRequest(
    `/portfolios/${encodeURIComponent(id)}/event-impact?days=${encodeURIComponent(days)}`
  );
}

// listThesisEventReviews — the deterministic review queue: which upcoming events touch a condition
// the user themselves wrote on an active thesis.
//
// `bearing` is never `supports` or `contradicts` here. Deterministic matching establishes that an
// event is ABOUT the same subject as something the user named; it establishes nothing about
// direction, and the server refuses to claim one.
export async function listThesisEventReviews(days = 14) {
  return portfolioRequest(`/theses/event-reviews?days=${encodeURIComponent(days)}`);
}
