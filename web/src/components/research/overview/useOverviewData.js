import { useCallback, useEffect, useState } from "react";
import { fetchCalendar } from "../../../lib/api.js";
import { fetchFollowing, listSubscriptions, followTicker, unfollowTicker } from "../../../lib/eventsApi.js";

// useOverviewData — the Overview first viewport's reads.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// INVARIANT #4. EVERY CALL IN THIS FILE IS A DETERMINISTIC, NON-MODEL READ.
//
// Forbidden here and reachable from nowhere in this hook: `/api/chat`, `/api/assistant`,
// `/api/brief/*`, `/api/digest`, `/api/scenario`, `/api/investigate`, `/api/committee` with
// `refresh=true`, and `POST /api/analyst/{ticker}`. The analyst run lives in the component, behind
// a button, and this hook has no reference to it — deliberately, so that adding a model call here
// would require importing something this file does not import.
//
// What it does call, all cheap and all deterministic:
//   * `GET /api/subscriptions`  — the follow set (gateway proxy, §9.3; never :8096)
//   * `GET /api/following`      — this ticker's material events (contract §8)
//   * `GET /api/calendar`       — upcoming scheduled events
//   * `GET /api/next-earnings/{ticker}` — the next earnings date
// ─────────────────────────────────────────────────────────────────────────────────────────────
//
// Every read FAILS SOFT. A gateway that is down leaves each block with an honest "unavailable"
// state and the page still renders — the Attestel view, which is the point of the screen, does not
// depend on any of them.

const BASE = import.meta.env.VITE_GATEWAY_URL || "";

// nextEarnings is a plain read of the gateway's lightweight per-ticker lookup. It is not in
// `api.js` (which this lane may not edit) and is one URL, so it lives here rather than justifying
// a fourth adapter module.
async function fetchNextEarnings(ticker) {
  const res = await fetch(`${BASE}/api/next-earnings/${encodeURIComponent(ticker)}`, {
    credentials: "include",
  });
  if (!res.ok) throw new Error(`gateway ${res.status}`);
  return res.json();
}

// isoDay formats a Date as YYYY-MM-DD for the calendar window. Local date parts, not `toISOString`,
// so a user west of UTC does not silently ask for tomorrow's window.
function isoDay(d) {
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

export function useOverviewData(ticker) {
  const [subscriptions, setSubscriptions] = useState({ list: [], loaded: false, error: null });
  const [events, setEvents] = useState({ items: [], degraded: [], loaded: false, error: null });
  const [upcoming, setUpcoming] = useState({ calendar: [], earnings: null, loaded: false, error: null });

  // Subscriptions — drives the Following toggle. A guest gets a 401, which is not an error state
  // for this block: it means "not following", and the toggle offers sign-in-gated follow exactly as
  // the rest of the app does.
  const loadSubscriptions = useCallback(() => {
    let alive = true;
    listSubscriptions()
      .then((res) => {
        if (!alive) return;
        const list = Array.isArray(res?.subscriptions) ? res.subscriptions : [];
        setSubscriptions({ list, loaded: true, error: null });
      })
      .catch((err) => {
        if (!alive) return;
        setSubscriptions({ list: [], loaded: true, error: err });
      });
    return () => { alive = false; };
  }, []);

  useEffect(() => loadSubscriptions(), [loadSubscriptions]);

  // This ticker's material events. Filtered server-side to the caller's own follow set, so an
  // unfollowed company returns an empty feed rather than another user's data.
  useEffect(() => {
    let alive = true;
    setEvents((s) => ({ ...s, loaded: false, error: null }));
    fetchFollowing({ tickers: [ticker], limit: 12 })
      .then((res) => {
        if (!alive) return;
        setEvents({
          items: Array.isArray(res?.items) ? res.items : [],
          degraded: Array.isArray(res?.degraded) ? res.degraded : [],
          loaded: true,
          error: null,
        });
      })
      .catch((err) => {
        if (!alive) return;
        setEvents({ items: [], degraded: [], loaded: true, error: err });
      });
    return () => { alive = false; };
  }, [ticker]);

  // Upcoming — the economic calendar window plus this ticker's next earnings date. Both are
  // descriptive dates and neither is a trade trigger.
  useEffect(() => {
    let alive = true;
    const from = new Date();
    const to = new Date();
    to.setDate(to.getDate() + 45);
    setUpcoming((s) => ({ ...s, loaded: false, error: null }));
    Promise.allSettled([fetchCalendar(isoDay(from), isoDay(to)), fetchNextEarnings(ticker)])
      .then(([cal, earn]) => {
        if (!alive) return;
        setUpcoming({
          calendar: cal.status === "fulfilled" && Array.isArray(cal.value?.events) ? cal.value.events : [],
          earnings: earn.status === "fulfilled" ? earn.value : null,
          loaded: true,
          error: cal.status === "rejected" && earn.status === "rejected" ? cal.reason : null,
        });
      });
    return () => { alive = false; };
  }, [ticker]);

  const following = subscriptions.list.find(
    (s) => String(s?.ticker || "").toUpperCase() === String(ticker).toUpperCase()
  ) || null;

  // toggleFollow is a WRITE and runs on click only. It refreshes the subscription list afterwards
  // so the chip reflects the server rather than an optimistic guess.
  const toggleFollow = useCallback(async () => {
    if (following) await unfollowTicker(following.id);
    else await followTicker(ticker);
    const res = await listSubscriptions().catch(() => null);
    const list = Array.isArray(res?.subscriptions) ? res.subscriptions : [];
    setSubscriptions({ list, loaded: true, error: null });
  }, [following, ticker]);

  return { subscriptions, following, toggleFollow, events, upcoming };
}
