import { Tag } from "../terminal/bits.jsx";
import { SectionShell } from "./Placeholder.jsx";

// ScenariosSection — structured what-if reasoning about this company.
//
// The scenario flow already exists end to end (POST /api/scenario/{ticker} -> services/llm/app/scenario.py,
// grounded in the supply-chain / roadmap / risk seed plus the live regime and news). It is reached through
// the Copilot's scenario mode, so this section frames it in company context and launches it there rather
// than duplicating that UI — Step 07 owns any object-first rework.
//
// It stays EXPLICITLY invoked: a scenario is several model calls, and Research must not run one on open.
//
// Wording is load-bearing (guide §3.1): a scenario is a hypothetical chain of consequences, qualitative
// only. It is not a forecast, not a probability, and not a recommendation.

const STARTERS = (t) => [
  `What if ${t}'s main supply constraint eases faster than expected?`,
  `What if a key customer moves a large workload to in-house silicon?`,
  `What if export restrictions tighten again on ${t}'s largest non-domestic market?`,
  `What if next quarter's guidance comes in flat instead of up?`,
];

export default function ScenariosSection({ ticker, onOpenScenario }) {
  return (
    <SectionShell
      title={`${ticker} · Scenarios`}
      tags={<Tag tone="warn">hypothetical</Tag>}
      right={
        <button
          type="button"
          onClick={() => onOpenScenario()}
          className="rounded-full border border-accent/50 bg-accent/10 px-3.5 py-1.5 text-[12.5px] font-semibold text-accent transition-colors hover:bg-accent/16"
        >
          Start a scenario →
        </button>
      }
      note="A scenario traces assumptions to first-order effects, second-order effects, and what to watch — grounded in the collected facts for this company. It is a thought experiment, NOT a prediction, a probability, or a recommendation."
    >
      <div className="px-[22px] py-4">
        <p className="text-[12.5px] leading-relaxed text-muted">
          Ask a &ldquo;what if&rdquo; and get a structured chain rather than a paragraph. Useful before a
          catalyst, when you want to know which of your assumptions a shock would actually break.
        </p>

        <p className="mt-4 label-mono text-muted">
          Try one of these
        </p>
        <ul className="mt-2 flex flex-col gap-1.5">
          {STARTERS(ticker).map((q) => (
            <li key={q}>
              <button
                type="button"
                onClick={() => onOpenScenario(q)}
                className="w-full rounded-md border border-line px-3 py-2 text-left text-[12.5px] leading-snug text-fg/85 transition-colors hover:border-accent/50 hover:bg-panel2"
              >
                {q}
              </button>
            </li>
          ))}
        </ul>
      </div>
    </SectionShell>
  );
}
