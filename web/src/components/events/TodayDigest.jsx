import { useEffect, useState } from "react";
import { Button, Label, Panel, Skeleton } from "../ui/index.js";
import { fetchCalendar, listTheses } from "../../lib/api.js";
import {
  BAND_LABELS,
  changedUnavailable,
  fetchChanged,
  fetchFollowing,
  MIN_IMPORTANCE_HIGH,
  importanceBand,
  listSubscriptions,
} from "../../lib/eventsApi.js";
import { readLastCheck } from "./lastCheck.js";

// TodayDigest — doc §16.10. Today, rebuilt around MATERIAL CHANGE rather than volume.
//
// The product claim, in one sentence: "87 new articles" creates work for the user; "7 material
// developments across your 12 followed companies" says Attestel already did the work. Every number
// in this block is a count of CHANGES or of the user's own companies. There is no article count
// anywhere in it, and there is no score.
//
// It composes only from data that already exists — `/api/changed`, `/api/following` filtered to
// high, the EXISTING `/api/calendar`, and `listSubscriptions()`. It adds no endpoint of its own.
//
// INVARIANT #4 — NOTHING HERE CALLS A MODEL. Every fetch below is deterministic. "View morning
// brief" is a BUTTON that hands off through `onViewBrief`; this component never touches
// `/api/brief/*` itself, so no page load, ticker change or re-render can reach the model through it.

// thesisRollup — the "your following" line.
//
// MEASURED, AND THE BRIEF IS WRONG ABOUT THIS. Doc §16.10's mock reads "8 bullish · 3 neutral ·
// 1 bearish", but a stored thesis HAS NO DIRECTION FIELD: `journal/theses.go:176-206` defines
// Thesis with claim, status, horizon, confidence, items, and `lastCheck` — and `ThesisCheck.Verdict`
// (`journal/theses.go:116`) is `confirming | neutral | challenged`, a verdict on THE USER'S CLAIM,
// never on the stock. `CLAUDE.md` says so explicitly of `thesis.py`.
//
// Mapping "confirming" onto "bullish" would be a category error with a direction attached: a
// confirming check on a BEARISH thesis is bearish. So this reports the vocabulary that is actually
// stored, and the row says "theses", not "companies".
//
// Running the analyst to get a real direction is a model call, which invariant #4 forbids on a page
// load. The unassessed bucket is therefore the COMMON case and is shown explicitly — never folded
// into "neutral", which would claim an assessment nobody made.
const VERDICT_LABELS = {
  confirming: "confirming",
  neutral: "neutral",
  challenged: "challenged",
};

const VERDICT_TONE = {
  confirming: "text-up",
  neutral: "text-muted",
  challenged: "text-down",
};

function thesisRollup(theses = []) {
  const counts = { confirming: 0, neutral: 0, challenged: 0, unassessed: 0 };
  let anyThesis = false;
  for (const t of theses) {
    anyThesis = true;
    const v = t?.lastCheck?.verdict;
    if (v && VERDICT_LABELS[v]) counts[v] += 1;
    else counts.unassessed += 1;
  }
  // No stored thesis for anyone ⇒ omit the row entirely rather than render a row of zeros.
  return anyThesis ? counts : null;
}

function SectionHead({ children }) {
  return (
    <Label as="h3" tone="accent" className="mb-1.5">
      {children}
    </Label>
  );
}

