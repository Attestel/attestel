import { useCallback, useEffect, useRef, useState } from "react";
import { cx } from "../../lib/cx.js";
import { useAuth } from "../../auth/AuthContext.jsx";
import { Badge, Button, Label, Panel, SkeletonText } from "../ui/index.js";
import { DestinationHeader } from "../shell/DestinationHeader.jsx";
import { Icon } from "../shell/icons.jsx";
import {
  AuthRequiredError,
  EVENT_TYPE_LABELS,
  fetchExplore,
  fetchOpportunities,
  fetchScout,
  relativeTime,
} from "../../lib/eventsApi.js";
// FollowButton and useIsColumnWidth are the panel's OWN surface, not the 4A seam — the button
// because Explore and the panel must behave identically on a follow, the hook because
// COLUMN_BREAKPOINT_PX is read in exactly one place and this view needs the same answer to pick its
// grid template.
import EventDetailPanel, { FollowButton, useIsColumnWidth } from "../events/EventDetailPanel.jsx";

// ExploreView — Wave 4 Lane 4B. Discovery, not a firehose (doc §15.3, §16.7).
//
// Six sections, and their KEYS AND ORDER are contract §8's, not a layout preference. Every item
// carries two mandatory reason strings and a one-click Follow, and selecting an item opens the
// event detail beside the feed rather than navigating away from it.
//
// THE ORDER ON SCREEN IS THE SERVER'S ORDER. `whyYouAreSeeingThis`, `possibleReadThrough`, the
// section assignment and the ranking are all computed deterministically in `gateway/explore.go`
// under `exploreWeightsVersion` (AD-7), so the feed is reproducible for the benchmark. Sorting,
// filtering by score or re-bucketing here would silently invalidate that. This file does exactly
// one filter — see DROPPED ITEMS — and no sort at all.
//
// DROPPED ITEMS. Contract §8 makes both reason strings mandatory and `gateway/explore.go` already
// drops an item that cannot produce both. If one arrives anyway it is dropped here too. An
// unexplained recommendation is the exact failure mode doc §16.7 names, and rendering a card with
// an empty reason line would be that failure with a blank space where the reason should be.
//
// Nothing on this screen is a buy, a sell, a hold, a target or a recommendation. "Possible
// read-through: positive semiconductor demand signal" is an information statement about an event
// type reaching a business dimension; it is not advice and it never becomes advice.

// SECTIONS — contract §8's six keys, in the contract's order, with doc §16.7's labels. The
// one-line purpose statement under each heading is what makes an empty section legible: a section
// that is empty because the user already follows everything relevant is a SUCCESS.
const SECTIONS = [
  {
    key: "forYou",
    label: "For You",
    purpose: "Ranked for the companies you follow and how you have been reading.",
  },
  {
    key: "marketMoving",
    label: "Market Moving",
    purpose: "High-importance events across the whole visible corpus, whoever they touch.",
  },
  {
    key: "relatedToYourCompanies",
    label: "Related to Your Companies",
    purpose: "Events about other companies that reach the ones you follow.",
  },
  {
    key: "earnings",
    label: "Earnings",
    purpose: "Results and guidance in the current window.",
  },
  {
    key: "macro",
    label: "Macro",
    purpose: "Releases and central-bank events that set the backdrop.",
  },
  {
    key: "trending",
    label: "Trending",
    purpose: "Events many sources are covering at once.",
  },
];

// The two markers gateway/feeds.go publishes when the event store cannot be reached. Explore
// refuses to rank raw headlines rather than fabricate a discovery feed, and that refusal is
// rendered as a stance.
const EVENTS_DOWN = ["events:unreachable", "events:unconfigured"];

// hasBothReasons is the whole of this file's client-side filtering.
const hasBothReasons = (item) =>
  Boolean(item?.whyYouAreSeeingThis?.trim()) && Boolean(item?.possibleReadThrough?.trim());

