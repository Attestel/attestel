import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Badge, Button, Panel } from "../../ui/index.js";
import { cx } from "../../../lib/cx.js";
import { deltaColor, fmtPrice, fmtSignedPct, relDay } from "../../../lib/format.js";
import {
  RUN_FAILED,
  RUN_RUNNING,
  directionGate,
  forgetAnalystRun,
  pollAnalystRun,
  recallAnalystRun,
  rememberAnalystRun,
  renderableThesis,
  startAnalystRun,
  thesisFor,
} from "../../../lib/analystApi.js";
import { AuthRequiredError } from "../../../lib/api.js";
import AttestelView from "./AttestelView.jsx";
import { eventTypeLabel, importanceBand, relativeTime } from "../../../lib/eventsApi.js";
import { useOverviewData } from "./useOverviewData.js";

// OverviewSection — the ticker page's first viewport (doc §16.9).
//
// The page opens with an ANSWER, not a wall of metrics: the Attestel view, what changed, what is
// coming, and the latest material events, all above the fold. Everything below the fold is the
// five-tab strip, and nothing in this block is a metric for its own sake.
//
// INVARIANT #4: mounting this section performs NO model call. `useOverviewData` reads only
// deterministic endpoints, and the ONE call in the lane that can reach the model —
// `startAnalystRun` — is bound to the Attestel card's button and to nothing else.
//
// There IS one effect here that fetches, and it is the one exception the async run required: if
// this browser tab already started a run for this ticker, the effect RESUMES WATCHING it through
// `fetchAnalystRun`, a status read that reaches a map lookup in the gateway and no upstream at all.
// It cannot start, restart or extend a run — a remembered id it does not recognise is reported as
// a lost run, never re-POSTed. That distinction is the whole reason the gateway split the two
// routes; see the invariant #4 block in `lib/analystApi.js`.
//
// Density rule: premium chrome, dense data. The Attestel view is a premium card; What Changed,
// Upcoming and Latest Events are dense row sets, deliberately not inflated, so all five blocks fit
// a 1440×900 first viewport with the shell in place.

// ─────────────────────────────────────────────────────────────────────────────────── identity

function IdentityRow({ ticker, name, quote, data, following, onToggleFollow, followBusy, followError }) {
  const price = quote?.price ?? data?.price?.lastClose;
  const chg = quote?.changePct ?? data?.price?.changePct;
  const synthetic = data?.priceSourceIsSynthetic === true;

  return (
    <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
      <div className="min-w-0">
        <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
          <h2 className="text-[22px] font-semibold tracking-[-0.02em] text-fg">{ticker}</h2>
          {name && <span className="truncate text-[13.5px] text-muted">{name}</span>}
          {/* The synthetic banner is a guardrail and stays at least as loud as it is elsewhere:
              nobody may trade on a synthetic bar, so the label travels with the price. */}
          {synthetic && <Badge variant="warn">synthetic bars</Badge>}
          {data?.priceSource && !synthetic && (
            <Badge variant="outline">{data.priceSource}</Badge>
          )}
        </div>
        {price != null && (
          <div className="mt-1 flex items-baseline gap-2.5">
            <span className="num text-[19px] font-semibold text-fg">{fmtPrice(price)}</span>
            {chg != null && (
              // `up`/`down` on the CHANGE value only — never on chrome, never as a focus ring.
              <span className={cx("num text-[13.5px] font-semibold", deltaColor(chg))}>
                {fmtSignedPct(chg)} today
              </span>
            )}
          </div>
        )}
      </div>

      <div className="flex flex-col items-end gap-1">
        <Button
          variant={following ? "secondary" : "primary"}
          size="sm"
          onClick={onToggleFollow}
          disabled={followBusy}
          aria-pressed={Boolean(following)}
        >
          {followBusy ? "Saving…" : following ? "Following" : "Follow"}
        </Button>
        {followError && (
          <span className="max-w-[24ch] text-right text-[11px] leading-snug text-warn">
            {followError}
          </span>
        )}
      </div>
    </div>
  );
}

// ──────────────────────────────────────────────────────────────────────────────── what changed

