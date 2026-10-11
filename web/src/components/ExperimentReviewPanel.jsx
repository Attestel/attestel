import { useCallback, useEffect, useRef, useState } from "react";
import {
  createExperimentSnapshot, startExperimentReview, pollExperimentReview,
  fetchAgencyRun, isTerminal, reviewStageProgress,
  PAPER_LABELS, CANDIDATE_LABELS, OPERATIONAL_LABELS, SOURCE_LABELS,
  EVALUATOR_VALIDITY_LABELS, evaluatorResultLabel,
  AuthRequiredError,
} from "../lib/experimentReviewApi.js";
import { Tag } from "./terminal/bits.jsx";
import { cx } from "../lib/cx.js";

// ExperimentReviewPanel — "Explain this snapshot with Hermes", in Journal → Experiments.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// NOTHING HERE RUNS ON ITS OWN
// ─────────────────────────────────────────────────────────────────────────────────────────────
// There is no mount effect, no interval, no timer, no retry loop and nothing that watches a ticker.
// The panel is inert until the button is pressed, and the only loop that ever runs is
// `pollExperimentReview`, which starts AFTER a click has already queued a review and stops the
// moment that review reaches a terminal state.
//
// THE ONE `useEffect` IN THIS FILE HAS AN EMPTY BODY. It exists solely to return a cleanup that
// aborts an in-flight poll when the component unmounts — navigating away from Journal → Experiments,
// or a reload. It starts nothing on mount and re-runs never (empty deps), so it does not weaken the
// rule above: "nothing runs on page load" is about work being STARTED, and this effect's only job is
// to stop work that a click already started. Without it, a poll survives the unmount, keeps hitting
// the run route on its own cadence, and resolves into the state of a component nobody is looking at.
//
// The surrounding PaperTradingPanel refreshes itself every 30 seconds. That interval does not touch
// any state in this component and cannot start, restart or re-run anything here.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// THREE STATUSES, RENDERED SEPARATELY, WITH THE CONFUSION DESIGNED OUT
// ─────────────────────────────────────────────────────────────────────────────────────────────
// Paper status, candidate status and operational status answer three different questions and are
// never merged into one verdict line. The copy in `experimentReviewApi.js` is written so that
// "the paper experiment has not been measured" and "the candidate strategy produced negative
// evidence" cannot be read as the same statement — because they are not, and a reader who conflates
// them concludes either that a working strategy failed or that a failed one is merely untested.
//
// SIMULATION ONLY — this panel explains an experiment. It produces no signal, and the review it
// displays has no field capable of carrying one.

const BUTTON_LABEL = "Explain this snapshot with Hermes";

function ToneTag({ tone, children }) {
  return <Tag tone={tone === "muted" ? "outline" : tone}>{children}</Tag>;
}

// StatusCard is ONE of the three readings. Each carries its own question, its own value and its own
// explanation, in its own box — the layout itself is what keeps them from being read as one verdict.
function StatusCard({ question, verdict, labels, children }) {
  const value = verdict?.value || "unknown";
  const meta = labels[value] || { label: value, tone: "muted", help: "" };
  return (
    <div className="min-w-0 rounded-md border border-line bg-panel2/40 px-3.5 py-3">
      <div className="label-mono text-muted">{question}</div>
      <div className="mt-1.5">
        <ToneTag tone={meta.tone}>{meta.label}</ToneTag>
      </div>
      <p className="mt-2 text-[12px] leading-[1.55] text-muted">{meta.help}</p>
      {children}
      {verdict?.rationale && (
        <p className="mt-2 border-t border-line/60 pt-2 text-[12.5px] leading-[1.55] text-fg/85">
          {verdict.rationale}
        </p>
      )}
    </div>
  );
}