export default function ExploreView({
  level,
  onOpenCompany,
  onAsk,
  onAuthRequired,
  // renderDetail — the host's own detail renderer, so the integrator can wire ONE panel instance
  // across Explore and Following. Absent, Explore renders its own EventDetailPanel, which is how
  // this lane can be verified end to end without waiting on the shell.
  renderDetail,
}) {
  const { user, promptSignIn } = useAuth();
  // One breakpoint, read from the panel's single constant — see useIsColumnWidth.
  const isColumn = useIsColumnWidth();
  const [status, setStatus] = useState("loading"); // loading | ready | error
  const [payload, setPayload] = useState(null);
  const [errMessage, setErrMessage] = useState("");
  const [nonce, setNonce] = useState(0);
  const [openEventId, setOpenEventId] = useState("");
  const invokerRef = useRef(null);

  const requireAuth = useCallback(() => {
    if (onAuthRequired) onAuthRequired();
    else promptSignIn();
  }, [onAuthRequired, promptSignIn]);

  useEffect(() => {
    let alive = true;
    setStatus("loading");
    setErrMessage("");
    Promise.all([
      fetchExplore({ limit: 20 }),
      fetchScout({ limit: 12 }).catch((error) => ({
        candidates: [],
        degraded: ["scout:unavailable"],
        error: error?.message || "Discovery Scout could not be loaded.",
      })),
      fetchOpportunities({ limit: 12 }).catch((error) => ({
        candidates: [],
        degraded: ["opportunities:unavailable"],
        error: error?.message || "Early Opportunity Radar could not be loaded.",
      })),
    ])
      .then(([body, scout, opportunities]) => {
        if (!alive) return;
        setPayload({ ...body, scout, opportunities });
        setStatus("ready");
      })
      .catch((err) => {
        if (!alive) return;
        if (err instanceof AuthRequiredError) {
          requireAuth();
          setErrMessage("Sign in to see a personalised Explore feed.");
        } else {
          setErrMessage(err?.message || "Explore could not be loaded.");
        }
        setStatus("error");
      });
    return () => {
      alive = false;
    };
  }, [nonce, user?.id, requireAuth]);

  const sections = payload?.sections || {};
  const scout = payload?.scout || { candidates: [], degraded: [] };
  const opportunities = payload?.opportunities || { candidates: [], degraded: [] };
  const degraded = Array.isArray(payload?.degraded) ? payload.degraded : [];
  const eventsDown = degraded.some((d) => EVENTS_DOWN.includes(d));

  // Selecting an event must not unmount the feed. The feed subtree below keeps the same element
  // identity whether or not the panel is open — only the grid template changes — so React
  // re-lays-out the column and never re-mounts it, and its scroll position survives.
  const openEvent = (eventId, invoker) => {
    invokerRef.current = invoker || null;
    setOpenEventId(eventId);
  };
  const closeEvent = () => {
    setOpenEventId("");
    // The column variant is not a modal and does not manage focus for the user; putting focus back
    // on the card they came from is still the courteous thing when they close it by hand.
    invokerRef.current?.focus?.();
    invokerRef.current = null;
  };

  const detail = openEventId
    ? renderDetail
      ? renderDetail(openEventId, {
          onClose: closeEvent,
          // §9.18: the disclosure the SERVER sent on the item that produced this card, threaded to
          // the panel it qualifies. The panel prefers its own response's `disclaimer` and falls
          // back to this; with neither, it withholds the direction and says so.
          disclosure: disclosureFor(sections, openEventId),
        })
      : (
        <EventDetailPanel
          eventId={openEventId}
          open
          onClose={closeEvent}
          onOpenCompany={onOpenCompany}
          onAsk={onAsk}
          onAuthRequired={requireAuth}
          disclosure={disclosureFor(sections, openEventId)}
        />
      )
    : null;

  return (
    <div className="flex flex-col gap-3">
      {/* Folio and title come from `lib/routes.js` via DestinationHeader now that `explore` is a
          routed destination — the hand-written `folio="Orient"` printed no number beside neighbours
          reading "02 · Monitor" and "04 · Orient · Monitor". */}
      <DestinationHeader
        view="explore"
        subtitle="Events beyond your watchlist, ranked by the gateway and explained on every card. Nothing appears here without a reason attached to it."
        actions={
          <Button size="sm" variant="ghost" onClick={() => setNonce((n) => n + 1)} aria-label="Reload Explore">
            <Icon name="reload" size={14} /> Reload
          </Button>
        }
      />

      {!user && status === "ready" && (
        <Panel className="border-line">
          <div className="flex flex-wrap items-center gap-3">
            <Icon name="user" size={16} className="text-muted" />
            <p className="m-0 flex-1 text-[13px] leading-[1.6] text-muted">
              You are browsing as a guest. Explore still ranks the market-wide sections; the ones
              that depend on what you follow stay empty, and Follow will ask you to sign in.
            </p>
            <Button size="sm" onClick={requireAuth}>Sign in</Button>
          </div>
        </Panel>
      )}

      {eventsDown && <DegradedBanner markers={degraded} />}

      {status === "error" && (
        <Panel danger>
          <div className="flex flex-wrap items-center gap-3">
            <Icon name="warning" size={16} className="text-down" />
            <p className="m-0 flex-1 text-[13px] text-down">{errMessage}</p>
            <Button size="sm" onClick={() => setNonce((n) => n + 1)}>Try again</Button>
          </div>
        </Panel>
      )}

      {/* THE THREE-ZONE LAYOUT (doc §16.14) lives here, inside the destination, not in the shell.
          At >= COLUMN_BREAKPOINT_PX the detail is a second grid column and the feed simply narrows;
          below it the panel becomes an overlay and this grid stays single-column. */}
      <div
        className={cx(
          "grid items-start gap-4",
          detail && isColumn ? "grid-cols-[minmax(0,1fr)_minmax(420px,560px)]" : "grid-cols-1"
        )}
      >
        <div className="flex min-w-0 flex-col gap-3">
          {status === "loading" && <LoadingSections />}
          {status === "ready" && (
            <>
              <OpportunitySection payload={opportunities} onOpenCompany={onOpenCompany} />
              <ScoutSection
                payload={scout}
                onOpenCompany={onOpenCompany}
                onAuthRequired={requireAuth}
              />
            </>
          )}
          {status === "ready" && SECTIONS.map((section) => (
              <ExploreSection
                key={section.key}
                section={section}
                items={(sections[section.key] || []).filter(hasBothReasons)}
                dropped={(sections[section.key] || []).length - (sections[section.key] || []).filter(hasBothReasons).length}
                eventsDown={eventsDown}
                level={level}
                openEventId={openEventId}
                onOpenEvent={openEvent}
                onAuthRequired={requireAuth}
              />
            ))}
        </div>

        {detail}
      </div>

      {status === "ready" && payload?.weightsVersion && (
        <p className="num px-1 text-[11px] text-dim">
          Ranked by {payload.weightsVersion} · read at {payload.asOf || "unknown cutoff"}
        </p>
      )}
    </div>
  );
}