export default function TodayDigest({ uid, onOpenEvent, onOpenCompany, onViewBrief, className }) {
  const [state, setState] = useState({ status: "loading" });

  useEffect(() => {
    let alive = true;
    setState({ status: "loading" });
    const since = readLastCheck(uid);

    // Each source fails INDEPENDENTLY. A calendar outage must not blank the change summary, and a
    // change feed that does not exist yet (§9.1 — Wave 5B owns `/api/changed`) must not blank the
    // attention list. `Promise.allSettled`, never `Promise.all`.
    Promise.allSettled([
      fetchChanged({ since }),
      fetchFollowing({ minImportance: MIN_IMPORTANCE_HIGH, limit: 6, since }),
      fetchCalendar(),
      listSubscriptions(),
      listTheses("", "active"),
    ]).then(([changed, following, calendar, subs, theses]) => {
      if (!alive) return;
      setState({
        status: "ready",
        changed: changed.status === "fulfilled" ? changed.value : null,
        changedMissing:
          changed.status === "rejected" ? changedUnavailable(changed.reason) : false,
        attention: following.status === "fulfilled" ? following.value : null,
        calendar: calendar.status === "fulfilled" ? calendar.value : null,
        subscriptions: subs.status === "fulfilled" ? subs.value : [],
        theses: theses.status === "fulfilled" ? theses.value : [],
      });
    });
    return () => {
      alive = false;
    };
  }, [uid]);

  if (state.status === "loading") {
    return (
      <Panel variant="glass" className={className}>
        <div className="flex flex-col gap-2.5" aria-busy="true">
          <Skeleton className="h-5 w-80" />
          <Skeleton className="h-3 w-full" />
          <Skeleton className="h-3 w-2/3" />
        </div>
      </Panel>
    );
  }

  const followedCount = state.subscriptions?.length || 0;
  const materialEvents = state.changed?.materialEvents ?? null;
  const companiesChanged = state.changed?.companiesChanged ?? null;
  const attention = (state.attention?.items || []).filter(
    (i) => importanceBand(i) === "high"
  );
  const now = new Date();
  const today = [
    now.getFullYear(), String(now.getMonth() + 1).padStart(2, "0"), String(now.getDate()).padStart(2, "0"),
  ].join("-");
  const calendarEvents = (state.calendar?.events || []).filter((event) => event.date === today).slice(0, 4);
  const rollup = thesisRollup(state.theses);

  return (
    <Panel variant="glass" className={className} bodyClassName="flex flex-col gap-4">
      {/* THE LEAD SENTENCE. Counts material developments and the user's own companies — never
          articles, never headlines. When the change feed is not yet served it says what it knows
          (how many companies are followed) and does not invent a developments count. */}
      <p className="m-0 max-w-[68ch] text-[19px] font-[550] leading-[1.35] tracking-[-0.02em] text-fg">
        {materialEvents != null ? (
          <>
            {materialEvents === 0 ? "No" : materialEvents} material{" "}
            {materialEvents === 1 ? "development" : "developments"} across your {followedCount}{" "}
            followed {followedCount === 1 ? "company" : "companies"}
          </>
        ) : (
          <>
            You follow {followedCount} {followedCount === 1 ? "company" : "companies"}
          </>
        )}
      </p>

      {state.changedMissing && (
        <p className="m-0 max-w-[72ch] text-[12.5px] leading-[1.55] text-muted">
          Material-change counts are not served yet — this is not a statement that nothing changed.
        </p>
      )}

      {attention.length > 0 && (
        <section>
          <SectionHead>Needs attention</SectionHead>
          <div className="flex flex-col">
            {attention.map((item) => (
              <button
                key={item.id}
                type="button"
                onClick={() => onOpenEvent?.(item.id)}
                className="-mx-2 flex flex-wrap items-baseline gap-x-2.5 gap-y-0.5 rounded-lg px-2 py-1.5 text-left transition-colors hover:bg-panel2"
              >
                <span className="w-[62px] flex-none font-mono text-[11px] uppercase tracking-[.1em] text-fg">
                  {item.tickers?.find((t) => t.followed)?.ticker || item.tickers?.[0]?.ticker || "—"}
                </span>
                <span className="min-w-0 flex-1 text-[13.5px] leading-[1.5] text-fg">
                  {item.title}
                </span>
                {/* Severity is `warn`. Never `down` — a high-impact POSITIVE event that reads red
                    is the bug locked decision 3 exists to prevent. */}
                <span className="flex-none font-mono text-[9px] uppercase tracking-[.12em] text-warn">
                  {BAND_LABELS.high}
                </span>
              </button>
            ))}
          </div>
        </section>
      )}

      {calendarEvents.length > 0 && (
        <section>
          <SectionHead>Today&rsquo;s events</SectionHead>
          <div className="flex flex-wrap items-baseline gap-x-5 gap-y-1.5">
            {calendarEvents.map((ev, i) => (
              <span key={`${ev.date}-${ev.name}-${i}`} className="inline-flex items-baseline gap-2">
                <span className="font-mono text-[11px] tabular-nums text-dim">
                  {ev.timeET || ev.date}
                </span>
                <span className="text-[13px] text-muted">{ev.name}</span>
              </span>
            ))}
          </div>
        </section>
      )}

      {(rollup || companiesChanged != null) && (
        <section>
          <SectionHead>Your following</SectionHead>
          <div className="flex flex-col gap-1">
            {rollup && (
              <p className="m-0 flex flex-wrap items-baseline gap-x-2 text-[13.5px] leading-[1.5] text-muted">
                {["confirming", "neutral", "challenged"]
                  .filter((k) => rollup[k] > 0)
                  .map((k, i, arr) => (
                    <span key={k}>
                      <span className={VERDICT_TONE[k]}>
                        {rollup[k]} {VERDICT_LABELS[k]}
                      </span>
                      {i < arr.length - 1 || rollup.unassessed > 0 ? (
                        <span className="mx-1 text-dim">·</span>
                      ) : null}
                    </span>
                  ))}
                {rollup.unassessed > 0 && (
                  <span className="text-dim">{rollup.unassessed} not yet assessed</span>
                )}
              </p>
            )}
            {rollup && (
              <p className="m-0 max-w-[72ch] text-[12px] leading-[1.55] text-muted">
                Verdicts on your own written theses, not on the companies.
              </p>
            )}
            {/* §16.10's mock says "2 theses changed since yesterday", but `/api/changed` returns
                `companiesChanged` — a COMPANY-level count. Upgrading it to a thesis-level claim
                would be a stronger statement than the data supports, so the wording follows the
                field. */}
            {companiesChanged != null && (
              <p className="m-0 text-[13.5px] leading-[1.5] text-muted">
                {companiesChanged} followed {companiesChanged === 1 ? "company" : "companies"} changed
                meaningfully
              </p>
            )}
          </div>
        </section>
      )}

      {/* Model-backed, and therefore a BUTTON — never a mount effect, never an interval. This
          component does not call the brief itself; it hands off, so invariant #4 cannot be broken
          from inside this file. Rendered only when the host wired the handler: a button that does
          nothing is worse than no button. */}
      {onViewBrief && (
        <div className="flex justify-end">
          <Button size="sm" onClick={onViewBrief}>
            View morning brief
          </Button>
        </div>
      )}
    </Panel>
  );
}
