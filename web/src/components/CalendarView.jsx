import { useEffect, useMemo, useState } from "react";
import { fetchCalendar } from "../lib/api.js";
import { listSubscriptions } from "../lib/eventsApi.js";
import { cx } from "../lib/cx.js";
import { SkeletonText, Tabs } from "./ui/index.js";
import ReviewCommitments from "./monitoring/ReviewCommitments.jsx";
import ReactionEvidence from "./events/ReactionEvidence.jsx";
import { DestinationHeader } from "./shell/DestinationHeader.jsx";

// Catalyst Calendar is an agenda over Attestel's stored event corpus. Loading this component is a
// read-only operation: providers are reached by the separate ingestion job, never by the browser.

const VIEWS = [
  { value: "mine", label: "My Calendar" },
  { value: "all", label: "All Events" },
  { value: "history", label: "History" },
];

function isoDay(date) {
  const y = date.getFullYear();
  const m = String(date.getMonth() + 1).padStart(2, "0");
  const d = String(date.getDate()).padStart(2, "0");
  return `${y}-${m}-${d}`;
}

function localDate(value) {
  const [y, m, d] = String(value || "").slice(0, 10).split("-").map(Number);
  return y && m && d ? new Date(y, m - 1, d) : null;
}

function fmtDay(value) {
  const date = localDate(value);
  return date
    ? date.toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric" })
    : value;
}

function relDay(value) {
  const date = localDate(value);
  if (!date) return "";
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const days = Math.round((date - today) / 86400000);
  if (days === 0) return "today";
  if (days === 1) return "tomorrow";
  if (days === -1) return "yesterday";
  if (days > 1 && days < 7) return `in ${days} days`;
  if (days < -1 && days > -7) return `${Math.abs(days)} days ago`;
  return days > 0 ? `in ${Math.round(days / 7)} weeks` : `${Math.round(Math.abs(days) / 7)} weeks ago`;
}

function eventDate(event) {
  return event.date || String(event.scheduledAt || "").slice(0, 10);
}

