import { EmptyState, SkeletonText } from "../ui/index.js";
import { Micro, Tag } from "../terminal/bits.jsx";
import { relFromUnix } from "../../lib/format.js";
import { rankTheses, countActive, recentEvents } from "../../lib/attention.js";
import { cx } from "../../lib/cx.js";
import { applyResearchLink, bearingTone } from "../../lib/monitoringApi.js";
import DueReviews from "../monitoring/DueReviews.jsx";

// AttentionPanel — "what needs review". The one block on Today that asks the user to DO something, so it
// sits directly under the lead.
//
// Two sources, both already available and both free of any model call:
//   * the journal's active theses + the single `lastCheck` it stores — ranked by lib/attention.js;
//   * the alerts service's recent triggered events.
//
// Every action is a RESEARCH action ("re-read the evidence", "open the company"), never a trade action.
// Alert messages are rendered as the alerts service wrote them — neutral statements of a condition.
//
// Step 08 filled that seam three ways, all of them additive:
//   * a THIRD source — scheduled reviews that have come due (DueReviews, one deterministic call);
//   * an event that carries a thesisId now renders WITH that context and links straight into Research;
//   * an event WITHOUT one renders exactly as before. Nothing implies a link that isn't stored, which
//     is the whole point of D-10: the system must not silently assert a connection the user didn't make.

function ThesisRow({ item, onOpen }) {
  return (
    <li className="flex flex-wrap items-start gap-x-3 gap-y-1.5 px-[22px] py-3">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="num text-[12px] font-bold text-accent">{item.ticker}</span>
          <Tag tone={item.tone}>{item.label}</Tag>
          {item.checkedAt && (
            <span className="num text-[10.5px] text-muted/70">checked {relFromUnix(item.checkedAt)}</span>
          )}
        </div>
        <p className="mt-1 line-clamp-2 text-[12.5px] leading-relaxed text-fg/80">{item.text}</p>
        {item.summary && (
          <p className="mt-1 line-clamp-2 text-[11.5px] leading-relaxed text-muted">{item.summary}</p>
        )}
      </div>
      {/* Hands off to Research → Thesis for this company — where the claim and its check actually live. */}
      <button
        type="button"
        onClick={() => onOpen(item.ticker)}
        className="shrink-0 rounded-full border border-line2 px-3 py-1.5 text-[12px] font-semibold text-fg/90 transition-colors hover:border-accent/60"
      >
        {item.action} →
      </button>
    </li>
  );
}

function EventRow({ ev, onOpen, onOpenThesis }) {
  const linked = Boolean(ev.thesisId);
  return (
    <li className="flex flex-wrap items-start gap-x-3 gap-y-1 px-[22px] py-2.5">
      <div className="min-w-0 flex-1">
        <div className="text-[12.5px] leading-snug text-fg/85">{ev.message}</div>
        <div className="mt-1 flex flex-wrap items-center gap-1.5">
          {/* Shown only when the server determined a bearing deterministically (§4.2). An event with
              no bearing makes no claim about the thesis, and renders that way. */}
          {ev.bearing && <Tag tone={bearingTone(ev.bearing)}>{ev.bearing} your thesis</Tag>}
          {linked && !ev.bearing && <Tag tone="outline">relates to your thesis</Tag>}
          {ev.dataState === "synthetic" && <Tag tone="warn">synthetic data</Tag>}
        </div>
        <div className="num mt-0.5 text-[10.5px] text-muted/70">
          {ev.ticker} · {ev.timeframe} · {ev.type} · {relFromUnix(ev.ts)}
        </div>
      </div>
      <button
        type="button"
        onClick={() =>
          linked
            ? applyResearchLink(ev.researchLink, { onOpenCompany: onOpenThesis || onOpen })
            : onOpen(ev.ticker)
        }
        className="shrink-0 text-[12px] text-accent transition-[filter] hover:brightness-125"
      >
        {/* A research verb, always — the action a monitoring event offers is never a trade action. */}
        {linked ? "Review evidence →" : `Open ${ev.ticker} →`}
      </button>
    </li>
  );
}

function StaleThesisRow({ marker, onOpen }) {
  return (
    <li className="flex flex-wrap items-start gap-x-3 gap-y-1 px-[22px] py-2.5">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="num text-[12px] font-bold text-accent">{marker.ticker}</span>
          <Tag tone="warn">stale thesis</Tag>
          {marker.asOfISO && <span className="num text-[10.5px] text-muted/70">as of {marker.asOfISO}</span>}
        </div>
        <p className="mt-1 text-[12.5px] leading-relaxed text-fg/80">{marker.reason}</p>
      </div>
      <button
        type="button"
        onClick={() => onOpen(marker.ticker)}
        className="shrink-0 text-[12px] text-accent transition-[filter] hover:brightness-125"
      >
        Review thesis →
      </button>
    </li>
  );
}

