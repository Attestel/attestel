import { useState } from "react";
import { cx } from "../../lib/cx.js";
import { Tag } from "../terminal/bits.jsx";
import {
  FINDING_KIND,
  TASK_BY_ID,
  fmtReviewTime,
  rateReview,
  setCitationFeedback,
} from "../../lib/lensApi.js";
import { Icon } from "../shell/icons.jsx";

// ReviewCard — one saved lens review.
//
// The card's job is TRACEABILITY, not decoration. Three things are always visible and never
// collapsed together:
//
//   1. WHO SAID THIS. The whole card is model output, framed in the app's model-provenance colour
//      and labelled with the lens that produced it and its version. It sits apart from the user's
//      own writing wherever it is rendered.
//   2. CITED vs INFERRED. Every finding carries its kind as a glyph AND a word, plus the evidence it
//      cites. A cited fact links to the record; an inference says so in plain language. Colour is
//      never the only carrier.
//   3. WHAT IT COULD NOT SEE. Missing evidence and open questions are peers of the findings, not a
//      footnote — a review that only shows what it found reads as more complete than it is.
//
// There is deliberately NO "apply to my thesis" button here. Accepting a finding is a separate,
// explicit edit the user makes with the text in front of them (§3.7), which is what the
// `onAcceptAsQuestion` callback opens — it never writes anything itself.

function FindingRow({ finding, evidenceById, onAcceptAsQuestion, feedback, onFeedback }) {
  const kind = FINDING_KIND[finding.kind] || FINDING_KIND.inference;
  const cited = finding.citedEvidenceIds || [];
  const accepted = feedback?.accepted;

  return (
    <li className="flex flex-col gap-1.5 py-2">
      <div className="flex items-start gap-2">
        <span className={cx("mt-px shrink-0 text-[12px]", kind.tone)} aria-hidden="true">
          {kind.glyph}
        </span>
        <div className="min-w-0 flex-1">
          <p className="break-words text-[12.5px] leading-relaxed text-fg/90">{finding.statement}</p>

          <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className={cx("text-[10px] font-bold uppercase tracking-wide", kind.tone)}>
              {kind.label}
            </span>
            <span className="text-[10.5px] text-muted/60">{kind.help}</span>
          </div>

          {cited.length > 0 && (
            <ul className="mt-1.5 flex flex-col gap-1">
              {cited.map((id) => {
                const rec = evidenceById?.[id];
                return (
                  <li key={id} className="flex flex-wrap items-baseline gap-1.5 text-[11.5px]">
                    <span className="text-muted/60" aria-hidden="true">
                      <Icon name="reply" size={13} />
                    </span>
                    <span className="break-words text-muted">
                      {rec?.title || <span className="font-mono text-muted/70">{id}</span>}
                    </span>
                    {rec?.dataState && (
                      <Tag tone={rec.dataState === "synthetic" ? "warn" : "outline"}>{rec.dataState}</Tag>
                    )}
                  </li>
                );
              })}
            </ul>
          )}

          {/* Citation acceptance — the metric this step is measured on, and a real signal from the
              user about whether the evidence actually backs the claim. */}
          {(cited.length > 0 || finding.kind === "inference") && (
            <div className="mt-1.5 flex flex-wrap items-center gap-2">
              {onFeedback && (
                <>
                  <button
                    type="button"
                    onClick={() => onFeedback(finding.id, true)}
                    aria-pressed={accepted === true}
                    className={cx(
                      "rounded border px-1.5 py-0.5 text-[10.5px] font-semibold transition-colors",
                      accepted === true
                        ? "border-accent/60 bg-accent/15 text-accent"
                        : "border-line text-muted hover:text-fg"
                    )}
                  >
                    Sound
                  </button>
                  <button
                    type="button"
                    onClick={() => onFeedback(finding.id, false)}
                    aria-pressed={accepted === false}
                    className={cx(
                      "rounded border px-1.5 py-0.5 text-[10.5px] font-semibold transition-colors",
                      accepted === false
                        ? "border-down/60 bg-down/10 text-down"
                        : "border-line text-muted hover:text-fg"
                    )}
                  >
                    Doesn't hold
                  </button>
                </>
              )}
              {onAcceptAsQuestion && (
                <button
                  type="button"
                  onClick={() => onAcceptAsQuestion(finding.statement)}
                  className="rounded border border-line px-1.5 py-0.5 text-[10.5px] font-semibold text-info transition-colors hover:border-info/60"
                >
                  Add as a next question…
                </button>
              )}
            </div>
          )}
        </div>
      </div>
    </li>
  );
}

