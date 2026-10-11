import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";

import {
  pollExperimentReview, PAPER_LABELS, CANDIDATE_LABELS,
  EVALUATOR_VALIDITY_LABELS, evaluatorResultLabel,
} from "../src/lib/experimentReviewApi.js";

// experimentReview.test.mjs — the two browser-side properties this lane must hold.
//
//  1. A poll that is aborted STOPS. An unmount must not leave a loop hitting the run route on its
//     own cadence and resolving into the state of a component nobody is looking at.
//  2. The panel starts NOTHING on its own — no mount fetch, no interval, no timer.
//
// (1) is behavioural. (2) is asserted against the source text, because there is no jsdom or React
// test renderer in this project and adding one to prove a negative would be a heavier dependency
// than the thing it proves. A source assertion catches the edit that actually causes the bug — a
// `setInterval` added to the panel — which is what matters.

const PANEL = new URL("../src/components/ExperimentReviewPanel.jsx", import.meta.url);
const panelSource = readFileSync(PANEL, "utf8");

// stripComments removes `//` line comments and `/* */` blocks so the source assertions below test
// the CODE rather than the prose that explains it. The panel's header discusses intervals and
// timers at length precisely because it must not have any.
function stripComments(src) {
  return src.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
}

const panelCode = stripComments(panelSource);

test("an aborted review poll stops and never ticks again", async () => {
  const previous = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = async () => {
    calls += 1;
    return {
      ok: true,
      status: 200,
      json: async () => ({ id: "agr_1", status: "running", pollAfterMs: 10 }),
    };
  };

  const controller = new AbortController();
  const ticks = [];
  try {
    const promise = pollExperimentReview("agr_1", {
      onTick: (r) => ticks.push(r),
      signal: controller.signal,
    });
    // Let the loop make progress, then abort mid-flight.
    await new Promise((r) => setTimeout(r, 40));
    const before = calls;
    controller.abort();
    const result = await promise;

    assert.equal(result, null, "an aborted poll must resolve to null, not to a run");
    // Give the loop every chance to fire again if it were still alive.
    const ticksAtAbort = ticks.length;
    await new Promise((r) => setTimeout(r, 60));
    assert.ok(
      calls <= before + 1,
      `the poll kept fetching after abort (${before} -> ${calls}); an unmounted panel must stop`
    );
    assert.equal(
      ticks.length,
      ticksAtAbort,
      "onTick fired after abort; a stale run would be written into a dead component"
    );
  } finally {
    globalThis.fetch = previous;
  }
});