function eventTime(event) {
  if (event.localTime) return event.localTime;
  if (!event.scheduledAt || /T00:00:00Z$/.test(event.scheduledAt)) return "";
  const parsed = new Date(event.scheduledAt);
  return Number.isNaN(parsed.getTime())
    ? ""
    : parsed.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

function timezoneLabel(event) {
  if (event.timezone === "America/New_York") return "ET";
  if (event.localTime && event.timezone) return event.timezone;
  return "";
}

function SourceBadge({ event }) {
  const label = event.sourceTier === "official" ? "official" : event.source || "stored";
  const className = cx(
    "rounded px-1.5 py-0.5 label-mono",
    event.sourceTier === "official"
      ? "bg-llm/15 text-llm"
      : "border border-line2 text-muted"
  );
  if (!event.sourceUrl) return <span className={className}>{label}</span>;
  return (
    <a
      href={event.sourceUrl}
      target="_blank"
      rel="noreferrer"
      className={cx(className, "hover:text-fg")}
      title={`Open ${event.source || "source"}`}
    >
      {label}
    </a>
  );
}

function ImportanceDot({ importance }) {
  const tone = importance === "high" ? "bg-warn" : importance === "low" ? "bg-muted/30" : "bg-muted/50";
  return <span aria-hidden="true" className={cx("mt-[6px] h-2 w-2 shrink-0 rounded-full", tone)} />;
}

function StatusBadge({ status }) {
  const label = status || "scheduled";
  return (
    <span className={cx(
      "rounded border px-1.5 py-0.5 label-mono",
      ["tentative", "cancelled"].includes(label) ? "border-warn/35 text-warn" : "border-line2 text-muted"
    )}>
      {label}
    </span>
  );
}

function RevisionNote({ event }) {
  const revisions = Array.isArray(event.revisions) ? event.revisions : [];
  const latest = [...revisions].reverse().find((item) => [
    "rescheduled", "cancelled", "status_changed", "source_upgraded", "released",
  ].includes(item.changeType));
  if (!latest) return null;
  let text = "Schedule record updated.";
  if (latest.changeType === "rescheduled" && latest.priorScheduledAt) {
    text = `Rescheduled from ${fmtDay(String(latest.priorScheduledAt).slice(0, 10))}.`;
  } else if (latest.changeType === "cancelled") {
    text = "The stored event was cancelled; its prior schedule remains in history.";
  } else if (latest.changeType === "status_changed") {
    const before = latest.priorStatus ? `${latest.priorStatus} → ` : "";
    text = `Status updated: ${before}${latest.status || "updated"}.`;
  } else if (latest.changeType === "source_upgraded") {
    text = "Source upgraded after stronger confirmation became available.";
  } else if (latest.changeType === "released") {
    text = "Actual values were recorded after the event occurred.";
  }
  return <p className="mb-0 mt-1 text-[10.5px] leading-relaxed text-warn/85">{text}</p>;
}

function fmtValue(value) {
  return typeof value === "number" && Number.isFinite(value)
    ? value.toLocaleString(undefined, { maximumFractionDigits: 2 })
    : "—";
}

function ValuesRow({ event }) {
  if (event.previous == null && event.estimate == null && event.actual == null) return null;
  const unit = event.unit ? ` ${event.unit}` : "";
  return (
    <div className="mt-1 flex flex-wrap gap-x-3 font-mono text-[10.5px] text-muted/80">
      <span>Previous <span className="text-fg/80">{fmtValue(event.previous)}{event.previous != null ? unit : ""}</span></span>
      <span>Expected <span className="text-fg/80">{fmtValue(event.estimate)}{event.estimate != null ? unit : ""}</span></span>
      <span>Actual <span className="font-semibold text-fg">{fmtValue(event.actual)}{event.actual != null ? unit : ""}</span></span>
      {typeof event.surprise === "number" && (
        <span>Surprise <span className="text-fg/80">{fmtValue(event.surprise)}{unit}</span></span>
      )}
    </div>
  );
}

export default function CalendarView() {
  const [view, setView] = useState("mine");
  const [state, setState] = useState({
    loading: true, events: [], subscriptions: [], degraded: [], irCoverage: null, error: null,
  });

  useEffect(() => {
    let alive = true;
    const from = new Date();
    const to = new Date();
    from.setDate(from.getDate() - 45);
    to.setDate(to.getDate() + 120);
    Promise.allSettled([fetchCalendar(isoDay(from), isoDay(to)), listSubscriptions()])
      .then(([calendar, subscriptions]) => {
        if (!alive) return;
        const payload = calendar.status === "fulfilled" ? calendar.value : null;
        setState({
          loading: false,
          events: Array.isArray(payload?.events) ? payload.events : [],
          subscriptions: subscriptions.status === "fulfilled" ? subscriptions.value : [],
          degraded: Array.isArray(payload?.degraded) ? payload.degraded : [],
          irCoverage: payload?.irCoverage || null,
          error: calendar.status === "rejected" ? calendar.reason : null,
        });
      });
    return () => { alive = false; };
  }, []);

  const followed = useMemo(
    () => new Set(state.subscriptions.map((item) => String(item?.ticker || "").toUpperCase()).filter(Boolean)),
    [state.subscriptions]
  );
  const today = isoDay(new Date());
  const visible = useMemo(() => state.events.filter((event) => {
    const date = eventDate(event);
    const historical = date < today || ["occurred", "released"].includes(event.status);
    if (view === "history") return historical;
    if (historical) return false;
    if (view === "all") return true;
    return !event.ticker || followed.has(String(event.ticker).toUpperCase());
  }), [followed, state.events, today, view]);

  const groups = useMemo(() => {
    const byDay = new Map();
    for (const event of visible) {
      const day = eventDate(event);
      if (!byDay.has(day)) byDay.set(day, []);
      byDay.get(day).push(event);
    }
    return [...byDay.entries()].sort(([a], [b]) => view === "history" ? b.localeCompare(a) : a.localeCompare(b));
  }, [view, visible]);

  return (
    <>
      <DestinationHeader
        view="calendar"
        subtitle="Forward catalyst context from stored, source-ranked events"
        actions={<Tabs items={VIEWS} value={view} onChange={setView} size="sm" ariaLabel="Calendar scope" />}
        className="mx-auto mb-4 max-w-[900px]"
      />
      <ReviewCommitments />

      <section className="surface-card mx-auto max-w-[900px] overflow-hidden">
        <div className="flex min-h-11 flex-wrap items-center gap-2.5 border-b border-line px-4 py-2 sm:px-[22px]">
          <span className="text-[14px] font-[550] tracking-[-0.02em] text-fg">Catalyst agenda</span>
          <span className="rounded border border-line2 px-1.5 py-1 label-mono text-muted">STORE-BACKED</span>
          <span className="ml-auto label-mono text-muted">{visible.length} events</span>
        </div>

        <p className="border-b border-line px-4 py-3 text-[12.5px] leading-relaxed text-muted sm:px-[22px]">
          Upcoming macro releases and company catalysts, with source authority, lifecycle and a concise explanation of why each event matters. Dates are informational, not trade triggers.
        </p>

        {state.irCoverage && (
          <div className="flex flex-wrap items-center gap-2 border-b border-line px-4 py-2.5 text-[11px] text-muted sm:px-[22px]">
            <span className="label-mono text-dim">Official company sources</span>
            {(state.irCoverage.covered || []).map((company) => (
              <span key={company.ticker} className="rounded border border-llm/30 bg-llm/10 px-1.5 py-0.5 text-llm">
                {company.ticker} configured
              </span>
            ))}
            {(state.irCoverage.missing || []).map((ticker) => (
              <span key={ticker} className="rounded border border-warn/30 bg-warn/5 px-1.5 py-0.5 text-warn">
                {ticker} no official source
              </span>
            ))}
            {(state.irCoverage.degraded || []).length > 0 && (
              <span className="text-warn">coverage status unavailable</span>
            )}
          </div>
        )}

        {(state.error || state.degraded.length > 0) && (
          <div className="border-b border-warn/20 bg-warn/5 px-4 py-3 text-[12px] text-muted sm:px-[22px]">
            The canonical event store is unavailable or degraded. No substitute dates were generated.
          </div>
        )}

        {state.loading ? (
          <div className="px-4 py-5 sm:px-[22px]"><SkeletonText lines={8} /></div>
        ) : groups.length === 0 ? (
          <div className="px-4 py-10 text-center sm:px-[22px]">
            <p className="m-0 text-[13px] text-muted">No stored events match this agenda.</p>
            {view === "mine" && followed.size === 0 && (
              <p className="mx-auto mt-2 max-w-[52ch] text-[11.5px] leading-relaxed text-dim">
                Follow companies to add their catalysts. Market-wide macro events still appear when they are stored.
              </p>
            )}
          </div>
        ) : (
          <ul>
            {groups.map(([day, dayEvents]) => (
              <li key={day} className="border-b border-line/50 last:border-b-0">
                <div className="flex items-baseline gap-2 bg-panel2/30 px-4 py-2 sm:px-[22px]">
                  <span className="num text-[12.5px] font-semibold text-fg">{fmtDay(day)}</span>
                  <span className="label-mono text-muted">{relDay(day)}</span>
                </div>
                {dayEvents.map((event) => {
                  const clock = eventTime(event);
                  return (
                    <div key={event.id || `${day}-${event.title}`} className="flex items-start gap-3 px-4 py-3 sm:px-[22px]">
                      <span className="num mt-[1px] w-[58px] shrink-0 text-right font-mono text-[11px] text-muted/70">
                        {clock || "—"}
                      </span>
                      <ImportanceDot importance={event.importance} />
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-1.5">
                          <span className="text-[13.5px] leading-snug text-fg">{event.title || event.name}</span>
                          {event.ticker && <span className="rounded bg-panel2 px-1.5 py-0.5 label-mono text-fg/75">{event.ticker}</span>}
                        </div>
                        <div className="mt-0.5 label-mono text-muted">
                          {event.importance} relevance{clock && timezoneLabel(event) ? ` · ${timezoneLabel(event)}` : ""}
                        </div>
                        {event.description && (
                          <p className="mb-0 mt-1 max-w-[70ch] text-[11.5px] leading-relaxed text-muted">{event.description}</p>
                        )}
                        <RevisionNote event={event} />
                        <ValuesRow event={event} />
                        {/* Phase 4. Stored before/after evidence, opened on demand. It is rendered
                            only in History, where the question is "what happened afterwards" — a
                            forward-looking agenda row has no reaction to show, and a permanently
                            empty block would read as an absence of movement rather than an absence
                            of evidence. */}
                        {view === "history" && event.id && (
                          <details className="mt-1.5">
                            <summary className="cursor-pointer label-mono text-muted hover:text-fg">
                              Before / after
                            </summary>
                            <ReactionEvidence eventId={event.id} />
                          </details>
                        )}
                      </div>
                      <div className="flex shrink-0 flex-col items-end gap-1">
                        <StatusBadge status={event.status} />
                        <SourceBadge event={event} />
                      </div>
                    </div>
                  );
                })}
              </li>
            ))}
          </ul>
        )}

        <p className="border-t border-line px-4 py-3 text-[11px] leading-relaxed text-muted/60 sm:px-[22px]">
          A schedule of known catalysts and observed outcomes. Attestel does not execute trades.
        </p>
      </section>
    </>
  );
}
