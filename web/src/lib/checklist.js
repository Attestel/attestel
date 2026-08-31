// checklist.js — the pre-trade checklist as a pure evaluator (docs/BEGINNER.md Piece 3). It turns
// open-ended analysis into a FINITE yes/no decision by COMPOSING facts the platform already computes —
// the validated signal, the position sizer, multi-timeframe confluence, and the trade draft. It adds
// no analysis and no opinion: it scores PLAN COMPLETENESS ("5 / 6"), never buy/sell (invariant #2).
// No LLM, no network — a pure function over inputs already on the page (invariant #3/#4).
//
// A MISSING fact never silently passes — the item stays unmet with a "not available" detail so the
// beginner SEES the gap rather than being handed a fabricated green tick. Four items auto-evaluate;
// two are self-attest (plan-not-FOMO, and item 4's explicit conflict acknowledgement).

const RISK_CAP = 2; // percent — the hard per-trade cap (matches the sizer + settings)
const RR_FLOOR = 1.5;

const nonEmpty = (s) => typeof s === "string" && s.trim().length > 0;
const usd0 = (n) => `$${Number(n || 0).toLocaleString(undefined, { maximumFractionDigits: 0 })}`;

// hasValidatedSignal — the prediction service returns a non-null `signal` ONLY after a model passes
// its walk-forward gate (services/prediction; backtest.passed), so signal PRESENCE is the gate — there
// is no separate signal.passed field. A soft-failed / absent fetch leaves it null and item 1 falls
// back to the thesis. (Defensive: honor an explicit passed:false if one ever appears.)
function hasValidatedSignal(signal) {
  if (!signal) return false;
  if (signal.passed === false) return false;
  return true;
}

// confluenceAligned — analysis computes trendAlignment as "aligned up" / "aligned down" / a conflict
// string, plus a conflicts[] list (ConfluenceColumn). Aligned when it starts with "aligned".
function confluenceAligned(confluence) {
  return String(confluence?.trendAlignment || "").toLowerCase().startsWith("aligned");
}

// conflictAcknowledged — the user explicitly opts into a counter-trend / pullback entry (item 4's ack,
// or a "conflict acknowledged" tag). Beginners aren't locked out of every pullback — they are made to
// SEE the conflict and take it deliberately.
function conflictAcknowledged(draft) {
  if (draft?.conflictAck === true) return true;
  const tags = Array.isArray(draft?.tags) ? draft.tags : [];
  return tags.some((t) => /conflict[\s-]?ack(nowledged)?/i.test(String(t)));
}

// evaluateChecklist composes the six items. `sizer` is the PositionSizer result (prompt 20), carrying
// accountEquity / dollarRisk / rr / flags. `draft` is the trade being logged (thesis, stop, tags, and
// the self-attest booleans planNotFomo / conflictAck).
export function evaluateChecklist({ dashboard, sizer, draft, mode } = {}) {
  const d = draft || {};
  const signal = dashboard?.signal;
  const confluence = dashboard?.confluence;
  const equity = sizer?.accountEquity || 0;
  const capDollars = equity * (RISK_CAP / 100);

  const items = [];

  // 1. A validated signal (passed backtest) OR a written thesis.
  {
    const sig = hasValidatedSignal(signal);
    const thesis = nonEmpty(d.thesis);
    const detail = sig
      ? `Validated signal present${signal?.direction ? `: ${String(signal.direction).toUpperCase()}` : ""}`
      : thesis
        ? "No validated signal — backed by your written thesis"
        : "No validated signal and no thesis yet — write your reason below";
    items.push({
      key: "signalOrThesis",
      label: "A validated signal or a written thesis",
      passed: sig || thesis,
      auto: true,
      source: sig ? "signal" : "thesis",
      detail,
    });
  }

  // 2. Risk within the 1–2% cap (from the position sizer).
  {
    let passed = false;
    let detail;
    if (!sizer || !(equity > 0)) {
      detail = "Size the trade first — set your account equity in the sizer";
    } else if (sizer.flags?.riskPctOverCap) {
      detail = `Risk % is above the ${RISK_CAP}% cap`;
    } else if (sizer.dollarRisk != null && sizer.dollarRisk <= capDollars + 1e-9) {
      passed = true;
      detail = `Risking ${usd0(sizer.dollarRisk)} ≤ ${RISK_CAP}% cap (${usd0(capDollars)})`;
    } else {
      detail = `Risking ${usd0(sizer.dollarRisk)} exceeds the ${RISK_CAP}% cap (${usd0(capDollars)})`;
    }
    items.push({ key: "riskWithinCap", label: `Risk ≤ ${RISK_CAP}% of equity`, passed, auto: true, source: "sizer", detail });
  }

  // 3. A stop is set and reward:risk ≥ 1.5 (from the draft + sizer).
  {
    const hasStop = Number(d.stop) > 0;
    const rr = sizer?.rr;
    let detail;
    if (!hasStop) detail = "No stop set";
    else if (rr == null) detail = "Add a target so reward:risk can be computed";
    else if (rr < RR_FLOOR) detail = `Reward:risk ${rr.toFixed(1)} is below ${RR_FLOOR}`;
    else detail = `Stop set · reward:risk ${rr.toFixed(1)}`;
    items.push({
      key: "stopAndRR",
      label: `Stop set and reward:risk ≥ ${RR_FLOOR}`,
      passed: hasStop && rr != null && rr >= RR_FLOOR,
      auto: true,
      source: "sizer",
      detail,
    });
  }

  // 4. Multi-timeframe confluence agrees, or the conflict is explicitly acknowledged.
  {
    const aligned = confluenceAligned(confluence);
    const ack = conflictAcknowledged(d);
    let detail;
    if (!confluence) detail = "Confluence not available — use your thesis";
    else if (aligned) detail = `Confluence ${confluence.trendAlignment}`;
    else if (ack) detail = `Conflict acknowledged (${confluence.trendAlignment || "mixed"})`;
    else {
      const c = Array.isArray(confluence.conflicts) ? confluence.conflicts.length : 0;
      detail = `Confluence ${confluence.trendAlignment || "mixed"}${c ? ` · ${c} conflict${c > 1 ? "s" : ""}` : ""} — acknowledge to proceed`;
    }
    items.push({
      key: "confluenceOK",
      label: "Confluence agrees (or the conflict is acknowledged)",
      passed: aligned || ack,
      auto: true,
      source: "confluence",
      detail,
    });
  }

  // 5. Self-attest: entering on plan, not FOMO / news-chasing.
  {
    const passed = d.planNotFomo === true;
    items.push({
      key: "planNotFomo",
      label: "Entering on my plan, not FOMO / news-chasing",
      passed,
      auto: false,
      source: "self",
      detail: passed ? "Attested" : "Confirm you're entering on your plan",
    });
  }

  // 6. Wrote down why (the thesis).
  {
    const passed = nonEmpty(d.thesis);
    items.push({
      key: "wroteWhy",
      label: "Wrote down why (the thesis)",
      passed,
      auto: true,
      source: "thesis",
      detail: passed ? "Thesis written" : "Write your thesis so the review has a 'why'",
    });
  }

  const passedCount = items.filter((i) => i.passed).length;
  return {
    items,
    score: { passed: passedCount, total: items.length },
    allPass: passedCount === items.length,
    mode: mode === "beginner" ? "beginner" : "standard",
  };
}
