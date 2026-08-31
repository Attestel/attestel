import { useEffect, useMemo, useRef, useState } from "react";
import { AnimatePresence, motion, useReducedMotion } from "framer-motion";
import { cx } from "../../lib/cx.js";
import {
  AuthRequiredError,
  BAND_LABELS,
  BAND_TONE,
  DIMENSION_LABELS,
  DIRECTION_LABELS,
  DIRECTION_STATUS_TONE,
  EVENT_TYPE_LABELS,
  HORIZON_LABELS,
  RELEVANCE_STEP,
  SOURCE_TIER_LABELS,
  STRENGTH_STEP,
  UNSCORED_BAND_LABEL,
  absoluteTime,
  directionTone,
  fetchEvent,
  fetchExplore,
  followTicker,
  importanceBand,
  isScored,
  listSubscriptions,
  relativeTime,
  relevanceWord,
  strengthWord,
  unfollowTicker,
} from "../../lib/eventsApi.js";
import { Badge, Button, EmptyState, IconChip, Label, SkeletonText, StatusPill } from "../ui/index.js";
import { Icon } from "../shell/icons.jsx";

// EventDetailPanel — Wave 4 Lane 4B. The panel that explains ONE canonical event without taking the
// feed off the screen (doc §16.4).
//
// Two behaviours, one component, one breakpoint:
//   * COLUMN  (>= COLUMN_BREAKPOINT_PX) — a right-hand column INSIDE the host destination's grid.
//     It is NOT a modal: no aria-modal, no focus trap, no backdrop. It is a landmark region with an
//     accessible name, and the feed beside it stays mounted, scrolled and readable. Announcing it as
//     a dialog would tell a screen-reader user the rest of the page is inert when it plainly is not.
//   * OVERLAY (<  COLUMN_BREAKPOINT_PX) — right-anchored, full height under the TopBar, backdrop,
//     aria-modal, focus trapped, Escape closes, focus returns to the invoking element. Mechanics are
//     copied from CopilotDrawer.jsx and NotificationsModal.jsx, the repo's only two over-shell
//     surfaces, down to the easing and duration — a third overlay idiom would read as foreign.
//
// The nine sections and their ORDER are doc §16.4's and are not a design choice. A section with no
// data renders an explicit "not available" state in place. It is never dropped and never reordered.
//
// What this component will not do:
//   * it never calls a model (CLAUDE.md invariant #4). `GET /api/events/{id}` is a store read
//     through the gateway (contract §9.2, §9.3); :8004 is never contacted from a browser. The
//     contextual questions in section 9 hand TEXT to the host's Ask surface and nothing else.
//   * it never prints a float. `relevance`, `potentialStrength`, `importance` and `novelty` are
//     floats in the contract and WORDS here (AD-8, doc §16.6).
//   * it never says an event moved a price. See MARKET REACTION.
//   * it never renders the model-authored audit copies of `thesis` and `invalidation`, the internal
//     direction-branch enum, the prose-suppression list or the warnings array — those are audit and
//     diagnostic records (Wave 4 addendum). `make check-web-bundle` greps the built bundle for their
//     literal names; those literals are deliberately absent from this file so a reviewer who runs
//     that grep over `web/src` instead of `web/dist` does not get a false positive off a comment.

// ── Local presentation detail ──────────────────────────────────────────────────────────────────
//
// THE 4A SEAM BLOCK THAT USED TO SIT HERE IS DELETED. It held this lane's own copies of the ten
// endpoint functions and display helpers, written to Lane 4A's quoted signatures because
// `lib/eventsApi.js` exported none of them at the time and a named import of a missing export is a
// Rollup build error. 4A has landed; every one of those symbols now comes from that module, which
// is the single definition. Six of them did NOT in fact match 4A's shipped signatures — see the
// integration handoff — and the reconciliation kept 4A's, per the merge ruling.
//
// What remains below is genuinely local: rank and tone maps that only this panel draws with, and a
// hostname formatter. None of it is contract vocabulary and none of it is a second definition of
// anything in `eventsApi.js`.

const SOURCE_TIER_TONE = { official: "ok", professional: "info", discussion: "dim" };
const SOURCE_TIER_RANK = { official: 0, professional: 1, discussion: 2 };
const HORIZON_ORDER = ["intraday", "short_term", "medium_term", "long_term"];

// hostOf renders a bare hostname for a source row. `documents[]` carries no title (see EVIDENCE),
// so the host is the only human-readable identity available for the link.
function hostOf(url) {
  try {
    return new URL(url).hostname.replace(/^www\./, "");
  } catch {
    return url || "source";
  }
}


// COLUMN_BREAKPOINT_PX — the one breakpoint, named once and read once. At or above it the panel is
// a column beside a mounted feed; below it, an overlay. Doc §16.4 fixes the desktop behaviour and
// locked decision 2 fixes the number.
export const COLUMN_BREAKPOINT_PX = 1280;

