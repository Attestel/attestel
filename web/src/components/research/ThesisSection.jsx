import { Tag } from "../terminal/bits.jsx";
import { SectionShell } from "./Placeholder.jsx";
import ThesisWorkspace from "./thesis/ThesisWorkspace.jsx";

// ThesisSection — the user's belief about this company, and the only place it is authored.
//
// Step 05 replaced the Step 03 body (the free-text ThesisTracker) with the structured workspace built
// on Step 04's versioned thesis aggregate: claim, time horizon, key assumptions, catalysts, risks,
// invalidation conditions, confidence, next questions, lifecycle, and version history.
//
// The frame is unchanged on purpose — same route (#research/thesis), same position in the section
// strip, same ownership banner — exactly as the Step 03 seam comment anticipated.
//
// Ownership is stated on the surface (guide §8.4) because it is the load-bearing rule of this section:
// the claim and every structured field are user-authored and nothing in this app rewrites them; the
// review beside them is a MODEL review of that claim against collected facts, and it is about the
// thesis, never about the stock.
//
// NOT in this section, and not pretended to be: saved evidence (Step 06 owns persistence and the
// Evidence UI — the workspace renders the boundary only) and the contextual lens contract (Step 07).
// The stored `lastCheck` shown here is the existing read-only compatibility output, not a new lens.
export default function ThesisSection({ ticker }) {
  return (
    <div className="flex flex-col gap-3">
      <SectionShell
        title={`${ticker} · Thesis`}
        tags={
          <>
            <Tag tone="outline">you author this</Tag>
            <Tag tone="llm">model reviews it</Tag>
          </>
        }
      >
        <div className="grid grid-cols-1 divide-y divide-line sm:grid-cols-2 sm:divide-x sm:divide-y-0">
          <div className="px-[22px] py-3.5">
            <p className="text-[11.5px] font-semibold uppercase tracking-[0.08em] text-fg/70">
              Your claim
            </p>
            <p className="mt-1.5 text-[12px] leading-relaxed text-muted">
              What you believe about {ticker} and why, in your own words — with the assumptions it rests
              on and the conditions that would change your mind. Yours to write and yours to change;
              nothing in this app rewrites it.
            </p>
          </div>
          <div className="px-[22px] py-3.5">
            <p className="text-[11.5px] font-semibold uppercase tracking-[0.08em] text-llm">
              Evidence check
            </p>
            <p className="mt-1.5 text-[12px] leading-relaxed text-muted">
              A model compares your claim against the facts collected for {ticker} and says whether they{" "}
              <span className="text-fg/80">confirm</span>, are <span className="text-fg/80">neutral</span>{" "}
              toward, or <span className="text-fg/80">challenge</span> it. A verdict on your reasoning —
              not on the stock, and not a decision.
            </p>
          </div>
        </div>
      </SectionShell>

      <ThesisWorkspace ticker={ticker} />
    </div>
  );
}