const SCOUT_BAND = {
  high_attention: { label: "High attention", variant: "accent" },
  monitor: { label: "Monitor", variant: "ok" },
  emerging: { label: "Emerging", variant: "dim" },
};

const OPPORTUNITY_STATE = {
  emerging: { label: "Emerging", variant: "accent" },
  confirmed: { label: "Confirmed setup", variant: "ok" },
  extended: { label: "Extended · no chase", variant: "warn" },
  invalidated: { label: "Invalidated", variant: "dim" },
};

const signedPct = (value) => {
  const number = Number(value);
  if (!Number.isFinite(number)) return "—";
  return `${number > 0 ? "+" : ""}${number.toFixed(1)}%`;
};

function OpportunitySection({ payload, onOpenCompany }) {
  const candidates = Array.isArray(payload?.candidates)
    ? payload.candidates.filter((item) => item?.ticker && item?.state && item?.reason)
    : [];
  const degraded = Array.isArray(payload?.degraded) ? payload.degraded : [];
  const stale = degraded.includes("opportunities:stale");
  const unavailable = degraded.includes("opportunities:no-runs")
    || degraded.includes("opportunities:unavailable")
    || degraded.includes("events:unreachable")
    || degraded.includes("events:unconfigured");

  return (
    <Panel variant="glass" bodyClassName="p-0">
      <div className="px-5 pb-3 pt-5">
        <div className="flex flex-wrap items-center gap-2">
          <Label as="h2" tone="accent">Early Opportunity Radar</Label>
          <Badge variant="dim">Completed daily bars</Badge>
          <Badge variant="dim">Model-free</Badge>
        </div>
        <p className="m-0 mt-1.5 text-[12.5px] leading-[1.55] text-muted">
          Developing price setups combined with stored event and catalyst context. Large moves are
          separated as extended so a late discovery is never presented as an early setup.
        </p>
        <p className="m-0 mt-1.5 text-[11.5px] leading-[1.5] text-dim">
          Setup evidence is an ordering score, not a probability. Paper eligibility is not assessed
          here and no card can open a paper position.
        </p>
      </div>

      {candidates.length === 0 ? (
        <div className="px-5 pb-5">
          <p className="m-0 rounded-xl border border-dashed border-line2 bg-white/[.015] px-4 py-6 text-center text-[13px] leading-[1.6] text-muted">
            {stale
              ? "The latest radar snapshot is stale, so its setups are withheld until a fresh completed-bar pass lands."
              : unavailable
              ? "No stored radar pass is available yet. The production opportunity-radar lane must complete once."
              : "No covered company currently clears the versioned early-setup rules. That is an honest empty result."}
          </p>
        </div>
      ) : (
        <ul className="flex flex-col">
          {candidates.map((item) => (
            <OpportunityCard
              key={`${payload.runId || "opportunities"}:${item.ticker}`}
              item={item}
              onOpenCompany={onOpenCompany}
            />
          ))}
        </ul>
      )}

      {(payload?.detectorVersion || payload?.asOf) && (
        <p className="border-t border-line px-5 py-2.5 text-[11px] text-dim">
          {payload.detectorVersion || "Early Opportunity Radar"}
          {payload.asOf ? ` · ${relativeTime(payload.asOf)}` : ""}
          {payload?.coverage?.benchmark?.ticker
            ? ` · relative strength vs ${payload.coverage.benchmark.ticker}` : ""}
        </p>
      )}
    </Panel>
  );
}

