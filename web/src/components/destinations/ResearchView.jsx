import { useEffect, useState } from "react";
import { SubNav } from "../shell/SubNav.jsx";
import InvestigationSection from "../research/InvestigationSection.jsx";
import ThesisSection from "../research/ThesisSection.jsx";
import EvidenceSection from "../research/EvidenceSection.jsx";
import PerspectivesSection from "../research/PerspectivesSection.jsx";
import ScenariosSection from "../research/ScenariosSection.jsx";
import HistorySection from "../research/HistorySection.jsx";
import DecisionsSection from "../research/DecisionsSection.jsx";
import { DestinationHeader } from "../shell/DestinationHeader.jsx";
import CatalystsPanel from "../research/CatalystsPanel.jsx";
import TranscriptsView from "../TranscriptsView.jsx";
import OverviewSection from "../research/overview/OverviewSection.jsx";
import { Tabs } from "../ui/index.js";
import { cx } from "../../lib/cx.js";

// ResearchView — question-led research automation for one company.
//
// The visible navigation is intentionally only Investigations + My Thesis. Specialist workspaces
// remain addressable by deep link for saved research and specialist workflows, but they no longer
// confront a new arrival as a seven-tab feature inventory.
//
//   investigations  ask one question -> watch real work -> receive one finite report
//   thesis           author and review the user's claim
//   evidence      what is known, by kind              (fundamentals · chart · news · transcripts ·
//                                                      catalysts · model evidence)
//   perspectives  what am I missing                   (research lenses + review with perspectives)
//   scenarios     what if                             (hypothetical chains, explicitly invoked)
//   history       how the record has moved            (stored read / transcript / committee snapshots)
//   decisions     what I did about it                 (recorded trades; decision records are Step 09)
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// WAVE 4 LANE 4C — the focused Overview surface, added ADDITIVELY.
//
// Five tabs, no sprawl: Overview · Events · Technical · Fundamentals · Earnings. Three of them
// REUSE the existing Research surfaces rather than rebuilding them — that is the additive-only
// rule, and rebuilding a specialist workspace to sit under a calmer tab strip would be exactly the
// regression this lane must not cause.
//
//   overview      the first viewport (doc §16.9): Attestel view, What Changed, Upcoming, Latest
//   events        this ticker's material events, from the same feed Following reads
//   technical     -> EvidenceSection, `chart` category  (chart context, confluence, regime)
//   fundamentals  -> EvidenceSection, `fundamentals` category  (incl. EPS beat/miss)
//   earnings      -> the EXISTING CatalystsPanel + TranscriptsView, composed
//
// NOTE ON `earnings`, measured rather than assumed: the brief says the evidence tabs "already
// render chart context, fundamentals and earnings", but there is no `earnings` evidence category.
// The six are fundamentals · chart · news · transcripts · catalysts · model, and the EPS beat/miss
// table lives INSIDE `FundamentalsView`. So this tab composes the two existing surfaces that
// actually carry earnings — what is scheduled, and what was said on the call — and reimplements
// neither. Recorded in the Lane 4C handoff.
//
// NOTHING IS DELETED AND NOTHING IS RENAMED. All seven original subviews still render below,
// unchanged, and every existing deep link still resolves. The five new ids are not yet in
// `routes.js` — that file is the integrator's, and the replacement array is quoted in the handoff.
// Until it lands, these branches are unreachable by design rather than by accident.
//
// TAB STATE, deliberately: the strip drives `onSubview`, so the tab IS the route. It does not hold
// local state that would desynchronise from the URL, and it keeps working both before and after
// AD-9 moves the ticker into the hash.
const OVERVIEW_TABS = [
  { value: "overview", label: "Overview" },
  { value: "events", label: "Events" },
  { value: "technical", label: "Technical" },
  { value: "fundamentals", label: "Fundamentals" },
  { value: "earnings", label: "Earnings" },
];

const OVERVIEW_SUBVIEWS = OVERVIEW_TABS.map((t) => t.value);

