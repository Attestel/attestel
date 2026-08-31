import { useEffect, useRef, useState } from "react";
import { AnimatePresence, motion, useReducedMotion } from "framer-motion";
import { cx } from "../../lib/cx.js";
import { listEvents, markEventRead, markEventsRead, AuthRequiredError } from "../../lib/api.js";
import { applyResearchLink } from "../../lib/monitoringApi.js";
import { useAuth } from "../../auth/AuthContext.jsx";

// NotificationsModal — the bell popup. Opens over a scrim, lists the signed-in user's triggered
// alert events (newest first), and marks a notification read when it's clicked. Unread items carry an
// accent bar; the caller's unread badge is kept in sync via onUnread. Fail-silent: if the alerts
// service is down or the user is a guest, it shows a friendly state and the app is unaffected.
// Descriptive notifications only — never a trade action or buy/sell advice.

const relTime = (ts) => {
  const s = Math.max(0, Math.floor(Date.now() / 1000 - ts));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
};

// researchActionLabel renders the event's research verb. The switch is total over the three values
// §4.2 allows, and its default is still a research verb — there is no input that produces a trade
// action here, because there is no trade action in the vocabulary.
const researchActionLabel = (action) => {
  if (action === "review_thesis") return "Review the thesis";
  if (action === "answer_question") return "Answer the open question";
  return "Review the evidence";
};

