import { useEffect, useRef, useState } from "react";
import { Label, Panel, Skeleton } from "../ui/index.js";
import { cx } from "../../lib/cx.js";
import { changedUnavailable, fetchChanged, relativeTime } from "../../lib/eventsApi.js";
import { readLastCheck, recordLastCheck, resolveLastCheck } from "./lastCheck.js";
import { DegradedNotice } from "./EventListStates.jsx";

// WhatChangedPanel — doc §16.5 / §15.12, "what changed since your last check" as a FIRST-CLASS
// OBJECT rather than a summary strip.
//
// THE TYPOGRAPHIC INVERSION IS THE FEATURE, and it is the whole product claim in one block:
//
//     4  material events detected          ← heading scale. What Attestel concluded.
//     2  followed companies changed        ← heading scale.
//     87 source documents processed        ← mono micro-label, dim. What it read to get there.
//
// "87 new articles" creates work for the user. "4 material events, 2 companies changed" says the
// work is already done. If a reviewer's eye lands on 87 first, this block has failed and must be
// rebuilt — that is doc §16.5's own test, not a preference.

// ChangeRow — one subject and what happened to it. MACRO is a legitimate subject alongside a
// ticker (doc §15.8): a Fed communication that moves rate expectations changed the user's companies
// whether or not it named one.
//
// SUBJECT and BEARING were read defensively across two vocabularies because, at Wave 4, `GET
// /api/changed` did not exist and its field names were not observable from the worktree.
//
// WAVE 5B LANDED THE ROUTE, AND THE GUESS WAS RIGHT. Measured against the real payload
// (`gateway/changed_test.go::TestPayloadMatchesWhatTheFrontendReadsFirst`): items are
// `changes.go`'s `ChangeItem`, and the FIRST spelling in each pair — `researchLink.ticker` and
// `bearing` — is what arrives. The `ticker` / `subject` / `direction` fallbacks are now DEAD
// SPELLINGS: nothing serves them, and that gateway test asserts nothing ever will, so the two
// vocabularies cannot drift apart.
//
// They are kept rather than deleted for one reason: `MACRO` is a legitimate subject on this feed
// (doc §15.8), and `subjectOf`'s final fallback is what renders it. The middle two cost nothing and
// removing them would only make this file's history harder to read.
const BEARING_TONE = {
  strengthens: "text-up",
  supports: "text-up",
  positive: "text-up",
  weakens: "text-down",
  contradicts: "text-down",
  negative: "text-down",
  updates: "text-warn",
  neutral: "text-muted",
};

function subjectOf(item) {
  return item?.researchLink?.ticker || item?.ticker || item?.subject || "MACRO";
}

function bearingOf(item) {
  return item?.bearing || item?.direction || "";
}

function ChangeRow({ item, onOpenEvent, onOpenCompany }) {
  const subject = subjectOf(item);
  const bearing = bearingOf(item);
  const text = item?.summary || item?.reason || "";
  const activate = () => {
    // An event id opens the driving event; a company rollup opens the company. Two different
    // destinations, chosen by what the row actually is.
    if (item?.eventId && onOpenEvent) onOpenEvent(item.eventId);
    else if (subject && subject !== "MACRO" && onOpenCompany) onOpenCompany(subject);
  };
  const interactive = Boolean((item?.eventId && onOpenEvent) || (subject !== "MACRO" && onOpenCompany));
  return (
    <div
      role={interactive ? "button" : undefined}
      tabIndex={interactive ? 0 : undefined}
      onClick={interactive ? activate : undefined}
      onKeyDown={
        interactive
          ? (e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                activate();
              }
            }
          : undefined
      }
      className={cx(
        "flex flex-wrap items-baseline gap-x-2.5 gap-y-0.5 rounded-lg px-2 py-1.5 -mx-2",
        interactive && "cursor-pointer transition-colors hover:bg-panel2"
      )}
    >
      <span className="w-[62px] flex-none font-mono text-[11px] uppercase tracking-[.1em] text-fg">
        {subject}
      </span>
      {bearing && (
        <span
          className={cx(
            "flex-none font-mono text-[10px] uppercase tracking-[.12em]",
            BEARING_TONE[bearing] || "text-muted"
          )}
        >
          → {bearing}
        </span>
      )}
      <span className="min-w-0 flex-1 text-[13px] leading-[1.55] text-muted">{text}</span>
    </div>
  );
}

// Metric — the large half of the inversion. `value` is a COUNT OF CHANGES, never a count of
// articles, and never a score: nothing in this block implies a calibrated probability.
function Metric({ value, label }) {
  return (
    <div className="flex items-baseline gap-2.5">
      <span className="text-[26px] font-[550] leading-[1.04] tracking-[-0.03em] text-fg tabular-nums">
        {value}
      </span>
      <span className="text-[13.5px] leading-[1.4] text-muted">{label}</span>
    </div>
  );
}