function OpportunityCard({ item, onOpenCompany }) {
  const state = OPPORTUNITY_STATE[item.state] || OPPORTUNITY_STATE.emerging;
  const facts = item.facts || {};
  const score = Number.isFinite(Number(item.setupScore))
    ? Math.round(Number(item.setupScore) * 100) : null;
  const risks = Array.isArray(item.riskFlags) ? item.riskFlags : [];

  return (
    <li className="border-t border-line px-5 py-4 first:border-t-0">
      <div className="flex flex-wrap items-center gap-2">
        <span className="num text-[13px] font-semibold tracking-[.02em] text-fg">{item.ticker}</span>
        <Badge variant={state.variant}>{state.label}</Badge>
        {score != null && <Badge variant="dim">setup evidence {score}/100</Badge>}
        {item.source && <Badge variant="dim">{item.source}</Badge>}
        {item.barTime && <span className="ml-auto text-[11.5px] text-dim">bar {item.barTime}</span>}
      </div>

      <p className="m-0 mt-2.5 text-[13px] leading-[1.6] text-fg">{item.reason}</p>

      <div className="mt-3 grid grid-cols-2 gap-2 sm:grid-cols-4">
        <OpportunityFact label="1 session" value={signedPct(facts.return1dPct)} />
        <OpportunityFact label="2 sessions" value={signedPct(facts.return2dPct)} />
        <OpportunityFact label="vs benchmark · 5D" value={signedPct(facts.excessReturn5dPct)} />
        <OpportunityFact
          label="20D volume"
          value={Number.isFinite(Number(facts.relativeVolume20))
            ? `${Number(facts.relativeVolume20).toFixed(1)}x` : "—"}
        />
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-2">
        {risks.map((risk) => <Badge key={risk} variant="warn">{risk}</Badge>)}
        <Badge variant="dim">paper: not assessed</Badge>
        <Button className="ml-auto" size="sm" onClick={() => onOpenCompany?.(item.ticker)}>
          Open research <Icon name="caret" size={13} />
        </Button>
      </div>

      {item.disclaimer && (
        <p className="m-0 mt-2 text-[11px] leading-[1.45] text-dim">{item.disclaimer}</p>
      )}
    </li>
  );
}