// onOpenResearch is optional: when the host passes it, clicking a thesis-linked notification sets the
// active company before the hash is applied. Without it the hash still resolves — just against
// whatever company is already active (the D-19 limitation).
export default function NotificationsModal({ open, onClose, onOpenAlerts, onUnread, onOpenResearch }) {
  const reduce = useReducedMotion();
  const { user, promptSignIn } = useAuth();
  const [events, setEvents] = useState([]);
  const [status, setStatus] = useState("loading"); // loading | ready | down
  const panelRef = useRef(null);
  const restoreFocusRef = useRef(null);

  const reportUnread = (list) => onUnread?.(list.filter((e) => !e.read).length);

  // Fetch the feed each time the modal opens (cheap; alerts also drives the badge poll).
  useEffect(() => {
    if (!open) return;
    let alive = true;
    setStatus("loading");
    listEvents(50)
      .then((r) => {
        if (!alive) return;
        const list = r.events || [];
        setEvents(list);
        setStatus("ready");
        onUnread?.(r.unread ?? list.filter((e) => !e.read).length);
      })
      .catch(() => alive && setStatus("down"));
    return () => {
      alive = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  // Esc to close, scroll-lock, focus management.
  useEffect(() => {
    if (!open) return;
    restoreFocusRef.current = document.activeElement;
    const onKey = (e) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        onClose();
      }
    };
    document.addEventListener("keydown", onKey, true);
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    const t = setTimeout(() => panelRef.current?.focus?.(), 40);
    return () => {
      document.removeEventListener("keydown", onKey, true);
      document.body.style.overflow = prevOverflow;
      clearTimeout(t);
      restoreFocusRef.current?.focus?.();
    };
  }, [open, onClose]);

  const markOne = async (ev) => {
    if (ev.read) return;
    // optimistic: flip to read, then reconcile with the server's unread count
    setEvents((prev) => {
      const next = prev.map((e) => (e.id === ev.id ? { ...e, read: true } : e));
      reportUnread(next);
      return next;
    });
    try {
      const r = await markEventRead(ev.id);
      if (typeof r?.unread === "number") onUnread?.(r.unread);
    } catch (err) {
      if (err instanceof AuthRequiredError) {
        promptSignIn();
        return;
      }
      // revert on failure
      setEvents((prev) => {
        const next = prev.map((e) => (e.id === ev.id ? { ...e, read: false } : e));
        reportUnread(next);
        return next;
      });
    }
  };

  const markAll = async () => {
    setEvents((prev) => prev.map((e) => ({ ...e, read: true })));
    onUnread?.(0);
    try {
      await markEventsRead();
    } catch (err) {
      if (err instanceof AuthRequiredError) promptSignIn();
    }
  };

  const unreadCount = events.filter((e) => !e.read).length;

  return (
    <AnimatePresence>
      {open && (
        <div className="fixed inset-0 z-50" role="presentation">
          <motion.div
            className="absolute inset-0 bg-black/60 backdrop-blur-[2px]"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            transition={{ duration: 0.18 }}
            onClick={onClose}
            aria-hidden="true"
          />

          <motion.div
            ref={panelRef}
            role="dialog"
            aria-modal="true"
            aria-label="Notifications"
            tabIndex={-1}
            initial={reduce ? { opacity: 0 } : { opacity: 0, y: -12, scale: 0.98 }}
            animate={reduce ? { opacity: 1 } : { opacity: 1, y: 0, scale: 1 }}
            exit={reduce ? { opacity: 0 } : { opacity: 0, y: -8, scale: 0.98 }}
            transition={{ duration: 0.22, ease: [0.16, 0.8, 0.24, 1] }}
            className="surface-deep absolute right-3 top-16 flex max-h-[72vh] w-[calc(100vw-24px)] flex-col overflow-hidden !rounded-2xl outline-none sm:right-5 sm:w-[400px]"
          >
            {/* Header */}
            <header className="relative flex items-center gap-2.5 border-b border-line px-4 py-3">
              <span className="text-[14px] font-[550] tracking-[-0.02em] text-fg">Notifications</span>
              {unreadCount > 0 && (
                <span className="num rounded-full bg-accent px-2 py-0.5 text-[11px] font-semibold leading-none text-bg">
                  {unreadCount}
                </span>
              )}
              <button
                type="button"
                onClick={markAll}
                disabled={unreadCount === 0}
                className="label-mono ml-auto transition-colors hover:text-fg disabled:opacity-40"
              >
                Mark all read
              </button>
              <button
                type="button"
                onClick={onClose}
                aria-label="Close notifications"
                className="flex h-7 w-7 items-center justify-center rounded-full text-muted transition-colors hover:bg-white/10 hover:text-fg"
              >
                <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
                  <path d="M6 6l12 12M18 6L6 18" />
                </svg>
              </button>
            </header>

            {/* Body */}
            <div className="relative min-h-0 flex-1 overflow-y-auto">
              {status === "loading" && (
                <div className="px-4 py-10 text-center text-[13px] text-muted">Loading…</div>
              )}

              {status === "down" && (
                <div className="px-4 py-10 text-center">
                  <div className="mb-1.5 text-[13.5px] font-semibold text-fg/80">Alerts service unavailable</div>
                  <p className="text-[12.5px] leading-relaxed text-muted">
                    Start it with <code className="rounded bg-panel2 px-1">cd alerts &amp;&amp; go run .</code> — the rest of the app is unaffected.
                  </p>
                </div>
              )}

              {status === "ready" && events.length === 0 && (
                <div className="px-4 py-12 text-center">
                  <div className="mx-auto mb-3.5 flex h-11 w-11 items-center justify-center rounded-full border border-line2">
                    <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden="true">
                      <path d="M6 9a6 6 0 1112 0c0 5 2 6 2 6H4s2-1 2-6zM10 21h4" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" className="text-muted" />
                    </svg>
                  </div>
                  <div className="mb-1.5 text-[14px] font-semibold text-fg/80">
                    {user ? "You're all caught up" : "No notifications"}
                  </div>
                  <p className="mx-auto max-w-[280px] text-[12.5px] leading-relaxed text-muted">
                    {user ? (
                      "Triggered alerts will show up here."
                    ) : (
                      <>
                        <button type="button" onClick={promptSignIn} className="text-accent hover:brightness-125">Sign in</button>{" "}
                        and create alert rules to get notified.
                      </>
                    )}
                  </p>
                </div>
              )}

              {status === "ready" &&
                events.map((ev, i) => {
                  const Row = ev.read ? "div" : "button";
                  return (
                    <Row
                      key={ev.id}
                      {...(ev.read ? {} : { type: "button", onClick: () => markOne(ev) })}
                      className={cx(
                        "block w-full px-4 py-3 text-left transition-colors",
                        i < events.length - 1 && "border-b border-line/50",
                        ev.read ? "opacity-60" : "border-l-2 border-l-accent bg-accent/[0.05] hover:bg-accent/[0.09]"
                      )}
                    >
                      <div className="flex items-start gap-2">
                        {!ev.read && <span className="mt-1.5 h-1.5 w-1.5 shrink-0 rounded-full bg-accent" aria-hidden="true" />}
                        <div className="min-w-0 flex-1">
                          <div className="text-[13px] leading-snug text-fg/90">{ev.message}</div>

                          {/* Thesis context (Step 08, §4.2). Every badge here is server-determined:
                              a bearing appears only when it was deterministic, and a synthetic
                              trigger is labelled wherever it is read. */}
                          {(ev.thesisId || ev.dataState === "synthetic") && (
                            <div className="mt-1 flex flex-wrap items-center gap-1.5">
                              {ev.bearing && (
                                <span className="rounded border border-line2 px-1.5 py-0.5 text-[10px] text-fg/75">
                                  {ev.bearing} your thesis
                                </span>
                              )}
                              {ev.thesisId && !ev.bearing && (
                                <span className="rounded border border-line2 px-1.5 py-0.5 text-[10px] text-muted">
                                  relates to your thesis
                                </span>
                              )}
                              {ev.dataState === "synthetic" && (
                                <span className="rounded border border-warn/50 px-1.5 py-0.5 text-[10px] text-warn">
                                  synthetic data
                                </span>
                              )}
                            </div>
                          )}

                          <div className="num mt-1 text-[11px] text-muted/70">
                            {ev.ticker} · {ev.timeframe} · {ev.type} · {relTime(ev.ts)}
                          </div>

                          {/* The direct link into Research context. It sets the active company FIRST
                              and only then applies the hash — D-19 leaves the company out of the URL,
                              so the other order would land on the right section of the wrong company. */}
                          {ev.thesisId && ev.researchLink && (
                            <span
                              role="link"
                              tabIndex={0}
                              onClick={(e) => {
                                e.stopPropagation();
                                markOne(ev);
                                onClose?.();
                                applyResearchLink(ev.researchLink, { onOpenCompany: onOpenResearch });
                              }}
                              onKeyDown={(e) => {
                                if (e.key !== "Enter" && e.key !== " ") return;
                                e.preventDefault();
                                e.stopPropagation();
                                markOne(ev);
                                onClose?.();
                                applyResearchLink(ev.researchLink, { onOpenCompany: onOpenResearch });
                              }}
                              className="mt-1.5 inline-block cursor-pointer text-[11.5px] text-accent transition-[filter] hover:brightness-125"
                            >
                              {researchActionLabel(ev.researchAction)} →
                            </span>
                          )}
                        </div>
                      </div>
                    </Row>
                  );
                })}
            </div>

            {/* Footer */}
            <div className="flex items-center justify-between border-t border-line px-4 py-2.5">
              <span className="label-mono text-muted">
                Descriptive · not advice
              </span>
              <button
                type="button"
                onClick={() => {
                  onClose();
                  onOpenAlerts?.();
                }}
                className="rounded-full border border-line2 px-3 py-1.5 text-[12px] text-fg/85 transition-colors hover:border-accent/60 hover:text-fg"
              >
                Monitoring rules
              </button>
            </div>
          </motion.div>
        </div>
      )}
    </AnimatePresence>
  );
}
