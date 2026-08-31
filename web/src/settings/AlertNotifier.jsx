import { useEffect, useRef } from "react";
import { useAuth } from "../auth/AuthContext.jsx";
import { useSettings } from "./SettingsContext.jsx";
import { listEvents } from "../lib/api.js";

// AlertNotifier — a null-render component (mounted once in App) that pops a desktop notification when a
// NEW alert event fires. Active only when the signed-in user has opted in AND the browser has granted
// permission. Descriptive alert events only — never advice, never an order.
//
// It owns its own light poll (so it needn't reach into App's bell-badge poll) and de-dupes against the
// newest event id it has already seen, persisted per-user in localStorage. On the first tick after it
// (re)activates it SEEDS that marker to the newest existing event and fires nothing — so pre-existing
// alerts on load never spam. Fail-silent throughout: a down alerts service just means no notifications.

const POLL_MS = 30000; // matches App's alerts cadence; don't hammer the service

export default function AlertNotifier() {
  const { user } = useAuth();
  const { settings } = useSettings();

  // Active only with the preference on AND a live OS grant. The saved preference alone never bypasses
  // the browser permission (Notification.permission is not reactive; the toggle flow re-renders us).
  const active =
    Boolean(user) &&
    settings.browserNotifications &&
    typeof window !== "undefined" &&
    "Notification" in window &&
    window.Notification.permission === "granted";

  const seededRef = useRef(false);

  useEffect(() => {
    if (!active) return;
    const key = `notify.lastEventId.${user.id}`;
    seededRef.current = false; // re-seed whenever we (re)activate or the user changes

    let alive = true;

    const readMarker = () => {
      try {
        return localStorage.getItem(key) || "";
      } catch {
        return "";
      }
    };
    const writeMarker = (id) => {
      try {
        localStorage.setItem(key, id);
      } catch {
        /* ignore (private mode / disabled storage) */
      }
    };

    const tick = async () => {
      let events;
      try {
        const r = await listEvents(5);
        events = r.events || [];
      } catch {
        return; // fail-silent
      }
      if (!alive || !events.length) return;

      const newest = events[0].id; // ListEvents returns newest-first (sorted by ts desc)

      // First tick after (re)activation: seed to the newest existing event, notify NOTHING. This is
      // what stops pre-existing alerts (incl. any that accrued while notifications were off) from
      // backfilling as a burst of notifications.
      if (!seededRef.current) {
        seededRef.current = true;
        writeMarker(newest);
        return;
      }

      // Everything strictly newer than the marker is genuinely new. events is newest-first; collect
      // the new ones until we hit the last-seen id, then fire oldest-first for natural ordering.
      const lastId = readMarker();
      const fresh = [];
      for (const ev of events) {
        if (ev.id === lastId) break;
        fresh.push(ev);
      }
      for (const ev of fresh.reverse()) {
        try {
          // tag: ev.id dedupes if the same id somehow fires twice (e.g. overlapping ticks).
          new Notification("Attestel alert", { body: ev.message, tag: ev.id });
        } catch {
          /* ignore */
        }
      }
      writeMarker(newest);
    };

    tick(); // seeds immediately (fires nothing on this first run)
    const id = setInterval(tick, POLL_MS);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, [active, user?.id]);

  return null;
}