test("the panel starts nothing on its own", () => {
  for (const forbidden of ["setInterval", "setTimeout", "requestAnimationFrame"]) {
    assert.ok(
      !panelCode.includes(forbidden),
      `ExperimentReviewPanel uses ${forbidden}; nothing in this panel may run on a timer`
    );
  }
  // Exactly one effect, and it must be the empty-body unmount cleanup. A second one, or one with a
  // body, is the edit that turns "inert until clicked" into "fetches on mount".
  const effects = panelCode.match(/useEffect\(/g) || [];
  assert.equal(
    effects.length,
    1,
    `ExperimentReviewPanel has ${effects.length} effects; it may have exactly one, the unmount cleanup`
  );
  assert.match(
    panelCode,
    /useEffect\(\(\)\s*=>\s*\(\)\s*=>[^,]*abort\(\),\s*\[\]\)/,
    "the one effect must be a cleanup-only unmount abort with empty deps"
  );
});

test("the panel aborts an in-flight poll before starting another", () => {
  // Two clicks in a row must not leave two loops running against the same component.
  assert.match(
    panelCode,
    /abortRef\.current\?\.abort\(\)/,
    "the click handler does not abort a previous poll"
  );
});

// The copy is the thing that keeps a reader from collapsing the two axes, so it is pinned rather
// than left to a future edit's judgement.
test("the paper and candidate labels cannot be read as the same statement", () => {
  assert.match(
    PAPER_LABELS.unjudged.help,
    /says nothing about whether the strategy works/i,
    "the `unjudged` copy does not disclaim being a finding about the strategy"
  );
  assert.match(
    CANDIDATE_LABELS.no_edge.help,
    /says nothing about whether the paper experiment has run/i,
    "the `no_edge` copy does not disclaim being a finding about the paper experiment"
  );
  // The `unknown` CONCLUSION no longer carries the missing-vs-negative distinction itself — that
  // distinction moved to the separate result/validity pair, because the old single-field version
  // had to claim "no usable verdict was read", which is false when one was. What this copy must
  // still do is refuse to be read as a finding, and send the reader to the fields that answer it.
  assert.match(
    CANDIDATE_LABELS.unknown.help,
    /no conclusion.*can be drawn/i,
    "the `unknown` copy must say it is an absence of conclusion, not a finding"
  );
  // The distinction itself must exist SOMEWHERE, and that somewhere is the validity axis.
  assert.notEqual(
    EVALUATOR_VALIDITY_LABELS.invalid.help,
    EVALUATOR_VALIDITY_LABELS.unknown.help,
    "a recorded-but-inapplicable verdict and a never-recorded one read identically"
  );
});


// ─────────────────────────────────── the acceptance case: three facts, not one

// THE STATE THE DEPLOYMENT IS ACTUALLY IN:
//
//   trainedOnSynthetic = true  +  evaluator NO EDGE  +  paper clock not started
//
// Three independent facts. A reader who collapses any two of them reaches a wrong conclusion:
//
//   * paper + candidate collapsed  -> "the strategy failed" (it was never measured live)
//   * result + validity collapsed  -> "nobody measured anything" (the evaluator DID run)
//   * validity + conclusion collapsed -> "there is no edge" (that was never established for the
//     real market)
//
// The panel must therefore surface all three separately. This test asserts the rendered vocabulary
// and copy make each one visible and none of them derivable-by-assumption from another.
test("the acceptance state communicates three facts without conflating them", () => {
  // 1. THE PAPER EXPERIMENT WAS NOT MEASURED, and the copy disclaims being about the strategy.
  const paper = PAPER_LABELS.unjudged;
  assert.match(paper.label, /not measured/i);
  assert.match(paper.help, /says nothing about whether the strategy works/i);

  // 2. THE EVALUATOR'S RAW RESULT IS SHOWN VERBATIM. Not mapped, not summarised, not hidden.
  assert.equal(evaluatorResultLabel("NO EDGE"), "NO EDGE");
  assert.equal(evaluatorResultLabel("SUSPECT"), "SUSPECT");
  assert.equal(evaluatorResultLabel("MIXED"), "mixed across configs");
  // A genuinely absent verdict reads as absent, and is a different string from any verdict.
  assert.equal(evaluatorResultLabel(""), "none recorded");
  assert.equal(evaluatorResultLabel(null), "none recorded");

  // 3. VALIDITY IS ITS OWN AXIS, and `invalid` must say a verdict WAS recorded.
  const invalid = EVALUATOR_VALIDITY_LABELS.invalid;
  assert.match(invalid.help, /verdict WAS recorded/i,
    "the `invalid` copy must state that a verdict exists, or a reader assumes none does");
  assert.match(invalid.help, /synthetic|unstated/i,
    "the `invalid` copy must say why it does not apply");
  // And `unknown` validity — no verdict at all — must be distinguishable from `invalid`.
  assert.match(EVALUATOR_VALIDITY_LABELS.unknown.help, /No evaluator verdict was recorded at all/i);
  assert.notEqual(EVALUATOR_VALIDITY_LABELS.unknown.label, invalid.label);

  // THE REGRESSION. The conclusion copy must NOT claim a verdict was unread — that sentence was
  // false in exactly this state.
  const conclusion = CANDIDATE_LABELS.unknown;
  assert.doesNotMatch(
    conclusion.help,
    /no usable evaluator verdict was read|the evaluator never ran|nothing was measured/i,
    "the `unknown` conclusion copy claims no verdict was read; in the acceptance state one WAS"
  );
  // It must instead point the reader at the two fields that answer that question.
  assert.match(conclusion.help, /evaluator result/i);
  assert.match(conclusion.help, /validity/i);
});

// The panel must actually RENDER the pair, unconditionally — not behind a condition that hides it
// in the very state it exists for.
test("the panel renders the evaluator result and its validity", () => {
  assert.match(panelCode, /EvaluatorReadout/,
    "the panel does not render the evaluator readout at all");
  assert.match(panelCode, /review\.evaluatorResult/,
    "the panel never reads review.evaluatorResult");
  assert.match(panelCode, /review\.evidenceValidity/,
    "the panel never reads review.evidenceValidity");
  // Rendered inside the candidate card, so the three facts sit together rather than in three
  // unrelated places. Anchored on the JSX usage, not the import.
  const cardStart = panelCode.indexOf("labels={CANDIDATE_LABELS}");
  assert.ok(cardStart > 0, "the candidate status card is not rendered");
  assert.match(panelCode.slice(cardStart, cardStart + 400), /EvaluatorReadout/,
    "the readout is not rendered beside the candidate conclusion");
  // And it must be unconditional — hidden behind a condition it would be absent in exactly the
  // state it exists for.
  assert.doesNotMatch(
    panelCode.slice(cardStart, cardStart + 400),
    /\{\s*review\.\w+\s*&&\s*<EvaluatorReadout/,
    "the evaluator readout is rendered conditionally; it must always be shown"
  );
});
