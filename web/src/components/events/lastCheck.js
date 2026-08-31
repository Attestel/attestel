// lastCheck — "when did you last look at what changed".
//
// WAVE 5B MOVED THIS TO THE SERVER, and this file is now the client half of a two-tier boundary
// rather than the whole thing.
//
//   signed in   → journal's per-user partition, via `GET/POST /api/event-state/last-check`
//                 (contract §2.3). Survives a new device, a cleared profile and a second browser.
//   signed out  → `localStorage`, exactly as before. A guest has no partition, and a guest boundary
//                 is precisely what local storage is for.
//
// Wave 4 Lane 4A shipped the local half and said in the same breath that it was a limitation rather
// than a design: "It does not survive a new device, a cleared profile or a second browser." 4A's
// handoff, GAPS.md and the Wave 4 integration report all named journal's `user_event_state` as the
// server-side home and recorded the move as a CONTRACT AMENDMENT. 5B lands `/api/changed`, which is
// the surface that reads the boundary, so this is the wave that owes the move.
//
// THE COPY RULE THIS FILE EXISTS TO ENFORCE — unchanged, and it is a rule, not a preference: a
// default 24-hour window may NEVER be labelled "since your last check". No stored timestamp ⇒ the
// caller omits `since`, the server answers `since.basis === "default24h"`, and the heading reads
// "IN THE LAST 24 HOURS". `hasLastCheck` and the resolved `basis` are how a caller knows which
// sentence it is allowed to write.
//
// THE LOCAL COPY IS STILL WRITTEN FOR A SIGNED-IN USER, deliberately. It is the offline floor: if
// journal is unreachable the surface still says something true about this browser rather than
// silently regressing to the 24-hour window. The SERVER value wins whenever both exist — the local
// one can only be older or equal, since every stamp goes to both.

const KEY = "attestel.lastCheck";
const BASE = import.meta.env.VITE_GATEWAY_URL || "";

// Keyed by uid so two accounts on one browser do not inherit each other's boundary. Signed-out is
// the literal "guest" — a real, separate reading position, not a missing one.
const slot = (uid) => `${KEY}.${uid || "guest"}`;

// readLastCheck returns unix SECONDS from LOCAL storage, or 0 when there is none. 0 is falsy on
// purpose: every caller then omits `since` and falls back to the 24-hour heading without a second
// branch. It stays synchronous because a surface has to render its first frame before any fetch
// resolves, and the local value is the honest thing to render meanwhile.
export function readLastCheck(uid) {
  try {
    const raw = window.localStorage.getItem(slot(uid));
    const n = Number(raw);
    return Number.isFinite(n) && n > 0 ? n : 0;
  } catch {
    // Private mode, disabled storage, quota. A missing boundary is a fully supported state, so
    // this degrades to "the last 24 hours" rather than surfacing an error at the user.
    return 0;
  }
}

// writeLastCheck is called ONLY when the surface was actually viewed — not on mount of a parent,
// not on a route change that never painted it. Writing it earlier would silently advance the
// boundary past events the user never saw, which is the one failure this whole object exists to
// prevent.
export function writeLastCheck(uid, at = Math.floor(Date.now() / 1000)) {
  try {
    window.localStorage.setItem(slot(uid), String(at));
  } catch {
    /* storage unavailable — the surface stays on the 24-hour window, which is honest */
  }
}

export const hasLastCheck = (uid) => readLastCheck(uid) > 0;

// resolveLastCheck is the SERVER-FIRST read. It returns `{ at, source }` where `source` is
// "server" | "local" | "none", so a caller can tell a boundary that will follow the user to their
// next device from one that will not.
//
// A guest never calls the server: there is no partition to read and the route is a 401 by design.
// A signed-in user whose journal is unreachable falls back to the local value — degraded, but
// truthful, and never to an invented "now".
export async function resolveLastCheck(uid) {
  const local = readLastCheck(uid);
  if (!uid) return { at: local, source: local ? "local" : "none" };

  try {
    const res = await fetch(`${BASE}/api/event-state/last-check`, { credentials: "include" });
    if (res.ok) {
      const body = await res.json();
      const at = Number(body?.lastCheck);
      // `null` is a REAL ANSWER — "this user has never recorded a boundary" — and it is not the
      // same as a failed read. Both fall through to the local value, but only the failed one is
      // a degradation, and neither may become an invented timestamp.
      if (Number.isFinite(at) && at > 0) {
        // Keep the local copy in step so the offline floor is never behind the server.
        if (at > local) writeLastCheck(uid, at);
        return { at, source: "server" };
      }
      return { at: local, source: local ? "local" : "none" };
    }
  } catch {
    /* offline or journal down — fall through to the local value */
  }
  return { at: local, source: local ? "local" : "none" };
}

// recordLastCheck stamps BOTH tiers. Local first and unconditionally, so the boundary survives even
// if the request fails; then the server, whose store is monotonic and will ignore a stamp older
// than one it already holds.
//
// Same rule as `writeLastCheck`: call it only once the surface has actually rendered an answer.
export async function recordLastCheck(uid, at = Math.floor(Date.now() / 1000)) {
  writeLastCheck(uid, at);
  if (!uid) return;
  try {
    await fetch(`${BASE}/api/event-state/last-check`, {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ at }),
    });
  } catch {
    /* the local stamp stands; the next successful call will carry it forward */
  }
}