// EvaluatorReadout renders the two facts the CONCLUSION above is derived from, and it is rendered
// unconditionally.
//
// THE FAILURE IT FIXES. When the evaluator returns `NO EDGE` about a model trained on synthetic
// data, the conclusion is `unknown` — and with only the conclusion on screen, a reader concluded
// nobody had measured anything. The evaluator HAD run and HAD returned a specific answer; that
// answer was reachable only by reading a blocker.
//
// So the raw result is shown verbatim, beside a separate statement of whether it applies. Three
// facts, three lines, none inferred from another.
function EvaluatorReadout({ result, validity }) {
  const meta = EVALUATOR_VALIDITY_LABELS[validity] || EVALUATOR_VALIDITY_LABELS.unknown;
  return (
    <div className="mt-2.5 border-t border-line/60 pt-2.5">
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <span className="label-mono text-muted">Evaluator recorded</span>
        <span className="num text-[13px] font-semibold text-fg">
          {evaluatorResultLabel(result)}
        </span>
      </div>
      <div className="mt-1.5 flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <span className="label-mono text-muted">Applies to real market</span>
        <ToneTag tone={meta.tone}>{meta.label}</ToneTag>
      </div>
      <p className="mt-1.5 text-[12px] leading-[1.55] text-muted">{meta.help}</p>
    </div>
  );
}