// useIsColumnWidth is the ONLY place COLUMN_BREAKPOINT_PX is read. The host destination needs the
// same answer to decide its grid template, so it calls this too rather than writing a Tailwind
// `xl:` prefix that happens to equal 1280 — two encodings of one number drift the first time
// somebody changes one of them.
export function useIsColumnWidth() {
  const [wide, setWide] = useState(() =>
    typeof window !== "undefined" && typeof window.matchMedia === "function"
      ? window.matchMedia(`(min-width: ${COLUMN_BREAKPOINT_PX}px)`).matches
      : true
  );
  useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return undefined;
    const mq = window.matchMedia(`(min-width: ${COLUMN_BREAKPOINT_PX}px)`);
    const onChange = (e) => setWide(e.matches);
    mq.addEventListener("change", onChange);
    setWide(mq.matches);
    return () => mq.removeEventListener("change", onChange);
  }, []);
  return wide;
}

// useDerivedVariant watches the viewport so the SAME component switches behaviour, rather than the
// host mounting two different ones and unmounting the feed on every resize.
function useDerivedVariant(override) {
  const wide = useIsColumnWidth();
  return override || (wide ? "column" : "overlay");
}

// ── Small shared pieces ────────────────────────────────────────────────────────────────────────

// StepMeter — the ONLY quantitative affordance in this file: three discrete steps, no number, no
// percentage, no continuous fill. AD-8 forbids a numeric confidence; a three-step qualitative meter
// is explicitly allowed. The accessible name is the WORD, never the float behind it.
function StepMeter({ step = 0, word, tone = "muted" }) {
  const TONE = { muted: "bg-muted", up: "bg-up", down: "bg-down", llm: "bg-llm", info: "bg-info" };
  return (
    <span className="inline-flex items-center gap-1.5" role="img" aria-label={word}>
      <span className="flex items-center gap-[2px]" aria-hidden="true">
        {[1, 2, 3].map((i) => (
          <span
            key={i}
            className={cx("h-2.5 w-1 rounded-[1px]", i <= step ? TONE[tone] || TONE.muted : "bg-white/12")}
          />
        ))}
      </span>
      <span className="text-[12px] text-muted">{word}</span>
    </span>
  );
}

// SectionShell — every one of the nine sections goes through this, which is what makes "a section is
// never silently dropped" structural rather than a promise. `absent` renders the stated not-available
// line in place; `interpretive` carries the llm tint and the explicit hypothesis label so a reader
// can tell a fact from an interpretation at a glance (locked decision 4, doc §15.7).
function SectionShell({ index, title, interpretive, absent, children }) {
  return (
    <section
      aria-labelledby={`evt-sec-${index}`}
      className={cx(
        "border-t border-line px-5 py-5 first:border-t-0",
        interpretive && "bg-llm/[.045]"
      )}
    >
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <Label id={`evt-sec-${index}`} as="h3" tone={interpretive ? "llm" : "dim"}>
          {title}
        </Label>
        {interpretive && (
          <Badge variant="llm" title="Model-authored interpretation, not an extracted fact">
            Interpretation
          </Badge>
        )}
      </div>
      {absent ? (
        <p className="text-[13px] leading-[1.6] text-dim">{absent}</p>
      ) : (
        children
      )}
    </section>
  );
}

// ── The panel ──────────────────────────────────────────────────────────────────────────────────