export default function WhatChangedPanel({
  uid,
  onOpenEvent,
  onOpenCompany,
  markViewed = true,
  className,
}) {
  // `since` is read ONCE, on mount, before the visit is recorded — otherwise the surface would ask
  // "what changed since now", which is always "nothing".
  //
  // WAVE 5B: the boundary is now SERVER-FIRST (`resolveLastCheck`), so a returning user on a second
  // device gets their real reading position instead of silently falling back to the 24-hour window.
  // The local value seeds the first frame — a surface has to render before any fetch resolves, and
  // the local number is the honest thing to show meanwhile.
  const [since, setSince] = useState(() => readLastCheck(uid));
  const [state, setState] = useState({ status: "loading" });
  const recorded = useRef(false);

  useEffect(() => {
    let alive = true;
    setState({ status: "loading" });
    resolveLastCheck(uid)
      .then(({ at }) => {
        if (!alive) return at;
        setSince(at);
        return at;
      })
      .then((at) => fetchChanged({ since: at }))
      .then((data) => alive && setState({ status: "ready", data }))
      .catch((err) =>
        alive &&
        setState({
          // A 404 here USED to be the route not existing yet (§9.1). Wave 5B landed
          // `GET /api/changed`, so a 404 now means something is genuinely wrong with the
          // deployment — but the state is KEPT, and kept honest, because a surface that renders an
          // outage as calm reassurance makes a false statement about the user's companies. That
          // was true when the route was missing and it is true now.
          status: changedUnavailable(err) ? "unavailable" : "error",
          error: err,
        })
      );
    return () => {
      alive = false;
    };
  }, [uid]);

  // The visit is recorded only once the surface has actually rendered an answer — never on the
  // parent's mount, never while still loading. `recordLastCheck` stamps BOTH tiers: local first so
  // the boundary survives a failed request, then the server, whose store is monotonic.
  useEffect(() => {
    if (!markViewed || recorded.current) return;
    if (state.status !== "ready" && state.status !== "unavailable") return;
    recorded.current = true;
    void recordLastCheck(uid);
  }, [state.status, markViewed, uid]);

  // THE COPY RULE (locked decision 6): a default 24-hour window is never labelled "since your last
  // check". The SERVER now states which it served — `since.basis` is `"requested"` or
  // `"default24h"` — so the heading is chosen from what was actually applied rather than from what
  // the client believed it sent. When the payload has not arrived yet, the client's own boundary
  // decides, which is the same answer one round trip earlier.
  const servedBasis = state.data?.since?.basis;
  const sinceApplied = servedBasis ? servedBasis === "requested" : Boolean(since);
  const heading = sinceApplied ? "Since your last check" : "In the last 24 hours";
  const headingSuffix = sinceApplied ? relativeTime(state.data?.since?.at || since) : "";

  // SCOPE, SET AT INTEGRATION. This panel is the CROSS-PORTFOLIO feed — every company the user
  // follows — and it is not the same object as the ticker page's "What changed in this read" or the
  // detail panel's "Recorded thesis changes". The scope line says so in the heading rather than
  // leaving three surfaces to be told apart by what they happen to contain. §9.18-adjacent
  // reasoning: a heading that could name two different things is a heading that names neither.
  const scope = "Across your companies";

  const d = state.data || {};
  const items = d.items || [];
  const materialEvents = d.materialEvents ?? 0;
  const companiesChanged = d.companiesChanged ?? 0;
  const documentsProcessed = d.documentsProcessed ?? 0;
  const nothingChanged = state.status === "ready" && !materialEvents && !companiesChanged;

  return (
    <Panel variant="glass" className={className}>
      <header className="mb-3 flex flex-wrap items-baseline gap-x-2.5">
        <Label as="h2" tone="accent">
          {scope}
        </Label>
        <Label as="span" className="!inline">
          {heading}
        </Label>
        {headingSuffix && <Label as="span" className="!inline">{headingSuffix}</Label>}
      </header>

      {state.status === "loading" && (
        <div className="flex flex-col gap-2.5" aria-busy="true">
          <Skeleton className="h-7 w-56" />
          <Skeleton className="h-7 w-64" />
          <Skeleton className="h-3 w-40" />
        </div>
      )}

      {state.status === "error" && (
        <p className="m-0 text-[13px] leading-[1.6] text-muted">
          Could not load what changed. {state.error?.message || ""}
        </p>
      )}

      {state.status === "unavailable" && (
        <p className="m-0 max-w-[70ch] text-[13px] leading-[1.6] text-muted">
          The change feed could not be reached. This reports what changed across your companies
          when it is available. It is not a statement that nothing changed.
        </p>
      )}

      {state.status === "ready" && (
        <>
          {nothingChanged ? (
            // Zero change is FIRST-CLASS and it is a GOOD outcome. Rendered calmly, at content
            // scale, with no warning colour and no empty-state frame.
            <p className="m-0 max-w-[62ch] text-[15px] leading-[1.5] text-fg">
              Nothing material changed {sinceApplied ? "since your last check" : "in the last 24 hours"}.
            </p>
          ) : (
            <div className="flex flex-col gap-1.5">
              <Metric value={materialEvents} label="material events detected" />
              <Metric value={companiesChanged} label="followed companies changed meaningfully" />
            </div>
          )}

          {/* The small half of the inversion. Volume is METADATA — mono, dim, subordinate. It is
              what was read, not what was concluded. */}
          {documentsProcessed > 0 && (
            <Label as="p" className="mt-2.5">
              {documentsProcessed} source documents processed
            </Label>
          )}

          {items.length > 0 && (
            <div className="mt-3 flex flex-col gap-0.5 border-t border-line pt-2.5">
              {items.map((item, i) => (
                <ChangeRow
                  key={item.id || i}
                  item={item}
                  onOpenEvent={onOpenEvent}
                  onOpenCompany={onOpenCompany}
                />
              ))}
            </div>
          )}

          <DegradedNotice degraded={d.degraded} className="mt-3" />
        </>
      )}
    </Panel>
  );
}