function OpportunityFact({ label, value }) {
  return (
    <div className="rounded-lg border border-line bg-white/[.015] px-2.5 py-2">
      <div className="text-[10.5px] uppercase tracking-[.06em] text-dim">{label}</div>
      <div className="num mt-1 text-[12.5px] text-fg">{value}</div>
    </div>
  );
}

function ScoutSection({ payload, onOpenCompany, onAuthRequired }) {
  const candidates = Array.isArray(payload?.candidates)
    ? payload.candidates.filter((item) => item?.ticker && item?.whyNow && item?.whyYouAreSeeingThis)
    : [];
  const degraded = Array.isArray(payload?.degraded) ? payload.degraded : [];
  const stale = degraded.includes("scout:stale");
  const unavailable = degraded.includes("scout:no-runs") || degraded.includes("scout:unavailable");

  return (
    <Panel variant="glass" bodyClassName="p-0">
      <div className="px-5 pb-3 pt-5">
        <div className="flex flex-wrap items-center gap-2">
          <Label as="h2" tone="accent">Discovery Scout</Label>
          <Badge variant="dim">Company-level</Badge>
        </div>
        <p className="m-0 mt-1.5 text-[12.5px] leading-[1.55] text-muted">
          Companies that deserve a research file opened now, ranked from stored events, catalysts,
          real completed bars, and their relationship to your existing coverage.
        </p>
        <p className="m-0 mt-1.5 text-[11.5px] leading-[1.5] text-dim">
          Research leads, not investment recommendations. Scout never starts Qwen or paper trades.
        </p>
      </div>

      {candidates.length === 0 ? (
        <div className="px-5 pb-5">
          <p className="m-0 rounded-xl border border-dashed border-line2 bg-white/[.015] px-4 py-6 text-center text-[13px] leading-[1.6] text-muted">
            {stale
              ? "The latest Scout pass is stale, so its leads are withheld until the production lane completes again."
              : unavailable
              ? "No stored Scout run is available yet. The production Scout lane must complete once before leads can appear."
              : "No company currently has enough fresh, real evidence to enter the research queue."}
          </p>
        </div>
      ) : (
        <ul className="flex flex-col">
          {candidates.map((item) => (
            <ScoutCard
              key={`${payload.runId || "scout"}:${item.ticker}`}
              item={item}
              onOpenCompany={onOpenCompany}
              onAuthRequired={onAuthRequired}
            />
          ))}
        </ul>
      )}

      {(payload?.scoreVersion || payload?.asOf) && (
        <p className="border-t border-line px-5 py-2.5 text-[11px] text-dim">
          {payload.scoreVersion || "Scout"} · personalized by {payload.personalizationVersion || "stored evidence"}
          {payload.asOf ? ` · ${relativeTime(payload.asOf)}` : ""}
        </p>
      )}
    </Panel>
  );
}