export default function EventDetailPanel({
  eventId,
  open,
  onClose,
  onOpenCompany,
  onFollow,
  onAsk,
  onAuthRequired,
  variant,
  // disclosure — contract §9.18's ANALYST_DISCLAIMER, SERVER-SUPPLIED and passed through verbatim.
  //
  // This prop is now a FALLBACK, not the primary path. `GET /api/events/{id}` used to be a purely
  // byte-verbatim proxy of a store whose `_event_json` holds no disclosure field, so the panel could
  // not obtain the string on its own and its host had to thread one in. The gateway now adds
  // `disclaimer` to that envelope as a key it owns — a verbatim proxy may ADD, never ALTER — so the
  // panel's OWN response carries the disclosure for the directions in that same response, which is
  // what §9.18 adjacency actually means. The prop remains for hosts that already hold the string
  // (Explore mirrors it onto every item) and for the component in isolation.
  //
  // When NO server string arrives from either, the per-ticker direction is WITHHELD and the
  // withholding is stated — see DIRECTION SUPPRESSION below. Composing the sentence in the browser
  // is forbidden: §9.18 makes it a server-supplied constant so that one place owns the words.
  disclosure,
}) {
  const derived = useDerivedVariant(variant);
  const isOverlay = derived === "overlay";
  const reduce = useReducedMotion();

  const panelRef = useRef(null);
  const restoreFocusRef = useRef(null);

  const [state, setState] = useState("idle"); // idle | loading | ready | notfound | error | degraded
  const [payload, setPayload] = useState(null);
  const [errMessage, setErrMessage] = useState("");
  const [nonce, setNonce] = useState(0);

  // Data: on open and on eventId change, aborted on unmount. A store read, never a model call.
  useEffect(() => {
    if (!open || !eventId) return undefined;
    let alive = true;
    setState("loading");
    setPayload(null);
    setErrMessage("");
    fetchEvent(eventId)
      .then((body) => {
        if (!alive) return;
        const degraded = Array.isArray(body?.degraded) ? body.degraded : [];
        if (!body?.event && degraded.length) {
          setPayload({ degraded });
          setState("degraded");
          return;
        }
        if (!body?.event) {
          setState("notfound");
          return;
        }
        setPayload(body);
        setState("ready");
      })
      .catch((err) => {
        if (!alive) return;
        if (err instanceof AuthRequiredError) {
          onAuthRequired?.();
          setErrMessage("Sign in to read this event.");
          setState("error");
          return;
        }
        if (err?.status === 404) {
          setState("notfound");
          return;
        }
        setErrMessage(err?.message || "The event could not be loaded.");
        setState("error");
      });
    return () => {
      alive = false;
    };
  }, [open, eventId, nonce, onAuthRequired]);

  // Overlay mechanics ONLY. In column variant the panel is part of the page: locking body scroll,
  // trapping Tab or restoring focus on close would each be wrong there, because nothing was ever
  // taken away from the user.
  useEffect(() => {
    if (!open || !isOverlay) return undefined;
    restoreFocusRef.current = document.activeElement;
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    const t = setTimeout(() => panelRef.current?.focus?.(), 40);
    return () => {
      clearTimeout(t);
      document.body.style.overflow = prevOverflow;
      restoreFocusRef.current?.focus?.();
    };
  }, [open, isOverlay]);

  // Escape + focus trap, matching CopilotDrawer.jsx:375-393 exactly.
  const onKeyDown = (e) => {
    if (!isOverlay) return;
    if (e.key === "Escape") {
      e.stopPropagation();
      onClose?.();
      return;
    }
    if (e.key !== "Tab") return;
    const focusables = panelRef.current?.querySelectorAll(
      'button, [href], input, textarea, select, [tabindex]:not([tabindex="-1"])'
    );
    if (!focusables || focusables.length === 0) return;
    const list = Array.from(focusables).filter((el) => !el.disabled && el.offsetParent !== null);
    if (list.length === 0) return;
    const first = list[0];
    const last = list[list.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  };

  if (!eventId) return null;

  const event = payload?.event || null;
  // The SERVED band word (§AD-8) plus the shared presentation maps. `isScored` keeps "never
  // scored" distinct from "scored lowest" — the degraded cascade omits `importance` entirely.
  //
  // THE BAND ARRIVES ON THE ENVELOPE, NOT ON THE EVENT, and only here. `GET /api/events/{id}` is a
  // verbatim proxy of a store that computes no bands, so the gateway adds `importanceBand` as a
  // TOP-LEVEL key it owns (§9.59) rather than reaching inside the store's `event` object. Feed items
  // carry it on the item itself. Reading only `event.importanceBand` is what made this panel render
  // "Routine" beside a feed card reading "High impact" for the same event.
  const banded = payload?.importanceBand
    ? { ...event, importanceBand: payload.importanceBand }
    : event;
  const band = importanceBand(banded);
  const bandScored = isScored(event);

  const body = (
    <>
      <PanelHeader
        event={event}
        band={band}
        bandScored={bandScored}
        state={state}
        isOverlay={isOverlay}
        onClose={onClose}
      />
      <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain">
        {state === "loading" && <LoadingBody />}
        {state === "error" && (
          <div className="p-5">
            <EmptyState icon={<Icon name="warning" size={22} />} tone="warn" title="Could not load this event"
              action={<Button size="sm" onClick={() => setNonce((n) => n + 1)}>Try again</Button>}>
              {errMessage}
            </EmptyState>
          </div>
        )}
        {state === "notfound" && (
          <div className="p-5">
            <EmptyState icon={<Icon name="info" size={22} />} title="Event not found">
              There is no event with this id at the current cutoff. It may have been published after
              the point-in-time cutoff you are reading at.
            </EmptyState>
          </div>
        )}
        {state === "degraded" && (
          <div className="p-5">
            <EmptyState icon={<Icon name="warning" size={22} />} tone="warn" title="Event service unavailable"
              action={<Button size="sm" onClick={() => setNonce((n) => n + 1)}>Try again</Button>}>
              The gateway is up; the event store is not ({(payload?.degraded || []).join(", ")}).
              Nothing is shown here rather than a reconstruction of what the event might have said.
            </EmptyState>
          </div>
        )}
        {state === "ready" && event && (
          <EventBody
            event={event}
            documents={payload?.documents || []}
            tickers={payload?.tickers || event.tickers || []}
            asOf={payload?.asOf}
            disclosure={payload?.disclaimer || event?.disclaimer || disclosure || ""}
            onOpenCompany={onOpenCompany}
            onFollow={onFollow}
            onAsk={onAsk}
            onAuthRequired={onAuthRequired}
          />
        )}
      </div>
    </>
  );

  // COLUMN — a landmark region inside the host's grid. Not a dialog, so it carries no aria-modal
  // and traps nothing; the feed beside it keeps its focus order and its scroll position.
  if (!isOverlay) {
    return (
      <AnimatePresence initial={false}>
        {open && (
          <motion.aside
            key="evt-column"
            ref={panelRef}
            role="region"
            aria-label={event?.title ? `Event detail: ${event.title}` : "Event detail"}
            initial={reduce ? { opacity: 0 } : { opacity: 0, x: 16 }}
            animate={reduce ? { opacity: 1 } : { opacity: 1, x: 0 }}
            exit={reduce ? { opacity: 0 } : { opacity: 0, x: 16 }}
            transition={{ duration: 0.22, ease: [0.16, 0.8, 0.24, 1] }}
            className="surface-deep sticky top-[72px] flex max-h-[calc(100vh-88px)] min-w-[420px] flex-col overflow-hidden"
          >
            {body}
          </motion.aside>
        )}
      </AnimatePresence>
    );
  }

  // OVERLAY — the CopilotDrawer / NotificationsModal idiom, anchored below the sticky TopBar (h-14,
  // z-40) so the shell's provenance chrome is never covered by this panel.
  return (
    <AnimatePresence>
      {open && (
        <div className="fixed inset-x-0 bottom-0 top-14 z-50" role="presentation">
          <motion.div
            className="absolute inset-0 bg-black/60 backdrop-blur-[2px]"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            transition={{ duration: 0.2 }}
            onClick={onClose}
            aria-hidden="true"
          />
          <motion.aside
            ref={panelRef}
            role="dialog"
            aria-modal="true"
            aria-label={event?.title ? `Event detail: ${event.title}` : "Event detail"}
            tabIndex={-1}
            onKeyDown={onKeyDown}
            initial={reduce ? { opacity: 0 } : { x: "100%" }}
            animate={reduce ? { opacity: 1 } : { x: 0 }}
            exit={reduce ? { opacity: 0 } : { x: "100%" }}
            transition={{ duration: 0.22, ease: [0.16, 0.8, 0.24, 1] }}
            className="surface-deep absolute right-0 top-0 flex h-full w-full flex-col outline-none !rounded-none sm:w-[min(560px,100vw-2rem)] sm:!rounded-l-[30px]"
          >
            {body}
          </motion.aside>
        </div>
      )}
    </AnimatePresence>
  );
}

// ── Header ─────────────────────────────────────────────────────────────────────────────────────
// Fixed while the body scrolls. Title truncates to two lines and keeps the full text available
// through `title=` so nothing is lost.

function PanelHeader({ event, band, bandScored, state, isOverlay, onClose }) {
  return (
    <header className="flex flex-none items-start gap-3 border-b border-line bg-panel/60 px-5 py-4">
      <div className="min-w-0 flex-1">
        <div className="mb-2 flex flex-wrap items-center gap-2">
          <Label tone="dim">Event detail</Label>
          {state === "ready" && (
            <StatusPill
              tone={bandScored ? BAND_TONE[band] : "neutral"}
              title="Deterministic importance band (contract §3.6), computed and served by the gateway"
            >
              {bandScored ? BAND_LABELS[band] : UNSCORED_BAND_LABEL}
            </StatusPill>
          )}
        </div>
        <h2
          title={event?.title || ""}
          className="m-0 line-clamp-2 text-[17px] font-[550] leading-[1.25] tracking-[-0.02em] text-fg"
        >
          {event?.title || (state === "loading" ? "Loading event…" : "Event")}
        </h2>
      </div>
      <button
        type="button"
        onClick={onClose}
        aria-label={isOverlay ? "Close event detail" : "Close event detail column"}
        className="mt-0.5 flex h-8 w-8 flex-none items-center justify-center rounded-full text-muted transition-colors hover:bg-white/10 hover:text-fg"
      >
        <Icon name="close" size={16} />
      </button>
    </header>
  );
}

function LoadingBody() {
  return (
    <div className="flex flex-col gap-6 p-5">
      <SkeletonText lines={2} />
      <SkeletonText lines={4} />
      <SkeletonText lines={3} />
    </div>
  );
}

// ── The nine sections, in doc §16.4's order ────────────────────────────────────────────────────

function EventBody({ event, documents, tickers, asOf, disclosure, onOpenCompany, onFollow, onAsk, onAuthRequired }) {
  const isRaw = event.enrichmentState !== "enriched";
  const rows = Array.isArray(tickers) && tickers.length ? tickers : event.tickers || [];

  return (
    <div>
      <IdentitySection event={event} />
      <TickersSection
        rows={rows}
        disclosure={disclosure}
        onOpenCompany={onOpenCompany}
        onFollow={onFollow}
        onAuthRequired={onAuthRequired}
      />
      <WhatHappenedSection event={event} />
      <WhyItMattersSection event={event} isRaw={isRaw} />
      <WhatChangedSection />
      <HorizonSection rows={rows} isRaw={isRaw} />
      <MarketReactionSection event={event} />
      <EvidenceSection documents={documents} event={event} asOf={asOf} disclosure={disclosure} />
      <AskSection event={event} rows={rows} onAsk={onAsk} />
    </div>
  );
}

// 1 — Event identity and confirmation. Not-confirmed is STATED, never omitted: the absence of an
// official filing is information, and leaving the row out would read as "confirmed".
function IdentitySection({ event }) {
  const typeLabel = EVENT_TYPE_LABELS[event.eventType] || event.eventType || "Uncategorised";
  const tier = event.sourceTier;
  const confirmed = event.officialConfirmed === true;
  return (
    <SectionShell index={1} title="Identity and confirmation">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="outline">{typeLabel}</Badge>
        {tier && (
          <Badge variant={SOURCE_TIER_TONE[tier] || "dim"}>
            {SOURCE_TIER_LABELS[tier] || tier} source
          </Badge>
        )}
      </div>

      <div className="mt-4 flex items-start gap-3 rounded-xl border border-line bg-panel2/60 p-3">
        <IconChip kind={confirmed ? "filing" : "neutral"} size="sm">
          <Icon name={confirmed ? "filing" : "info"} size={16} />
        </IconChip>
        <div className="min-w-0">
          <div className="text-[13px] font-[550] text-fg">
            {confirmed ? "Confirmed by an official filing" : "Not confirmed by an official filing"}
          </div>
          <p className="mt-1 text-[12.5px] leading-[1.55] text-muted">
            {confirmed
              ? "At least one supporting document is an official filing or a primary-source release."
              : "No official filing supports this event yet. It rests on secondary reporting."}
          </p>
        </div>
      </div>

      {/* occurredAt and publishedAt both, always. They differ, and the gap between them is how
          late the market learned — a fact no single timestamp can carry. */}
      <dl className="mt-4 grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div>
          <Label as="dt">Occurred</Label>
          <dd className="num mt-1 text-[12.5px] text-fg">{absoluteTime(event.occurredAt)}</dd>
        </div>
        <div>
          <Label as="dt">Published</Label>
          <dd className="num mt-1 text-[12.5px] text-fg">
            {absoluteTime(event.publishedAt)}
            {relativeTime(event.publishedAt) && (
              <span className="ml-2 font-sans text-muted">({relativeTime(event.publishedAt)})</span>
            )}
          </dd>
        </div>
      </dl>
    </SectionShell>
  );
}

// 2 — Affected tickers. The `transmission` sentence is the single most valuable line in the panel
// and gets its own room.
//
// DIRECTION SUPPRESSION (contract §9.18). A direction is model-influenced output and its disclosure
// string must stay ADJACENT to it (docs/DISCLOSURE_CLASSIFICATION.md rule 3). The string is a
// SERVER constant; the browser may not compose one. `GET /api/events/{id}` does not send it today,
// so when no server string reached this panel the direction is withheld and the withdrawal is
// stated. A visible withdrawal is the point — it makes the gap countable instead of silent, which
// is the posture §9.58 settled for prose citing an unserved level.
function TickersSection({ rows, disclosure, onOpenCompany, onFollow, onAuthRequired }) {
  const canShowDirection = Boolean(disclosure);
  const anyDirection = rows.some((t) => t.potentialDirection);

  return (
    <SectionShell
      index={2}
      title="Affected companies"
      absent={rows.length ? null : "No company is linked to this event in the store."}
    >
      <ul className="flex flex-col gap-3">
        {rows.map((t) => (
          <TickerRow
            key={t.ticker}
            row={t}
            canShowDirection={canShowDirection}
            onOpenCompany={onOpenCompany}
            onFollow={onFollow}
            onAuthRequired={onAuthRequired}
          />
        ))}
      </ul>

      {rows.length > 0 && anyDirection && !canShowDirection && (
        <p className="mt-3 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 text-[12.5px] leading-[1.55] text-warn">
          Potential direction is withheld here. The server did not send the disclosure string that
          contract §9.18 requires beside it, and this app will not write one on the server&rsquo;s
          behalf.
        </p>
      )}

      {rows.length > 0 && canShowDirection && (
        <p className="mt-3 text-[12px] leading-[1.55] text-dim">{disclosure}</p>
      )}
    </SectionShell>
  );
}

function TickerRow({ row, canShowDirection, onOpenCompany, onFollow, onAuthRequired }) {
  const rel = relevanceWord(row.relevance);
  const strength = strengthWord(row.potentialStrength);
  const tone = directionTone(row.potentialDirection);
  const dims = Array.isArray(row.affectedDimensions) ? row.affectedDimensions : [];

  return (
    <li className="rounded-xl border border-line bg-panel2/50 p-3.5">
      <div className="flex flex-wrap items-center gap-2">
        <span className="num text-[14px] font-semibold tracking-[-0.01em] text-fg">{row.ticker}</span>
        {row.isPrimary && (
          <Badge variant="accent" title="The event is about this company">Subject</Badge>
        )}
        {rel && <StepMeter step={RELEVANCE_STEP[rel]} word={`${rel} relevance`} tone="info" />}
        <div className="ml-auto flex items-center gap-2">
          <FollowButton ticker={row.ticker} onFollow={onFollow} onAuthRequired={onAuthRequired} />
          <Button
            size="xs"
            variant="ghost"
            onClick={() => onOpenCompany?.(row.ticker)}
            aria-label={`Open the ${row.ticker} company page`}
          >
            Open company
          </Button>
        </div>
      </div>

      {(canShowDirection && row.potentialDirection) || row.expectedHorizon || strength ? (
        <div className="mt-2.5 flex flex-wrap items-center gap-2.5">
          {canShowDirection && row.potentialDirection && (
            <StatusPill tone={DIRECTION_STATUS_TONE[tone]} title="Analytical hypothesis, not a recommendation">
              {DIRECTION_LABELS[row.potentialDirection] || row.potentialDirection}
            </StatusPill>
          )}
          {canShowDirection && strength && (
            <StepMeter
              step={STRENGTH_STEP[strength]}
              word={`${strength} — hypothesis`}
              tone={tone === "up" ? "up" : tone === "down" ? "down" : "llm"}
            />
          )}
          {row.expectedHorizon && (
            <span className="text-[12px] text-muted">
              {HORIZON_LABELS[row.expectedHorizon] || row.expectedHorizon}
            </span>
          )}
        </div>
      ) : null}

      {dims.length > 0 && (
        <div className="mt-2.5 flex flex-wrap gap-1.5">
          {dims.map((d) => (
            <Badge key={d} variant="dim">{DIMENSION_LABELS[d] || d}</Badge>
          ))}
        </div>
      )}

      {row.transmission ? (
        <blockquote className="mt-3 border-l-2 border-llm/50 pl-3">
          <Label tone="llm" className="mb-1">How it reaches {row.ticker}</Label>
          <p className="m-0 text-[13.5px] leading-[1.65] text-fg">{row.transmission}</p>
        </blockquote>
      ) : (
        <p className="mt-3 text-[12.5px] leading-[1.55] text-dim">
          No transmission sentence has been written for {row.ticker}. That line is enrichment output,
          and enrichment has not run for this event.
        </p>
      )}
    </li>
  );
}

// FollowButton — one click, three states, undo, and the card never moves under the cursor
// (doc §16.8). It calls the host's `onFollow` when given one and falls back to the seam's
// `followTicker` so Explore and the panel both work without host wiring.
function FollowButton({ ticker, onFollow, onAuthRequired, followed: initiallyFollowed = false, size = "xs" }) {
  const [status, setStatus] = useState(initiallyFollowed ? "done" : "idle"); // idle|pending|done|error
  const [subId, setSubId] = useState("");
  const [message, setMessage] = useState("");

  useEffect(() => {
    setStatus(initiallyFollowed ? "done" : "idle");
    setSubId("");
    setMessage("");
  }, [ticker, initiallyFollowed]);

  const follow = async () => {
    setStatus("pending");
    setMessage("");
    try {
      const sub = onFollow ? await onFollow(ticker) : await followTicker(ticker, { source: "explore" });
      setSubId(sub?.id || "");
      setStatus("done");
    } catch (err) {
      if (err instanceof AuthRequiredError) {
        setStatus("idle");
        onAuthRequired?.();
        return;
      }
      setMessage(err?.message || "Follow failed.");
      setStatus("error");
    }
  };

  const undo = async () => {
    if (!subId) {
      setStatus("idle");
      return;
    }
    setStatus("pending");
    try {
      await unfollowTicker(subId);
      setSubId("");
      setStatus("idle");
    } catch (err) {
      if (err instanceof AuthRequiredError) {
        onAuthRequired?.();
        setStatus("done");
        return;
      }
      setMessage(err?.message || "Undo failed.");
      setStatus("error");
    }
  };

  if (status === "done") {
    return (
      <span className="inline-flex items-center gap-2">
        <span className="inline-flex items-center gap-1.5 rounded-full border border-up/50 px-2.5 py-[4px] font-mono text-[9px] uppercase leading-none tracking-[.12em] text-up">
          <Icon name="check" size={10} /> Following
        </span>
        <Button size="xs" variant="ghost" onClick={undo} aria-label={`Stop following ${ticker}`}>
          Undo
        </Button>
      </span>
    );
  }

  return (
    <span className="inline-flex items-center gap-2">
      <Button
        size={size}
        variant="secondary"
        onClick={follow}
        disabled={status === "pending"}
        aria-label={`Follow ${ticker}`}
      >
        {status === "pending" ? "Following…" : `+ Follow ${ticker}`}
      </Button>
      {status === "error" && (
        <span className="text-[11.5px] text-down" role="status">{message}</span>
      )}
    </span>
  );
}

// 3 — What happened. FACTS. No tint, no interpretive label, no llm wash: the visual difference
// between this section and section 4 is the whole point of storing them in separate columns.
function WhatHappenedSection({ event }) {
  const facts = Array.isArray(event.keyFacts) ? event.keyFacts.filter(Boolean) : [];
  const hasBody = Boolean(event.summary) || facts.length > 0;
  return (
    <SectionShell
      index={3}
      title="What happened"
      absent={hasBody ? null : "The store holds no summary or extracted facts for this event."}
    >
      {event.summary && (
        <p className="m-0 text-[13.5px] leading-[1.7] text-fg">{event.summary}</p>
      )}
      {facts.length > 0 && (
        <>
          <Label className="mb-2 mt-4">Key facts</Label>
          <ul className="flex flex-col gap-2">
            {facts.map((f, i) => (
              <li key={i} className="flex gap-2.5 text-[13px] leading-[1.6] text-fg">
                <span className="mt-[7px] h-1 w-1 flex-none rounded-full bg-muted" aria-hidden="true" />
                <span>{f}</span>
              </li>
            ))}
          </ul>
        </>
      )}
      {event.summary && facts.length === 0 && (
        <p className="mt-3 text-[12.5px] text-dim">
          No key facts have been extracted. Fact extraction is enrichment output.
        </p>
      )}
    </SectionShell>
  );
}

// 4 — Why it matters. A HYPOTHESIS. `raw` means the event was never analysed, and that is said
// outright rather than shown as an empty box (locked decision 4).
function WhyItMattersSection({ event, isRaw }) {
  if (isRaw || !event.whyItMatters) {
    return (
      <SectionShell
        index={4}
        title="Why it matters"
        absent={
          isRaw
            ? "This event has not been analysed yet. Enrichment is off by default, so there is no interpretation to show — not an empty one, none."
            : "This event was analysed, but no interpretation was recorded for it."
        }
      />
    );
  }
  return (
    <SectionShell index={4} title="Why it matters" interpretive>
      <p className="m-0 text-[13.5px] leading-[1.7] text-fg">{event.whyItMatters}</p>
      <p className="mt-3 text-[12px] leading-[1.55] text-dim">
        Model-authored interpretation of a stored fact set. It is stored separately from the facts
        above so a later backtest can tell evidence from reading.
      </p>
    </SectionShell>
  );
}

// 5 — What changed. `GET /api/changed` is contract §9.1's route and belongs to Wave 5 Lane 5B
// alone; it is registered NOWHERE in the gateway today (gateway/feeds.go:15 says so explicitly).
// So there is nothing to filter to this event, and the section says that rather than leaving a gap
// or inventing a change set.
function WhatChangedSection() {
  return (
    <SectionShell
      index={5}
      // See the heading note in OverviewSection: this section is THESIS-scoped and says so.
      title="Recorded thesis changes"
      absent="No recorded thesis change for this event. The change feed that would answer this is not built yet, so nothing is claimed here."
    />
  );
}

// 6 — Potential impact by horizon. Hypotheses, grouped by §3.8's horizon enum, words only.
function HorizonSection({ rows, isRaw }) {
  const grouped = useMemo(() => {
    const out = new Map();
    for (const t of rows) {
      if (!t.expectedHorizon) continue;
      const list = out.get(t.expectedHorizon) || [];
      list.push(t);
      out.set(t.expectedHorizon, list);
    }
    return out;
  }, [rows]);

  const present = HORIZON_ORDER.filter((h) => grouped.has(h));
  if (present.length === 0) {
    return (
      <SectionShell
        index={6}
        title="Potential impact by horizon"
        absent={
          isRaw
            ? "No horizon has been assigned. Horizon, direction and strength are all enrichment output, and enrichment has not run for this event."
            : "No horizon was assigned to any affected company."
        }
      />
    );
  }

  return (
    <SectionShell index={6} title="Potential impact by horizon" interpretive>
      <div className="flex flex-col gap-3.5">
        {present.map((h) => (
          <div key={h}>
            <Label tone="llm" className="mb-1.5">{HORIZON_LABELS[h]}</Label>
            <ul className="flex flex-col gap-1.5">
              {grouped.get(h).map((t) => {
                const strength = strengthWord(t.potentialStrength);
                return (
                  <li key={t.ticker} className="flex flex-wrap items-center gap-2 text-[13px] text-fg">
                    <span className="num font-semibold">{t.ticker}</span>
                    <span className="text-muted">
                      {DIRECTION_LABELS[t.potentialDirection] || "Direction not assigned"}
                    </span>
                    {strength && <StepMeter step={STRENGTH_STEP[strength]} word={strength} tone="llm" />}
                  </li>
                );
              })}
            </ul>
          </div>
        ))}
      </div>
      <p className="mt-3.5 text-[12px] leading-[1.55] text-dim">
        A hypothesis about what could follow, not a forecast and not a target. Nothing here is a
        recommendation to act.
      </p>
    </SectionShell>
  );
}

// 7 — Market reaction. Locked decision 5: the price may be shown, the attribution may not.
//
// No price series reaches this panel. `GET /api/events/{id}` is a proxy of the event store, which
// holds no prices, and the composite dashboard endpoint that does hold them triggers an LLM read on
// the gateway — calling it from a panel open would violate CLAUDE.md invariant #4. So this section
// states the honest thing. The handoff records the server-side change that would fill it.
function MarketReactionSection({ event }) {
  const symbols = (event.tickers || []).map((t) => t.ticker).filter(Boolean);
  const naming = symbols.length ? symbols.join(", ") : "the affected companies";
  return (
    <SectionShell
      index={7}
      title="Market reaction"
      absent={`Not enough data to attribute a move. No price series measured against this event's publication time is served with it, so nothing is said about how ${naming} traded. Attestel does not attribute a move it has not measured.`}
    />
  );
}

// 8 — Evidence and source list. Official tier first — a filing and a forum post must not look
// alike (invariant 5). A dense data list: row heights and type sizes stay tight (density rule).
//
// D-29: professional-tier documents are stored as a bounded excerpt plus a link and are never
// re-fetched in-app. What §3.7 / §9.6 actually store per link is `document_id, url, provider,
// source_tier, published_at` — there is NO title and NO excerpt column on `event_documents`, and
// `_sources_json` (services/events/app/events.py:709) returns exactly those five fields. So this
// list renders what is served and links out; it does not invent a headline for a row.
function EvidenceSection({ documents, event, asOf, disclosure }) {
  const rows = useMemo(() => {
    const list = (documents && documents.length ? documents : event.sources || []).slice();
    list.sort((a, b) => {
      const ra = SOURCE_TIER_RANK[a.sourceTier] ?? 9;
      const rb = SOURCE_TIER_RANK[b.sourceTier] ?? 9;
      if (ra !== rb) return ra - rb;
      return String(b.publishedAt || "").localeCompare(String(a.publishedAt || ""));
    });
    return list;
  }, [documents, event.sources]);

  const audit = event.audit || null;

  return (
    <SectionShell
      index={8}
      title="Evidence and sources"
      absent={rows.length ? null : "No supporting documents are linked to this event."}
    >
      {rows.length > 0 && (
        <ul className="flex flex-col divide-y divide-line">
          {rows.map((d) => (
            <li key={d.documentId || d.url} className="flex items-start gap-3 py-2.5 first:pt-0">
              <Badge variant={SOURCE_TIER_TONE[d.sourceTier] || "dim"} className="mt-[3px] flex-none">
                {SOURCE_TIER_LABELS[d.sourceTier] || d.sourceTier || "unknown"}
              </Badge>
              <div className="min-w-0 flex-1">
                <a
                  href={d.url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="inline-flex items-center gap-1.5 text-[13px] leading-[1.45] text-fg underline decoration-line2 underline-offset-2 transition-colors hover:text-accent"
                >
                  <span className="truncate">{hostOf(d.url)}</span>
                  <Icon name="reply" size={11} className="flex-none -scale-x-100" />
                  <span className="sr-only">(opens in a new tab)</span>
                </a>
                <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11.5px] text-dim">
                  <span>{d.provider}</span>
                  <span aria-hidden="true">·</span>
                  <span className="num">{absoluteTime(d.publishedAt)}</span>
                </div>
              </div>
            </li>
          ))}
        </ul>
      )}

      {rows.length > 0 && (
        <p className="mt-3 text-[12px] leading-[1.55] text-dim">
          Source titles and excerpts are not served with an event&rsquo;s link list, so each row is
          identified by its host and provider. Documents open at the publisher; nothing is fetched
          or expanded in the app (D-29).
        </p>
      )}

      {/* Provenance footer — sections 2, 4 and 6 are model-influenced and the reader is entitled to
          know by whom. The disclosure string (§9.18) is carried here as well as beside the
          direction chips, because the footer is what a reader scrolls to when they ask "who wrote
          this". Both copies are the server's string, verbatim. */}
      <div className="mt-4 rounded-xl border border-line bg-panel2/40 p-3">
        <Label className="mb-2">Provenance</Label>
        {audit ? (
          <dl className="grid grid-cols-2 gap-x-4 gap-y-1.5 text-[11.5px]">
            <dt className="text-dim">Model</dt>
            <dd className="num text-muted">{audit.modelUsed || "not recorded"}</dd>
            <dt className="text-dim">Prompt</dt>
            <dd className="num text-muted">{audit.promptVersion || "not recorded"}</dd>
            <dt className="text-dim">Reasoning mode</dt>
            <dd className="num text-muted">{audit.reasoningMode || "not recorded"}</dd>
            <dt className="text-dim">Enriched at</dt>
            <dd className="num text-muted">{audit.enrichedAt ? absoluteTime(audit.enrichedAt) : "not recorded"}</dd>
          </dl>
        ) : (
          <p className="m-0 text-[12px] leading-[1.55] text-muted">
            No model has touched this event. Sections 2, 4 and 6 are empty for that reason, not
            because the data is missing.
          </p>
        )}
        {disclosure && (
          <p className="mt-2.5 border-t border-line pt-2.5 text-[12px] leading-[1.55] text-dim">
            {disclosure}
          </p>
        )}
        {asOf && (
          <p className="num mt-2 text-[11px] text-dim">Read at cutoff {absoluteTime(asOf)}</p>
        )}
      </div>
    </SectionShell>
  );
}

// 9 — Contextual Attestel questions (doc §16.12). Three buttons that hand TEXT to the host's Ask
// surface. They call no model themselves; a page that opened a panel and silently generated three
// answers would be invariant #4 broken in the most expensive way possible.
function AskSection({ event, rows, onAsk }) {
  const symbols = rows.map((t) => t.ticker).filter(Boolean);
  const primary = rows.find((t) => t.isPrimary)?.ticker || symbols[0] || "";
  const second = symbols.find((s) => s !== primary) || "";

  const questions = [
    { key: "why", ticker: primary, text: `Why does "${event.title}" matter?` },
    second
      ? { key: "compare", ticker: primary, text: `Compare the impact on ${primary} and ${second}.` }
      : {
          key: "compare",
          ticker: primary,
          text: primary
            ? `Which other companies could this reach besides ${primary}?`
            : "Which companies could this reach?",
        },
    { key: "invalidate", ticker: primary, text: "Does this invalidate the thesis?" },
  ];

  if (!onAsk) {
    return (
      <SectionShell
        index={9}
        title="Ask Attestel"
        absent="The Ask surface is not wired into this screen yet, so these questions have nowhere to go."
      />
    );
  }

  return (
    <SectionShell index={9} title="Ask Attestel">
      <div className="flex flex-col gap-2">
        {questions.map((q) => (
          <Button
            key={q.key}
            size="sm"
            variant="secondary"
            className="justify-start text-left"
            onClick={() => onAsk({ eventId: event.id, ticker: q.ticker, question: q.text })}
          >
            <Icon name="copilot" size={14} className="flex-none text-llm" />
            <span className="min-w-0 truncate">{q.text}</span>
          </Button>
        ))}
      </div>
      <p className="mt-3 text-[12px] leading-[1.55] text-dim">
        These hand the question to Ask Attestel. Nothing is asked until you press one.
      </p>
    </SectionShell>
  );
}

export { FollowButton, StepMeter, SectionShell };
