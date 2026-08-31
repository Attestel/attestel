import { useCallback, useEffect, useState } from "react";
import { EmptyState, SkeletonText } from "../ui/index.js";
import { Micro, Tag } from "../terminal/bits.jsx";
import { relFromUnix } from "../../lib/format.js";
import {
  fetchChanges,
  summarizeChanges,
  markReviewed,
  kindLabel,
  bearingTone,
  AuthRequiredError,
} from "../../lib/monitoringApi.js";

// WhatChangedPanel — "What changed since your last review?" (Step 08, PARALLEL_CONTRACTS.md §4.4).
//
// THE LIST IS THE ANSWER. The deterministic change set renders first and always; the optional model
// summary is a button the user presses, sits BELOW the list, and is labelled as inference. That
// ordering is the product position: the user should be able to review without ever reading a
// generated sentence, and should never be shown a narrative they cannot trace to a row.
//
// "Nothing has changed" is a real, useful answer and is rendered as one. Filling that state with
// synthesized commentary would teach the user to distrust the times it isn't empty.
//
// Marking a review done advances the Step 04 checkpoint (§1.8) so the NEXT comparison has a stable
// boundary. That is the only write this panel performs; it never edits a thesis field.
export default function WhatChangedPanel({ thesisId, onOpenEvidence }) {
  const [set, setSet] = useState(null);
  const [status, setStatus] = useState("loading");
  const [summary, setSummary] = useState(null);
  const [summarizing, setSummarizing] = useState(false);
  const [marking, setMarking] = useState(false);

  const load = useCallback(() => {
    if (!thesisId) return undefined;
    let alive = true;
    setStatus("loading");
    setSummary(null); // a summary describes ONE change set; never carry it across a reload
    fetchChanges(thesisId)
      .then((res) => {
        if (!alive) return;
        setSet(res);
        setStatus("ready");
      })
      .catch((err) => {
        if (!alive) return;
        setStatus(err instanceof AuthRequiredError ? "guest" : "down");
      });
    return () => {
      alive = false;
    };
  }, [thesisId]);

  useEffect(load, [load]);

  const items = set?.items || [];

  // On-demand ONLY — bound to this click and nothing else.
  const onSummarize = async () => {
    setSummarizing(true);
    try {
      setSummary(await summarizeChanges(thesisId));
    } catch {
      setSummary({ error: true });
    } finally {
      setSummarizing(false);
    }
  };

  const onMarkReviewed = async () => {
    setMarking(true);
    try {
      await markReviewed(thesisId, items.length);
      load(); // the boundary moved, so the set is now (correctly) empty
    } catch {
      /* fail-soft: the list stays as it is and the user can retry */
    } finally {
      setMarking(false);
    }
  };

  if (status === "guest") return null;

  return (
    <section className="overflow-hidden rounded-xl border border-line bg-panel">
      <div className="flex flex-wrap items-center gap-2.5 border-b border-line px-[22px] py-3">
        <span className="text-[14px] font-semibold text-fg">What changed since your last review?</span>
        {status === "ready" && (
          <>
            {items.length > 0 && <Tag tone="warn">{items.length}</Tag>}
            <Tag tone="outline">
              {set.since?.basis === "checkpoint"
                ? `since ${relFromUnix(set.since.at)}`
                : "since you created this thesis"}
            </Tag>
          </>
        )}
      </div>

      {status === "loading" && (
        <div className="px-[22px] py-4">
          <SkeletonText lines={3} />
        </div>
      )}

      {status === "down" && (
        <div className="px-[22px] py-4 text-[12.5px] leading-relaxed text-muted">
          The change history is unavailable right now. Nothing has been lost — this view is computed
          from your stored records each time it is opened.
        </div>
      )}

      {/* The honest empty state. */}
      {status === "ready" && set.empty && (
        <div className="px-[22px] py-4">
          <EmptyState title="Nothing has changed since your last review">
            No thesis edits, evidence changes, saved reviews, or triggered alerts since{" "}
            {set.since?.basis === "checkpoint" ? "your last review" : "you created this thesis"}.
          </EmptyState>
        </div>
      )}

      {status === "ready" && items.length > 0 && (
        <ul className="divide-y divide-line">
          {items.map((item) => (
            <ChangeRow key={item.id} item={item} onOpenEvidence={onOpenEvidence} />
          ))}
        </ul>
      )}

      {/* A source that could not be read is NAMED. A shorter list must never pass for a quiet one. */}
      {status === "ready" && set.degraded?.length > 0 && (
        <div className="border-t border-line px-[22px] py-2.5 text-[11.5px] text-muted">
          Some sources could not be read this time ({set.degraded.join(", ")}), so this list may be
          incomplete.
        </div>
      )}

      {status === "ready" && set.truncated && (
        <div className="border-t border-line px-[22px] py-2.5 text-[11.5px] text-muted">
          Showing the most recent {items.length} changes — there are more since your last review.
        </div>
      )}

      {status === "ready" && !set.empty && (
        <div className="flex flex-wrap items-center gap-2 border-t border-line px-[22px] py-3">
          <button
            type="button"
            onClick={onMarkReviewed}
            disabled={marking}
            className="rounded-full border border-accent/50 bg-accent/10 px-4 py-2 text-[12.5px] font-semibold text-accent transition-colors hover:bg-accent/16 disabled:opacity-50"
          >
            {marking ? "Saving…" : "Mark reviewed"}
          </button>
          <button
            type="button"
            onClick={onSummarize}
            disabled={summarizing}
            className="rounded-full border border-line2 px-4 py-2 text-[12.5px] font-semibold text-fg/90 transition-colors hover:border-accent/60 disabled:opacity-50"
          >
            {summarizing ? "Summarizing…" : "Summarize these changes"}
          </button>
          <span className="text-[10.5px] text-muted/70">
            the list above is the record — a summary only compresses it
          </span>
        </div>
      )}

      {summary && <SummaryBlock summary={summary} />}
    </section>
  );
}