function ScoutCard({ item, onOpenCompany, onAuthRequired }) {
  const band = SCOUT_BAND[item.attentionBand] || SCOUT_BAND.emerging;
  const evidenceCount = Array.isArray(item.evidence) ? item.evidence.length : 0;
  const related = Array.isArray(item.relatedToYourCompanies) ? item.relatedToYourCompanies : [];
  const when = relativeTime(item.latestEvidenceAt);

  return (
    <li className="border-t border-line px-5 py-4 first:border-t-0">
      <div className="flex flex-wrap items-center gap-2">
        <span className="num text-[13px] font-semibold tracking-[.02em] text-fg">{item.ticker}</span>
        <Badge variant={band.variant}>{band.label}</Badge>
        {related.length > 0 && <Badge variant="dim">Related to {related.join(", ")}</Badge>}
        {when && <span className="ml-auto text-[11.5px] text-dim">{when}</span>}
      </div>

      <dl className="mt-3 flex flex-col gap-2.5">
        <div>
          <Label as="dt">Why it surfaced</Label>
          <dd className="m-0 mt-1 text-[13px] leading-[1.6] text-muted">
            {item.whyYouAreSeeingThis}
          </dd>
        </div>
        <div>
          <Label as="dt" tone="accent">Why now</Label>
          <dd className="m-0 mt-1 text-[13px] leading-[1.6] text-fg">{item.whyNow}</dd>
        </div>
      </dl>

      <div className="mt-3.5 flex flex-wrap items-center gap-2">
        <Badge variant="dim">{evidenceCount} evidence {evidenceCount === 1 ? "item" : "items"}</Badge>
        {item.dataState && item.dataState !== "live" && <Badge variant="warn">{item.dataState}</Badge>}
        <span className="ml-auto flex items-center gap-2">
          <FollowButton
            ticker={item.ticker}
            followed={item.follow?.followed}
            onAuthRequired={onAuthRequired}
            size="sm"
          />
          <Button size="sm" onClick={() => onOpenCompany?.(item.ticker)}>
            Open research <Icon name="caret" size={13} />
          </Button>
        </span>
      </div>

      {item.disclaimer && (
        <p className="m-0 mt-2 text-[11px] leading-[1.45] text-dim">{item.disclaimer}</p>
      )}
    </li>
  );
}

// disclosureFor recovers the SERVER's §9.18 disclosure string for an event from the Explore
// response that produced the card. `GET /api/events/{id}` does not carry one (it is a byte-verbatim
// proxy of the event store), and this app will not compose one in the browser, so the panel is
// handed the string the server already sent with the item.
function disclosureFor(sections, eventId) {
  for (const { key } of SECTIONS) {
    for (const item of sections?.[key] || []) {
      if (item.id === eventId && item.disclaimer) return item.disclaimer;
    }
  }
  return "";
}

function LoadingSections() {
  return (
    <>
      {[0, 1, 2].map((i) => (
        <Panel key={i}>
          <SkeletonText lines={4} />
        </Panel>
      ))}
    </>
  );
}

// The degraded state is a STANCE, not an error. The gateway is up and answered; it declined to
// rank raw headlines it could not explain. Saying so is more useful than a red failure box.
function DegradedBanner({ markers }) {
  return (
    <Panel className="border-warn/40 bg-warn/[.06]">
      <div className="flex items-start gap-3">
        <Icon name="warning" size={17} className="mt-0.5 flex-none text-warn" />
        <div className="min-w-0">
          <div className="text-[13.5px] font-[550] text-warn">
            Discovery is unavailable while the event service is down.
          </div>
          <p className="mt-1.5 text-[13px] leading-[1.6] text-muted">
            We are not ranking raw headlines. Every card on this screen has to say why you are
            seeing it and how the event could reach a company; without the event store there is no
            honest way to produce either, so the sections stay empty rather than filling with
            unexplained items.
          </p>
          <div className="mt-2 flex flex-wrap gap-1.5">
            {markers.map((m) => (
              <Badge key={m} variant="warn">{m}</Badge>
            ))}
          </div>
        </div>
      </div>
    </Panel>
  );
}

function ExploreSection({ section, items, dropped, eventsDown, openEventId, onOpenEvent, onAuthRequired }) {
  return (
    <Panel variant="glass" bodyClassName="p-0">
      <div className="px-5 pb-3 pt-5">
        <Label as="h2" tone="accent">{section.label}</Label>
        <p className="m-0 mt-1.5 text-[12.5px] leading-[1.55] text-muted">{section.purpose}</p>
      </div>

      {items.length === 0 ? (
        <div className="px-5 pb-5">
          <p className="m-0 rounded-xl border border-dashed border-line2 bg-white/[.015] px-4 py-6 text-center text-[13px] leading-[1.6] text-muted">
            {eventsDown
              ? "Nothing here right now — the event store is unreachable."
              : "Nothing here right now. That is a result, not a fault: no event in the current window earned a place in this section."}
          </p>
        </div>
      ) : (
        <ul className="flex flex-col">
          {items.map((item) => (
            <ExploreCard
              key={`${section.key}:${item.id}`}
              item={item}
              selected={openEventId === item.id}
              onOpenEvent={onOpenEvent}
              onAuthRequired={onAuthRequired}
            />
          ))}
        </ul>
      )}

      {dropped > 0 && (
        <p className="border-t border-line px-5 py-2.5 text-[11.5px] leading-[1.5] text-dim">
          {dropped === 1 ? "One item was" : `${dropped} items were`} withheld from this section for
          arriving without both reason lines. An unexplained recommendation is not shown.
        </p>
      )}
    </Panel>
  );
}