// WhatChanged renders the ANALYST's own `whatChanged` list (contract §7), which §9.20 names as one
// of the fields that render unconditionally.
//
// MEASURED, AND RECORDED IN THE HANDOFF: the per-ticker `/api/changed` feed this lane's brief names
// as the source DOES NOT EXIST at this tree — contract §9.1 assigns it to Wave 5 Lane B, and the
// gateway's only change surface today is THESIS-scoped (`GET /api/theses/{id}/changes`), not
// ticker-scoped. So the honest source for "what changed about this company's read" is the read's
// own `whatChanged`, which is served, deterministic, and already tied to the events that moved it.
// HEADING, SET AT INTEGRATION. Three surfaces in this wave rendered something called "What
// changed" and they are three different objects: this one (the ANALYST's record of what moved THIS
// company's read), the detail panel's (recorded THESIS changes for one event) and the Today
// digest's (the cross-portfolio feed, which /api/changed does not serve yet). A user who saw a
// populated "What Changed" here and "change tracking is not available" on Today would reasonably
// read one feature as broken in one place. The SEMANTICS are the lane's and unchanged; only the
// heading is narrowed, so each says which object it is.
function WhatChanged({ items, hasRun }) {
  const rows = Array.isArray(items) ? items : [];
  return (
    <Panel title="What changed in this read">
      {rows.length === 0 ? (
        <p className="text-[12.5px] leading-[1.65] text-muted">
          {hasRun
            ? "The run recorded no change to this read since the previous cutoff."
            : "Changes to this read are listed once an analysis has been run."}
        </p>
      ) : (
        <ul className="flex flex-col gap-2">
          {rows.slice(0, 5).map((c, i) => (
            <li key={i} className="flex items-baseline gap-3">
              <span className="num w-[42px] shrink-0 text-right font-mono text-[11px] text-muted/80">
                {relativeTime(c?.at) || "—"}
              </span>
              <span className="min-w-0 flex-1 text-[12.5px] leading-snug text-fg/85">
                {c?.effect || "changed"}
              </span>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

// ─────────────────────────────────────────────────────────────────────────────────── upcoming

function Upcoming({ upcoming, ticker }) {
  const rows = useMemo(() => {
    const out = [];
    if (upcoming.earnings?.date) {
      out.push({ date: upcoming.earnings.date, name: `${ticker} earnings`, kind: "earnings" });
    }
    for (const e of upcoming.calendar) {
      if (e.ticker && String(e.ticker).toUpperCase() !== String(ticker).toUpperCase()) continue;
      if (e.kind === "earnings" && e.date === upcoming.earnings?.date) continue;
      out.push({ date: e.date, name: e.name, kind: e.kind, importance: e.importance });
    }
    return out.sort((a, b) => String(a.date).localeCompare(String(b.date))).slice(0, 5);
  }, [upcoming, ticker]);

  return (
    <Panel title="Upcoming">
      {!upcoming.loaded ? (
        <p className="text-[12.5px] text-muted">Loading scheduled events…</p>
      ) : rows.length === 0 ? (
        <p className="text-[12.5px] leading-[1.65] text-muted">
          No scheduled events in the next six weeks, or the calendar is unreachable.
        </p>
      ) : (
        <ul className="flex flex-col gap-2">
          {rows.map((r, i) => (
            <li key={`${r.date}-${i}`} className="flex items-baseline gap-3">
              <span className="num w-[76px] shrink-0 font-mono text-[11px] text-muted/80">{r.date}</span>
              <span className="min-w-0 flex-1 truncate text-[12.5px] text-fg/85">{r.name}</span>
              <span className="label-mono shrink-0 text-dim">{relDay(r.date)}</span>
            </li>
          ))}
        </ul>
      )}
      {/* Descriptive dates only — never a trade trigger. Same claim the Calendar destination makes,
          because it is the same data. */}
      <p className="mt-2.5 border-t border-line pt-2.5 text-[11px] leading-relaxed text-muted">
        Scheduled dates, informational — not a trade trigger.
      </p>
    </Panel>
  );
}

// ─────────────────────────────────────────────────────────────────────────── latest events

// LatestEvents is a COMPACT three-line row of this lane's own, deliberately NOT Lane 4A's
// `EventCard`: the brief forbids reimplementing it, and the ticker page needs a denser row than the
// feed's three-level card. The display vocabulary comes from `lib/eventsApi.js`, so the words match
// Following and Explore even though the layout does not.
function LatestEvents({ events, onOpenEvent, onAsk, limit = 3, title = "Latest material events" }) {
  const rows = events.items.slice(0, limit);
  const guest = events.error instanceof AuthRequiredError;

  return (
    <Panel title={title}>
      {!events.loaded ? (
        <p className="text-[12.5px] text-muted">Loading events…</p>
      ) : guest ? (
        <p className="text-[12.5px] leading-[1.65] text-muted">
          Sign in to see the material events for the companies you follow.
        </p>
      ) : events.error ? (
        <p className="text-[12.5px] leading-[1.65] text-muted">
          The event feed is unreachable — the rest of this page is unaffected.
        </p>
      ) : rows.length === 0 ? (
        <p className="text-[12.5px] leading-[1.65] text-muted">
          No material events recorded for this company in the last 30 days.
        </p>
      ) : (
        <ul className="flex flex-col divide-y divide-line">
          {rows.map((ev) => {
            const band = importanceBand(ev);
            return (
              <li key={ev.id} className="flex items-start gap-3 py-2.5 first:pt-0 last:pb-0">
                <div className="min-w-0 flex-1">
                  <button
                    type="button"
                    onClick={() => onOpenEvent?.(ev.id)}
                    className="block w-full truncate text-left text-[13px] font-medium leading-snug text-fg hover:text-accent"
                  >
                    {ev.title}
                  </button>
                  <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1">
                    <span className="label-mono text-dim">{eventTypeLabel(ev.eventType)}</span>
                    <span className="label-mono text-dim">·</span>
                    <span className="label-mono text-dim">{relativeTime(ev.publishedAt)}</span>
                    {/* A BAND NAME, never the underlying float — no numeric score on screen. */}
                    {band === "high" && <Badge variant="warn">high importance</Badge>}
                  </div>
                </div>
                {onAsk && (
                  <Button
                    variant="ghost"
                    size="xs"
                    onClick={() => onAsk(ev)}
                    aria-label={`Ask about ${ev.title}`}
                  >
                    Ask
                  </Button>
                )}
              </li>
            );
          })}
        </ul>
      )}
    </Panel>
  );
}

// ────────────────────────────────────────────────────────────────────────────────────── section

// `eventsOnly` is the Events TAB: the same per-ticker feed this section already reads, shown at
// full length with the identity row above it, and without the Attestel view. It is a mode of this
// component rather than a second one because both render the same data from the same hook — a
// separate file would be a copy that drifts.
export default function OverviewSection({ ticker, data, quote, onOpenEvent, onAsk, eventsOnly = false }) {
  const { following, toggleFollow, events, upcoming } = useOverviewData(ticker);

  // The analyst run. State lives here, and `startAnalystRun` is reachable from the click handler
  // below and from nowhere else in this tree (invariant #4).
  const [envelope, setEnvelope] = useState(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  // `runId` is what makes the run survive a navigation or a reload: it is remembered per ticker in
  // sessionStorage and the resume effect below picks it up again.
  const [runId, setRunId] = useState("");
  const runAbort = useRef(null);

  // settle applies a TERMINAL run body. A failed run is stated, not swallowed: the card must never
  // answer a failure by quietly reverting to "Not yet assessed", which is exactly what the 504 used
  // to look like from the user's seat.
  const settle = useCallback((body, tick) => {
    if (!body) return;
    forgetAnalystRun(tick);
    setRunId("");
    setPending(false);
    if (body.status === RUN_FAILED || body.available === false) {
      setError(
        `The analyst run did not finish${body.error ? `: ${body.error}` : "."} Nothing was saved — you can start it again.`
      );
      return;
    }
    setError("");
    setEnvelope(body);
  }, []);

  // watch follows an already-started run to its end. STATUS READS ONLY — see the header. It is
  // reachable from the click handler and from the resume effect, and neither of them can cause a
  // generation.
  const watch = useCallback(
    async (id, tick) => {
      const controller = new AbortController();
      runAbort.current?.abort();
      runAbort.current = controller;
      try {
        const body = await pollAnalystRun(id, { signal: controller.signal });
        if (controller.signal.aborted) return;
        settle(body, tick);
      } catch (err) {
        if (controller.signal.aborted) return;
        setPending(false);
        setRunId("");
        forgetAnalystRun(tick);
        setError(
          err?.code === "unknown_run"
            ? "That run is no longer available — the gateway restarted, or it finished too long ago. Start a new one."
            : "Lost contact with the analyst run. It may still be finishing; start a new one if the card stays empty."
        );
      }
    },
    [settle]
  );

  // RESUME. On mount, and on a ticker change, rejoin a run this tab already started. No POST, no
  // model call — `pollAnalystRun` only ever reads `GET /api/analyst/runs/{id}`.
  useEffect(() => {
    setEnvelope(null);
    setError("");
    const remembered = recallAnalystRun(ticker);
    if (!remembered) {
      setPending(false);
      setRunId("");
      runAbort.current?.abort();
      return undefined;
    }
    setPending(true);
    setRunId(remembered);
    watch(remembered, ticker);
    return () => runAbort.current?.abort();
  }, [ticker, watch]);

  const [followBusy, setFollowBusy] = useState(false);
  const [followError, setFollowError] = useState("");

  // onRun — THE ONLY THING IN THIS TREE THAT CAN REACH THE MODEL, and it is on a click.
  //
  // The POST returns as soon as the run is registered, so nothing here waits on the model inside a
  // request: the run is then followed through the status read. A cached identical run comes back
  // already finished and is rendered immediately.
  const onRun = useCallback(async () => {
    setPending(true);
    setError("");
    try {
      const started = await startAnalystRun(ticker);
      if (started?.status === RUN_RUNNING && started?.runId) {
        rememberAnalystRun(ticker, started.runId);
        setRunId(started.runId);
        watch(started.runId, ticker);
        return;
      }
      settle(started, ticker); // a cache hit: the envelope is already here
    } catch (err) {
      setPending(false);
      setError(
        err instanceof AuthRequiredError
          ? "Sign in to run an analysis."
          : "The analyst could not be reached, so no run was started — the rest of this page is unaffected."
      );
    }
  }, [ticker, watch, settle]);

  const handleToggleFollow = useCallback(async () => {
    setFollowBusy(true);
    setFollowError("");
    try {
      await toggleFollow();
    } catch (err) {
      setFollowError(
        err instanceof AuthRequiredError ? "Sign in to follow companies." : "Could not save that."
      );
    } finally {
      setFollowBusy(false);
    }
  }, [toggleFollow]);

  // The §7 object, PROJECTED to the keys a renderer may read. Every component below consumes this
  // and never the raw served object, so an audit-only field is not present to reach.
  const thesis = useMemo(() => renderableThesis(thesisFor(envelope)), [envelope]);

  // The contextual Ask for the read itself: the facts are lines ALREADY ON SCREEN, composed into a
  // VISIBLE first turn (locked decision 6). Nothing hidden, nothing fetched, nothing added.
  const askAboutRead = useMemo(() => {
    if (!onAsk) return null;
    return () => {
      const facts = [];
      if (thesis) {
        // The context turn states what is ON SCREEN, so it reads the same gate the card does
        // rather than asserting a fixed sentence that would stop being true the day Wave 5A
        // serves a verdict.
        const gate = directionGate(envelope, thesis);
        facts.push(
          gate.validated
            ? `Direction: ${thesis.direction}, validated by the ${gate.verdict.rung} ablation rung.`
            : "Direction: not yet validated — no ablation verdict exists for this horizon."
        );
        if (thesis.confidenceBucket) facts.push(`Confidence bucket: ${thesis.confidenceBucket}.`);
        if (thesis.regime) facts.push(`Regime: ${thesis.regime}.`);
        if (thesis.thesis) facts.push(`Thesis on screen: ${thesis.thesis}`);
        if (thesis.invalidation) facts.push(`Invalidation on screen: ${thesis.invalidation}`);
        for (const r of thesis.riskFlags || []) facts.push(`Risk flag: ${r}`);
      }
      onAsk({
        kind: "ticker",
        ticker,
        label: `the ${ticker} Attestel view`,
        facts,
        suggestions: [
          "Which evidence layer is weakest, and why?",
          "What would change this read?",
          "What is the strongest argument against it?",
        ],
      });
    };
  }, [onAsk, thesis, envelope, ticker]);

  const askAboutEvent = useMemo(() => {
    if (!onAsk) return null;
    return (ev) => {
      const facts = [`Event type: ${eventTypeLabel(ev?.eventType)}.`];
      if (ev?.summary) facts.push(ev.summary);
      for (const f of ev?.keyFacts || []) facts.push(f);
      const tickers = (ev?.tickers || []).map((t) => t?.ticker).filter(Boolean);
      if (tickers.length) facts.push(`Companies named: ${tickers.join(", ")}.`);
      onAsk({
        kind: "event",
        ticker,
        eventId: ev?.id || null,
        label: ev?.title || `${ticker} event`,
        facts,
      });
    };
  }, [onAsk, ticker]);

  return (
    <div className="flex flex-col gap-3">
      <IdentityRow
        ticker={ticker}
        name={data?.company || data?.name}
        quote={quote}
        data={data}
        following={following}
        onToggleFollow={handleToggleFollow}
        followBusy={followBusy}
        followError={followError}
      />

      {!eventsOnly && (
        <>
          <AttestelView
            ticker={ticker}
            envelope={envelope}
            thesis={thesis}
            onRun={onRun}
            pending={pending}
            runId={runId}
            error={error}
            onAsk={askAboutRead}
          />

          <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
            <WhatChanged items={thesis?.whatChanged} hasRun={Boolean(envelope)} />
            <Upcoming upcoming={upcoming} ticker={ticker} />
          </div>
        </>
      )}

      <LatestEvents
        events={events}
        onOpenEvent={onOpenEvent}
        onAsk={askAboutEvent}
        limit={eventsOnly ? 12 : 3}
        title={eventsOnly ? `${ticker} material events` : "Latest material events"}
      />
    </div>
  );
}