export default function AttentionPanel({
  theses,
  thesesStatus, // "loading" | "ready" | "down"
  events,
  eventsStatus,
  monitor,
  monitorStatus,
  user,
  onOpenCompany,
  onOpenThesis,
  onSignIn,
  onCreateThesis,
}) {
  const items = thesesStatus === "ready" ? rankTheses(theses) : [];
  const active = countActive(theses);
  const evs = eventsStatus === "ready" ? recentEvents(events) : [];
  const staleMarkers = monitorStatus === "ready" ? (monitor?.markers || []).filter((m) => m.stale) : [];
  const loading = thesesStatus === "loading" || eventsStatus === "loading" || monitorStatus === "loading";
  const nothing = !loading && items.length === 0 && evs.length === 0 && staleMarkers.length === 0;

  return (
    <section className="overflow-hidden surface-card">
      <div className="flex flex-wrap items-center gap-2.5 border-b border-line px-[22px] py-3">
        <span className="text-[14px] font-semibold text-fg">Needs your review</span>
        {items.length > 0 && (
          <Tag tone={items.some((i) => i.reason === "challenged") ? "down" : "warn"}>
            {items.length} of {active} {active === 1 ? "thesis" : "theses"}
          </Tag>
        )}
        {evs.length > 0 && <Tag tone="outline">{evs.length} recent alerts</Tag>}
        {staleMarkers.length > 0 && <Tag tone="warn">{staleMarkers.length} stale</Tag>}
      </div>

      {loading && (
        <div className="px-[22px] py-4">
          <SkeletonText lines={3} />
        </div>
      )}

      {/* Guest — orientation still works; only the personal record needs an account. */}
      {!loading && !user && (
        <div className="px-[22px] py-4">
          <EmptyState title="Your theses live here">
            The News tab works without an account.{" "}
            <button type="button" onClick={onSignIn} className="text-accent hover:brightness-125">
              Sign in
            </button>{" "}
            to keep theses and get alerts on what would change them.
          </EmptyState>
        </div>
      )}

      {/* Signed in, no theses yet — the activation path. */}
      {!loading && user && thesesStatus === "ready" && active === 0 && (
        <div className="px-[22px] py-4">
          <EmptyState
            title="Start with one thesis"
            action={
              <button
                type="button"
                onClick={onCreateThesis}
                className="rounded-full border border-accent/50 bg-accent/10 px-4 py-2 text-[12.5px] font-semibold text-accent transition-colors hover:bg-accent/16"
              >
                Create your first thesis
              </button>
            }
          >
            Write what you believe about a company and why. Attestel will check it against new evidence and
            tell you when something challenges it.
          </EmptyState>
        </div>
      )}

      {/* Journal unreachable — say so plainly rather than showing an empty list. */}
      {!loading && user && thesesStatus === "down" && (
        <div className="px-[22px] py-4 text-[12.5px] leading-relaxed text-muted">
          Your theses are unavailable — the journal service may be unreachable. The News tab is unaffected.
        </div>
      )}

      {items.length > 0 && (
        <>
          <div className="flex h-8 items-center border-y border-line bg-panel2/30 px-[22px]">
            <Micro>Theses</Micro>
          </div>
          <ul className="divide-y divide-line">
            {items.map((i) => (
              <ThesisRow key={i.id} item={i} onOpen={onOpenThesis || onOpenCompany} />
            ))}
          </ul>
        </>
      )}

      {evs.length > 0 && (
        <>
          <div className="flex h-8 items-center gap-2 border-y border-line bg-panel2/30 px-[22px]">
            <Micro>Recent alerts</Micro>
            <span className="text-[10.5px] text-muted/60">conditions that became true — not advice</span>
          </div>
          <ul className="divide-y divide-line">
            {evs.map((ev) => (
              <EventRow key={ev.id} ev={ev} onOpen={onOpenCompany} onOpenThesis={onOpenThesis} />
            ))}
          </ul>
        </>
      )}

      {staleMarkers.length > 0 && (
        <>
          <div className="flex h-8 items-center gap-2 border-y border-line bg-panel2/30 px-[22px]">
            <Micro>Continuous thesis monitor</Micro>
            <span className="text-[10.5px] text-muted/60">
              deterministic change markers · re-synthesis worker is operator-enabled
            </span>
          </div>
          <ul className="divide-y divide-line">
            {staleMarkers.map((marker) => (
              <StaleThesisRow
                key={marker.thesisId}
                marker={marker}
                onOpen={onOpenThesis || onOpenCompany}
              />
            ))}
          </ul>
        </>
      )}

      {/* Scheduled reviews the user committed to. Self-fetching and deterministic: it renders
          nothing at all when none are due, so the panel's existing states are untouched. */}
      <DueReviews user={user} onOpenCompany={onOpenCompany} onOpenThesis={onOpenThesis} />

      {/* Signed in, has theses, nothing flagged, no alerts fired. */}
      {nothing && user && thesesStatus === "ready" && active > 0 && (
        <div className={cx("px-[22px] py-4")}>
          <EmptyState title="Nothing needs your review">
            All {active} active {active === 1 ? "thesis has" : "theses have"} a recent evidence check and
            no alert has fired. Come back after the next catalyst.
          </EmptyState>
        </div>
      )}

      {/* Alerts service down — local degradation only. */}
      {user && eventsStatus === "down" && (
        <div className="border-t border-line px-[22px] py-2.5 text-[11.5px] text-muted">
          Alert history unavailable (the alerts service may be down). Theses above are unaffected.
        </div>
      )}
      {user && monitorStatus === "down" && (
        <div className="border-t border-line px-[22px] py-2.5 text-[11.5px] text-muted">
          Thesis-monitor status is unavailable. This is not a statement that no thesis went stale.
        </div>
      )}
      {user && monitorStatus === "ready" && monitor?.enabled === false && (
        <div className="border-t border-line px-[22px] py-2.5 text-[11.5px] text-muted">
          Continuous thesis monitoring is off for this deployment, so no stale markers are being created.
        </div>
      )}
    </section>
  );
}