// The Explore card. Built here rather than shared with Lane 4A's EventCard on purpose (locked
// decision 9): an Explore item carries two reason strings and a Follow affordance and has no
// per-user read state, so the two cards are genuinely different objects. They stay siblings through
// the shared token and primitive layer, not through a component that would couple two live lanes.
function ExploreCard({ item, selected, onOpenEvent, onAuthRequired }) {
  const ticker = item.follow?.ticker || item.primaryTicker || "";
  const typeLabel = EVENT_TYPE_LABELS[item.eventType] || item.eventType || "";
  const when = relativeTime(item.publishedAtISO);

  return (
    <li
      className={cx(
        "border-t border-line px-5 py-4 transition-colors first:border-t-0",
        selected ? "bg-accent/[.07]" : "hover:bg-white/[.02]"
      )}
      aria-current={selected ? "true" : undefined}
    >
      {/* Metadata line — ticker and event type are context, deliberately quieter than the title. */}
      <div className="mb-1.5 flex flex-wrap items-center gap-2">
        {ticker && <span className="num text-[12px] font-semibold tracking-[.02em] text-fg">{ticker}</span>}
        {typeLabel && <Label>{typeLabel}</Label>}
        {item.officialConfirmed && (
          <Badge variant="ok" title="Supported by an official filing">Confirmed</Badge>
        )}
        {when && <span className="ml-auto text-[11.5px] text-dim">{when}</span>}
      </div>

      {/* Title dominant, and the button IS the title — one target, keyboard reachable, no nested
          interactive elements inside a clickable row. */}
      <button
        type="button"
        onClick={(e) => onOpenEvent(item.id, e.currentTarget)}
        className="block w-full rounded-md text-left text-[15px] font-[550] leading-[1.35] tracking-[-0.02em] text-fg transition-colors hover:text-accent"
      >
        {item.title}
      </button>

      {/* The two mandatory reasons, each under its own micro-label. Neither is ever blank: an item
          missing one never reaches this component. */}
      <dl className="mt-3 flex flex-col gap-2.5">
        <div>
          <Label as="dt">Why you are seeing this</Label>
          <dd className="m-0 mt-1 text-[13px] leading-[1.6] text-muted">{item.whyYouAreSeeingThis}</dd>
        </div>
        <div>
          <Label as="dt" tone="llm">Possible read-through</Label>
          <dd className="m-0 mt-1 text-[13px] leading-[1.6] text-fg">{item.possibleReadThrough}</dd>
          {/* §9.18: the disclosure stays ADJACENT to the output it qualifies, and it is the
              server's string rendered verbatim — never composed here. */}
          {item.disclaimer && (
            <dd className="m-0 mt-1.5 text-[11.5px] leading-[1.5] text-dim">{item.disclaimer}</dd>
          )}
        </div>
      </dl>

      <div className="mt-3.5 flex flex-wrap items-center gap-2">
        {item.readThroughBasis && (
          <Badge
            variant="dim"
            title="Which stage of the server's three-stage composer wrote the read-through (contract §9.51)"
          >
            basis: {item.readThroughBasis}
          </Badge>
        )}
        {item.dataState && item.dataState !== "live" && (
          <Badge variant={item.dataState === "synthetic" ? "warn" : "dim"}>{item.dataState}</Badge>
        )}
        {ticker && (
          <span className="ml-auto">
            <FollowButton
              ticker={ticker}
              followed={item.follow?.followed}
              onAuthRequired={onAuthRequired}
              size="sm"
            />
          </span>
        )}
      </div>
    </li>
  );
}