function ChangeRow({ item, onOpenEvidence }) {
  const clickable = item.kind === "evidence_linked" || item.kind === "evidence_unlinked";
  return (
    <li className="px-[22px] py-3">
      <div className="flex flex-wrap items-center gap-2">
        <Tag tone="outline">{kindLabel(item.kind)}</Tag>
        {/* A bearing is shown ONLY when the server determined one deterministically. Its absence is
            not rendered as "neutral" — an unmarked row simply makes no claim. */}
        {item.bearing && <Tag tone={bearingTone(item.bearing)}>{item.bearing}</Tag>}
        {/* Invariant #1's visible half: synthetic never renders as if it were market data. */}
        {item.dataState === "synthetic" && <Tag tone="warn">synthetic data</Tag>}
        {item.dataState === "seed" && <Tag tone="outline">seed data</Tag>}
        <span className="num text-[10.5px] text-muted/70">{relFromUnix(item.at)}</span>
      </div>
      <p className="mt-1 text-[12.5px] leading-relaxed text-fg/85">{item.summary}</p>
      <div className="mt-1 flex flex-wrap items-center gap-2">
        {/* Every row names the store it came from, so any line can be traced to a durable record. */}
        <span className="num text-[10px] text-muted/55">{item.sourceRef?.store}</span>
        {clickable && onOpenEvidence && (
          <button
            type="button"
            onClick={() => onOpenEvidence(item)}
            className="text-[11.5px] text-accent transition-[filter] hover:brightness-125"
          >
            Review evidence →
          </button>
        )}
      </div>
    </li>
  );
}

function SummaryBlock({ summary }) {
  if (summary.error) {
    return (
      <div className="border-t border-line px-[22px] py-3 text-[12px] text-muted">
        The summary could not be generated. The change list above is unaffected.
      </div>
    );
  }
  return (
    <div className="border-t border-line bg-panel2/20 px-[22px] py-3">
      <div className="flex flex-wrap items-center gap-2">
        <Micro>Summary</Micro>
        {/* Always marked. This is a reading OF the list, not an additional observation. */}
        <Tag tone="outline">inference</Tag>
        {summary.modelUsed?.startsWith("stub:") && <Tag tone="warn">offline — computed summary</Tag>}
      </div>
      <p className="mt-1.5 text-[12.5px] leading-relaxed text-fg/80">{summary.summary}</p>
      {summary.warnings?.length > 0 && (
        <ul className="mt-1.5 space-y-0.5">
          {summary.warnings.map((wmsg, i) => (
            <li key={i} className="text-[11px] text-muted/80">
              {wmsg}
            </li>
          ))}
        </ul>
      )}
      <p className="mt-1.5 text-[10.5px] leading-relaxed text-muted/70">{summary.disclaimer}</p>
    </div>
  );
}