function Bullets({ title, items, tone = "text-muted" }) {
  if (!items?.length) return null;
  return (
    <div>
      <p className="text-[10.5px] font-semibold uppercase tracking-wide text-muted">{title}</p>
      <ul className="mt-1 flex flex-col gap-0.5">
        {items.map((x, i) => (
          <li key={i} className={cx("break-words text-[12px] leading-relaxed", tone)}>
            · {x}
          </li>
        ))}
      </ul>
    </div>
  );
}

export default function ReviewCard({
  review,
  evidenceById = {},
  onAcceptAsQuestion,
  defaultOpen = true,
  compact = false,
}) {
  const [open, setOpen] = useState(defaultOpen);
  const [rating, setRating] = useState(review.rating || null);
  const [feedback, setFeedback] = useState(() =>
    Object.fromEntries((review.citationFeedback || []).map((f) => [f.findingId, f]))
  );
  const [err, setErr] = useState(null);

  const task = TASK_BY_ID[review.task];
  const findings = review.findings || [];
  const facts = findings.filter((f) => f.kind === "fact").length;

  const rate = (value) => {
    const next = rating === value ? null : value;
    setRating(next);
    rateReview(review.id, next).catch((e) => setErr(e.message));
  };

  const onFeedback = (findingId, accepted) => {
    const cur = feedback[findingId];
    const next = { ...feedback };
    if (cur && cur.accepted === accepted) delete next[findingId];
    else next[findingId] = { findingId, accepted };
    setFeedback(next);
    setCitationFeedback(review.id, Object.values(next)).catch((e) => setErr(e.message));
  };

  return (
    <section
      aria-label={`${review.lens?.name || "Lens"} review`}
      className="overflow-hidden rounded-lg border border-llm/40 bg-llm/[0.04]"
    >
      <div className="flex flex-wrap items-center gap-2 border-b border-llm/25 px-3 py-2">
        <Tag tone="llm">model review</Tag>
        <span className="text-[12px] font-semibold text-fg">{review.lens?.name || review.lens?.id}</span>
        {task && <span className="text-[11px] text-muted">{task.label}</span>}
        {review.stub && (
          <Tag tone="warn">offline stub</Tag>
        )}
        {review.retried && <Tag tone="outline">retried</Tag>}
        <span className="ml-auto flex items-center gap-2">
          <span className="text-[10.5px] text-muted/60">{fmtReviewTime(review.savedAt || review.createdAt)}</span>
          <button
            type="button"
            onClick={() => setOpen((o) => !o)}
            aria-expanded={open}
            className="rounded border border-line px-1.5 py-0.5 text-[10.5px] font-semibold text-muted hover:text-fg"
          >
            {open ? "hide" : "show"}
          </button>
        </span>
      </div>

      {/* A stub is announced, not disguised: an offline review contains no analysis and the card
          must not let it read like one. */}
      {review.stub && (
        <p className="border-b border-llm/20 bg-warn/[0.06] px-3 py-2 text-[11.5px] leading-relaxed text-warn">
          The model was unavailable, so this review contains no analysis — it only lists the evidence
          that was attached. Run it again when the service is back.
        </p>
      )}

      {open && (
        <div className="flex flex-col gap-3 px-3 py-3">
          {!compact && (
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-muted">
              <span>
                <span className="font-mono text-fg">{findings.length}</span> findings
                {findings.length > 0 && (
                  <span className="text-muted/70">
                    {" "}
                    ({facts} cited, {findings.length - facts} inferred)
                  </span>
                )}
              </span>
              {review.lensConfidence != null && (
                <span>
                  Lens confidence <span className="font-mono text-fg">{review.lensConfidence}</span>
                  <span className="text-muted/70">/100</span>
                </span>
              )}
              {review.modelUsed && (
                <span className="font-mono text-[10.5px] text-muted/60">{review.modelUsed}</span>
              )}
              {review.lens?.version && (
                <span className="text-[10.5px] text-muted/50">lens {review.lens.version.slice(0, 10)}</span>
              )}
            </div>
          )}

          {findings.length > 0 ? (
            <ul className="flex flex-col divide-y divide-llm/15">
              {findings.map((f) => (
                <FindingRow
                  key={f.id}
                  finding={f}
                  evidenceById={evidenceById}
                  onAcceptAsQuestion={onAcceptAsQuestion}
                  feedback={feedback[f.id]}
                  onFeedback={onFeedback}
                />
              ))}
            </ul>
          ) : (
            <p className="text-[12px] text-muted">This review produced no findings.</p>
          )}

          <Bullets title="Agreements" items={review.agreements} tone="text-up/90" />
          <Bullets title="Disagreements" items={review.disagreements} tone="text-down/90" />
          <Bullets title="Missing evidence" items={review.missingEvidence} />
          <Bullets title="Open questions" items={review.openQuestions} />

          {review.chartContext && (
            <div>
              <p className="text-[10.5px] font-semibold uppercase tracking-wide text-muted">
                Chart context used
                <span className="ml-1.5 font-normal normal-case text-muted/60">
                  computed by the server, not the model
                </span>
              </p>
              <p className="mt-1 text-[11.5px] text-muted">
                {review.chartContext.timeframe} · support{" "}
                <span className="font-mono text-fg">
                  {[].concat(review.chartContext.support ?? []).join(", ") || "n/a"}
                </span>{" "}
                · resistance{" "}
                <span className="font-mono text-fg">
                  {[].concat(review.chartContext.resistance ?? []).join(", ") || "n/a"}
                </span>
                {review.chartContext.dataState === "synthetic" && (
                  <span className="ml-1.5 text-warn">— synthetic data, not real prices</span>
                )}
              </p>
            </div>
          )}

          {review.committeeRef && (
            <p className="text-[11px] text-muted/70">
              Full debate and the deterministic consensus are kept in the committee snapshot{" "}
              <span className="font-mono">{review.committeeRef.id}</span>.
            </p>
          )}

          <div className="flex flex-wrap items-center gap-2 border-t border-llm/15 pt-2">
            <span className="text-[10.5px] uppercase tracking-wide text-muted">Was this useful?</span>
            <button
              type="button"
              onClick={() => rate("useful")}
              aria-pressed={rating === "useful"}
              className={cx(
                "rounded border px-2 py-0.5 text-[10.5px] font-semibold transition-colors",
                rating === "useful" ? "border-accent/60 bg-accent/15 text-accent" : "border-line text-muted hover:text-fg"
              )}
            >
              Useful
            </button>
            <button
              type="button"
              onClick={() => rate("not_useful")}
              aria-pressed={rating === "not_useful"}
              className={cx(
                "rounded border px-2 py-0.5 text-[10.5px] font-semibold transition-colors",
                rating === "not_useful" ? "border-down/60 bg-down/10 text-down" : "border-line text-muted hover:text-fg"
              )}
            >
              Not useful
            </button>
            {err && <span className="text-[10.5px] text-down">{err}</span>}
            <span className="ml-auto text-[10px] text-muted/50">{review.disclaimer}</span>
          </div>
        </div>
      )}
    </section>
  );
}