// ClaimList renders blockers / unknowns / contradictions with the snapshot fields each one cites.
// The paths are shown rather than hidden: a claim in this lane is only as good as the field it
// rests on, and the server rejected any claim that cited a field the snapshot does not contain.
function ClaimList({ title, items, tone, empty }) {
  if (!items?.length) {
    return (
      <div>
        <div className={cx("label-mono", "text-muted")}>{title}</div>
        <p className="mt-1 text-[12.5px] text-muted">{empty}</p>
      </div>
    );
  }
  return (
    <div>
      <div className="label-mono text-muted">
        {title} <span className="num">({items.length})</span>
      </div>
      <ul className="mt-1.5 flex flex-col gap-2">
        {items.map((c, i) => (
          <li key={`${c.code || "x"}-${i}`} className="text-[12.5px] leading-[1.55]">
            <div className="flex flex-wrap items-baseline gap-1.5">
              {c.code && <ToneTag tone={tone}>{c.code}</ToneTag>}
              <span className="min-w-0 text-fg/90">{c.statement}</span>
            </div>
            {c.evidencePaths?.length > 0 && (
              <div className="mt-0.5 font-mono text-[10.5px] leading-snug text-muted/60">
                {c.evidencePaths.join(" · ")}
              </div>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}

const CHECK_TONE = { blocking: "down", high: "warn", routine: "outline" };

export default function ExperimentReviewPanel() {
  const [snapshot, setSnapshot] = useState(null);
  const [run, setRun] = useState(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState(null);
  // Held so a second click, or an unmount, aborts the in-flight poll rather than leaving it to
  // resolve into state nobody is watching.
  const abortRef = useRef(null);

  // UNMOUNT CLEANUP ONLY — no body, empty deps. See the header for why this is not a violation of
  // "nothing runs on page load": it starts nothing and only ever stops something a click began.
  useEffect(() => () => abortRef.current?.abort(), []);

  // explain is the ONLY entry point in this component, and it is bound to a click.
  //
  // It does three things in order, and every one of them is a step the user asked for: freeze the
  // evidence, queue a review of it, then follow that review. Nothing before the click reaches the
  // network, and nothing after the review terminates keeps running.
  const explain = useCallback(async () => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;

    setError(null);
    setRun(null);
    setBusy("Freezing the experiment evidence…");
    try {
      const snapResp = await createExperimentSnapshot();
      const snap = snapResp?.snapshot;
      if (!snap?.id) throw new Error("the deployment returned no snapshot");
      setSnapshot(snap);

      setBusy("Queuing the review on your machine…");
      const started = await startExperimentReview(snap.id);
      const queued = started?.run;
      if (!queued?.id) throw new Error("the deployment returned no review run");
      setRun(queued);

      // The poll is model-free: it reads a stored row and can neither claim, resume nor extend
      // anything. It stops on its own at a terminal state.
      setBusy("Waiting for the local Hermes worker…");
      const finished = await pollExperimentReview(queued.id, {
        onTick: (r) => { if (!controller.signal.aborted) setRun(r); },
        signal: controller.signal,
      });
      if (finished && !controller.signal.aborted) setRun(finished);
    } catch (err) {
      if (controller.signal.aborted) return;
      if (err instanceof AuthRequiredError) {
        setError("Sign in to run an experiment review.");
      } else if (err?.code === "mixed_generation") {
        // NOT stored, deliberately. See createExperimentSnapshot.
        setError(
          "The deployment changed experiment generation while the evidence was being frozen, so " +
          "the snapshot would have straddled a reset. Nothing was stored — press the button again."
        );
      } else {
        setError(err?.message || "the review could not be started");
      }
    } finally {
      if (!controller.signal.aborted) setBusy("");
    }
  }, []);

  // A one-shot manual refresh for a run the poll stopped following (an aborted click, a reload).
  // Bound to a click like everything else here.
  const refreshRun = useCallback(async () => {
    if (!run?.id) return;
    try {
      setRun(await fetchAgencyRun(run.id));
    } catch (err) {
      setError(err?.message || "the review run could not be read");
    }
  }, [run?.id]);

  const review = run?.review || null;
  const progress = reviewStageProgress(run);
  const running = Boolean(busy) || (run && !isTerminal(run.status));

  return (
    <section className="border-t border-line">
      <div className="flex flex-wrap items-center gap-2.5 border-b border-line px-[22px] py-3">
        <span className="text-[14px] font-semibold text-fg">Explain this experiment</span>
        <Tag tone="outline">Hermes · local</Tag>
        <button
          type="button"
          onClick={explain}
          disabled={running}
          className={cx(
            "ml-auto rounded-md px-3.5 py-2 text-[12.5px] font-semibold transition-colors",
            running
              ? "cursor-not-allowed bg-line text-muted"
              : "bg-accent text-bg hover:opacity-90"
          )}
        >
          {running ? "Working…" : BUTTON_LABEL}
        </button>
      </div>

      {/* The standing explanation of what the button does, so pressing it is an informed act. */}
      <p className="px-[22px] pt-3 text-[12px] leading-[1.6] text-muted">
        Nothing here runs on its own — no polling, no timers, no page-load fetch. Pressing the button
        freezes this deployment&rsquo;s evidence under one timestamp, then asks agents on{" "}
        <strong className="text-fg/85">your own machine</strong> to explain it. The review describes
        the experiment; it produces no signal, cannot change a model or a threshold, and cannot write
        to the paper ledger.
      </p>

      {busy && (
        <p className="px-[22px] pt-2 text-[12.5px] text-muted">
          {busy}
          {progress.label && (
            <span className="num ml-2 text-fg/70">
              stage {progress.index}/{progress.total} · {progress.label}
            </span>
          )}
        </p>
      )}

      {error && (
        <p className="mx-[22px] mt-3 rounded-md bg-down/[0.08] px-3 py-2 text-[12.5px] leading-[1.55] text-down">
          {error}
        </p>
      )}

      {snapshot && <SnapshotHeader snapshot={snapshot} />}

      {run && !review && isTerminal(run.status) && (
        <div className="px-[22px] py-4 text-[12.5px] leading-[1.6] text-muted">
          <strong className="text-fg/85">The review ended as {run.status}.</strong>{" "}
          {run.error || "No reason was recorded."}
          {run.status === "queued" && " No local worker has claimed it yet."}
          <button type="button" onClick={refreshRun} className="ml-2 underline">re-check</button>
        </div>
      )}

      {run && !review && !isTerminal(run.status) && !busy && (
        <div className="px-[22px] py-4 text-[12.5px] text-muted">
          The review is <span className="num text-fg/80">{run.status}</span>. It runs on your own
          machine; nothing happens here until a local bridge claims it.
          <button type="button" onClick={refreshRun} className="ml-2 underline">re-check</button>
        </div>
      )}

      {review && <ReviewBody review={review} run={run} />}
    </section>
  );
}

// SnapshotHeader states WHEN, WHICH GENERATION and WHICH BUILD the evidence came from, plus the
// per-source state. `unavailable`, `stale` and a measured value are three different answers and are
// shown as three different things.
function SnapshotHeader({ snapshot }) {
  const sources = Object.entries(snapshot.sources || {}).sort(([a], [b]) => a.localeCompare(b));
  return (
    <div className="border-t border-line px-[22px] py-3">
      <div className="flex flex-wrap items-baseline gap-x-5 gap-y-1 text-[12px]">
        <span className="text-muted">
          Snapshot <span className="num text-fg/80">{snapshot.id}</span>
        </span>
        <span className="text-muted">
          Taken <span className="num text-fg/80">{snapshot.asOf}</span>
        </span>
        <span className="text-muted">
          Generation <span className="num text-fg/80">{snapshot.generation}</span>
        </span>
        <span className="text-muted">
          Deployment revision <span className="num text-fg/80">{snapshot.revision}</span>
        </span>
      </div>
      {sources.length > 0 && (
        <div className="mt-2 flex flex-wrap gap-1.5">
          {sources.map(([name, src]) => {
            const meta = SOURCE_LABELS[src.state] || SOURCE_LABELS.unavailable;
            return (
              <span key={name} title={`${meta.help}${src.reason ? ` — ${src.reason}` : ""}`}>
                <ToneTag tone={meta.tone}>{name}: {meta.label}</ToneTag>
              </span>
            );
          })}
        </div>
      )}
    </div>
  );
}

function ReviewBody({ review, run }) {
  const action = run?.actionability;
  return (
    <div className="flex flex-col gap-4 border-t border-line px-[22px] py-4">
      {/* THE THREE READINGS, SEPARATE BY CONSTRUCTION. */}
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        <StatusCard
          question="Has the paper experiment been measured?"
          verdict={review.paperStatus}
          labels={PAPER_LABELS}
        />
        <StatusCard
          question="What did the evaluator establish about the candidate strategy?"
          verdict={review.candidateStatus}
          labels={CANDIDATE_LABELS}
        >
          <EvaluatorReadout
            result={review.evaluatorResult}
            validity={review.evidenceValidity}
          />
        </StatusCard>
        <StatusCard
          question="Can this deployment keep honest score?"
          verdict={review.operationalStatus}
          labels={OPERATIONAL_LABELS}
        />
      </div>

      <div className="rounded-md bg-panel2/50 px-3.5 py-3 text-[13px] leading-[1.6] text-fg/90">
        {review.summary}
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <ClaimList
          title="Blockers" tone="down" items={review.blockers}
          empty="No blockers were recorded."
        />
        <ClaimList
          title="Unknowns" tone="warn" items={review.unknowns}
          empty="Nothing was recorded as unknown."
        />
      </div>

      {review.contradictions?.length > 0 && (
        <ClaimList
          title="Contradictions" tone="warn" items={review.contradictions}
          empty="None."
        />
      )}

      {review.nextChecks?.length > 0 && (
        <div>
          <div className="label-mono text-muted">
            Next checks <span className="num">({review.nextChecks.length})</span>
          </div>
          <ul className="mt-1.5 flex flex-col gap-2">
            {review.nextChecks.map((c, i) => (
              <li key={i} className="text-[12.5px] leading-[1.55]">
                <div className="flex flex-wrap items-baseline gap-1.5">
                  <ToneTag tone={CHECK_TONE[c.priority] || "outline"}>{c.priority}</ToneTag>
                  <span className="min-w-0 text-fg/90">{c.statement}</span>
                </div>
                {c.evidencePaths?.length > 0 && (
                  <div className="mt-0.5 font-mono text-[10.5px] leading-snug text-muted/60">
                    {c.evidencePaths.join(" · ")}
                  </div>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}

      {/* Provenance of the review itself, and the NO_SIGNAL block the run view serves on every run
          of every workflow. A completed review is still NO_SIGNAL / NO_ACTION, and it says so. */}
      <div className="border-t border-line pt-3 text-[11px] leading-relaxed text-muted/70">
        <div>
          Snapshot <span className="num">{review.snapshotId}</span> · generation{" "}
          <span className="num">{review.generation}</span> · cutoff{" "}
          <span className="num">{review.cutoff}</span> · workflow{" "}
          <span className="num">{review.workflowVersion}</span> · produced{" "}
          <span className="num">{review.producedAt}</span>
        </div>
        {review.degraded?.length > 0 && (
          <div className="mt-1 text-warn">Degraded: {review.degraded.join("; ")}</div>
        )}
        {action && (
          <div className="mt-1.5">
            <strong className="text-muted">
              {action.evidenceState} / {action.action}.
            </strong>{" "}
            {action.note}
          </div>
        )}
      </div>
    </div>
  );
}
