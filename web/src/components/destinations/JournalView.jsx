import { useEffect, useState } from "react";
import { Tabs } from "../ui/index.js";
import JournalPanel from "../JournalPanel.jsx";
import PaperTradingPanel from "../PaperTradingPanel.jsx";
import DecisionLog from "../decisions/DecisionLog.jsx";
import OutcomeLog from "../decisions/OutcomeLog.jsx";
import { SectionShell } from "../research/Placeholder.jsx";
import { Tag } from "../terminal/bits.jsx";
import { DestinationHeader } from "../shell/DestinationHeader.jsx";

// JournalView — the Journal destination (Decide · Review): your own record of what you did, why, and
// what you learned.
//
// Four contextual sections, in the order the loop runs:
//
//   decisions    why you acted — or deliberately did not          (decision records)
//   outcomes     how the reasoning held up                        (outcome reviews)
//   trades       what you executed yourself                       (the manual trade ledger, unchanged)
//   experiments  the platform's out-of-sample validation book     (paper, simulation only)
//
// "Journal" is no longer a synonym for "trades". A trade is one possible CONSEQUENCE of a decision;
// a deliberate non-action has no trade at all and is still the thing most worth reviewing. Trades keep
// their own section, their own schema and their own numbers — nothing here merges them with decisions
// or with the simulated book.
//
// NAVIGATION NOTE: all four sections are in lib/routes.js as of the Wave 4 integration — the change
// this step's handoff asked the integrator to make, since routes.js is reserved by Contracts §7.3.
// So every section is deep-linkable (#journal/decisions … #journal/experiments) and drives the
// destination's SubNav through onSubview. `#paper` still resolves explicitly to
// #journal/experiments, and a bare `#journal` now lands on Decisions — the first section of the
// loop, and the one Journal is now about.

const SECTIONS = [
  { value: "decisions", label: "Decisions" },
  { value: "outcomes", label: "Outcomes" },
  { value: "trades", label: "Trades" },
  { value: "experiments", label: "Experiments" },
];

const SECTION_IDS = SECTIONS.map((s) => s.value);

const HINTS = {
  decisions: "Your reasoning, recorded when you had it — not reconstructed afterwards.",
  outcomes: "What happened, and whether the reasoning was sound. Two separate questions.",
  trades: "Trades you executed yourself. This app places no orders and holds no money.",
  experiments: "Simulation only — no orders, no money.",
};

export default function JournalView({
  subview,
  onSubview,
  level,
  tickers,
  ticker,
  prefill,
  dashboard,
}) {
  // The routed subview is now the single source of truth. Local state exists only so the tabs still
  // work if a host renders this view without an onSubview handler; a deep link always wins.
  const [section, setSection] = useState(
    SECTION_IDS.includes(subview) ? subview : "decisions",
  );
  useEffect(() => {
    if (SECTION_IDS.includes(subview)) setSection(subview);
  }, [subview]);

  const choose = (next) => {
    setSection(next);
    onSubview?.(next);
  };

  return (
    <div className="flex flex-col gap-3">
      <DestinationHeader view="journal" className="mb-1" />

      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <Tabs
          items={SECTIONS.map((s) => ({ value: s.value, label: s.label }))}
          value={section}
          onChange={choose}
          size="sm"
          ariaLabel="journal sections"
        />
        <span className="text-[11.5px] text-muted">{HINTS[section]}</span>
      </div>

      {section === "decisions" && (
        <SectionShell
          title="Decisions"
          tags={<Tag tone="outline">your record</Tag>}
          note="Every decision you recorded, newest first — including the ones where you deliberately did nothing. Each keeps the thesis and evidence exactly as they were at the time, so a later review judges the decision you actually made."
        >
          <DecisionLog />
        </SectionShell>
      )}

      {section === "outcomes" && (
        <SectionShell
          title="Outcomes & reviews"
          tags={<Tag tone="outline">what you learned</Tag>}
          note="A review separates two questions that are easy to confuse: what the market did, and whether your reasoning was sound. Only the second one transfers to your next decision."
        >
          <OutcomeLog />
        </SectionShell>
      )}

      {section === "trades" && (
        <JournalPanel
          tickers={tickers}
          defaultTicker={ticker}
          prefill={prefill}
          dashboard={dashboard}
        />
      )}

      {section === "experiments" && <PaperTradingPanel />}
    </div>
  );
}