// Opening Research performs no research call. The bounded job begins only when Run research is pressed.
export default function ResearchView({
  subview,
  onSubview,
  tab,
  onTab,
  level,
  data,
  // `quote` was REFERENCED but never destructured — the Wave 4 integration pass found it by
  // loading the page: `ReferenceError: quote is not defined` crashed the whole Research
  // destination. `App.jsx` was already passing it; Rollup cannot catch an undeclared identifier,
  // and the lane that wrote the two call sites had no browser connected to load the route.
  quote,
  ticker,
  timeframe,
  onTimeframe,
  hasSeed,
  loading,
  onLogTrade,
  onApplyLens,
  onOpenScenario,
  // Wave 4 Lane 4C, both OPTIONAL so this component behaves exactly as before when the integrator
  // has not yet wired them (`App.jsx` is reserved; the literal lines are in the handoff).
  //   onAsk({ kind, ticker, eventId, label, facts, suggestions }) -> launches the Copilot drawer
  //   onOpenEvent(eventId)                                        -> opens Lane 4B's detail panel
  onAsk,
  onOpenEvent,
}) {
  // Cross-section navigation from a contextual result action into a specialist workspace.
  const goSection = (section, sectionTab = null) => onSubview(section, sectionTab);

  const inOverview = OVERVIEW_SUBVIEWS.includes(subview);

  // The evidence category shown under the Technical / Fundamentals tabs. Seeded from whichever tab
  // the user opened and then owned by the inner strip, so both remain live. It is deliberately NOT
  // routed: the five new subviews carry no `tabs` array in the recorded `routes.js` change, and
  // adding one would put a second tab dimension into every deep link for a cosmetic reason.
  const [evidenceTab, setEvidenceTab] = useState("chart");
  useEffect(() => {
    if (subview === "technical") setEvidenceTab("chart");
    else if (subview === "fundamentals") setEvidenceTab("fundamentals");
  }, [subview]);

  return (
    <div className={cx(
      "flex flex-col gap-3",
      subview === "investigations" && "h-[calc(100dvh-88px)] overflow-hidden"
    )}>
      <DestinationHeader view="research" className="mb-1" />

      {/* The five-item strip replaces SubNav ONLY on the five new subviews. On every pre-existing
          subview SubNav renders exactly as it always has — the additive-only rule in one branch. */}
      {inOverview ? (
        <div className="flex items-center">
          <Tabs
            items={OVERVIEW_TABS}
            value={subview}
            onChange={(s) => onSubview(s)}
            ariaLabel={`${ticker} research sections`}
          />
        </div>
      ) : (
        <SubNav
          view="research"
          subview={subview}
          onChange={(s) => onSubview(s)}
          level={level}
          title={ticker}
        />
      )}

      {subview === "overview" && (
        <OverviewSection
          ticker={ticker}
          data={data}
          quote={quote}
          onAsk={onAsk}
          onOpenEvent={onOpenEvent}
        />
      )}

      {/* Events — this ticker's material feed. It reuses the Overview section's own event block
          rather than Lane 4A's card, for the reason stated in that file. */}
      {subview === "events" && (
        <OverviewSection
          ticker={ticker}
          data={data}
          quote={quote}
          onAsk={onAsk}
          onOpenEvent={onOpenEvent}
          eventsOnly
        />
      )}

      {/* Technical and Fundamentals REUSE EvidenceSection, opened on the matching category.

          The category is held in LOCAL state rather than in the route, and `onTab` is forwarded to
          it. The alternative — a fixed `tab` with a no-op `onTab` — would leave EvidenceSection's
          own six-category strip on screen and DEAD, and a control that does nothing when clicked is
          a worse outcome than one more control. This keeps every existing capability of the
          evidence surface reachable from the new tabs (additive-only invariant 5) without editing
          EvidenceSection, which this lane does not own. The nested strip is a cosmetic deviation
          from "one calm strip"; it is recorded in the handoff with the one-line refinement the
          integrator can take instead. */}
      {(subview === "technical" || subview === "fundamentals") && (
        <EvidenceSection
          ticker={ticker}
          tab={evidenceTab}
          onTab={setEvidenceTab}
          data={data}
          hasSeed={hasSeed}
          loading={loading}
          timeframe={timeframe}
          onTimeframe={onTimeframe}
          onLogTrade={onLogTrade}
        />
      )}

      {subview === "earnings" && (
        <div className="flex flex-col gap-3">
          <CatalystsPanel ticker={ticker} catalysts={data?.catalysts} hasSeed={hasSeed} />
          <TranscriptsView ticker={ticker} />
        </div>
      )}

      {subview === "investigations" && (
        <InvestigationSection ticker={ticker} level={level} onOpenSection={goSection} />
      )}

      {subview === "thesis" && <ThesisSection ticker={ticker} />}

      {subview === "evidence" && (
        <EvidenceSection
          ticker={ticker}
          tab={tab}
          onTab={onTab}
          data={data}
          hasSeed={hasSeed}
          loading={loading}
          timeframe={timeframe}
          onTimeframe={onTimeframe}
          onLogTrade={onLogTrade}
        />
      )}

      {subview === "perspectives" && (
        <PerspectivesSection ticker={ticker} onApplyLens={onApplyLens} />
      )}

      {subview === "scenarios" && (
        <ScenariosSection ticker={ticker} onOpenScenario={onOpenScenario} />
      )}

      {subview === "history" && <HistorySection ticker={ticker} />}

      {subview === "decisions" && <DecisionsSection ticker={ticker} />}
    </div>
  );
}
