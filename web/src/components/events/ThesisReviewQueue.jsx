import { useCallback, useEffect, useState } from "react";
import { listThesisEventReviews } from "../../lib/portfolioApi.js";
import { AuthRequiredError } from "../../lib/api.js";
import { Badge, EmptyState, Panel } from "../ui/index.js";

// ThesisReviewQueue — Phase 3: the short list of "you should look at this" items, built by joining
// the stored calendar to the conditions the user themselves wrote on an active thesis.
//
// THE BEARING VOCABULARY IS THE POINT OF THIS COMPONENT.
//
//   context  — this event is about the same subject as something you named.
//   unclear  — this is a high-importance event on your company that matches nothing you named.
//
// `supports` and `contradicts` exist in the server's vocabulary and are NOT produced by
// deterministic matching, because term overlap establishes subject, not direction. If one ever
// appears here it came from a bounded model hypothesis that cited its evidence — so it is rendered
// with that provenance visible rather than as a plain verdict.
//
// Nothing here changes a thesis. A review item is a pointer to work the user might do.

const BEARING = {
  context: { tone: "info", label: "Context", note: "touches something you wrote" },
  unclear: { tone: "outline", label: "Unclear", note: "no condition matched" },
  supports: { tone: "ok", label: "Hypothesis: supports", note: "model hypothesis — check the evidence" },
  contradicts: { tone: "warn", label: "Hypothesis: challenges", note: "model hypothesis — check the evidence" },
};

function when(iso) {
  if (!iso) return "—";
  return iso.replace("T", " ").replace(":00Z", "Z");
}

function ReviewRow({ review }) {
  const bearing = BEARING[review.bearing] || BEARING.unclear;
  return (
    <li className="flex flex-col gap-1.5 border-t border-line/60 px-4 py-3.5 first:border-t-0">
      <div className="flex flex-wrap items-center gap-2">
        <a className="num text-[12.5px] font-semibold text-fg hover:text-accent" href={review.researchDeepLink}>
          {review.ticker}
        </a>
        <a className="text-[13px] text-fg hover:text-accent" href={review.event.deepLink}>
          {review.event.title}
        </a>
        <Badge variant={bearing.tone}>{bearing.label}</Badge>
        <Badge variant="dim">{review.relationship}</Badge>
        <span className="num text-[11.5px] text-muted">{when(review.event.scheduledAt)}</span>
      </div>

      {review.matchedCondition ? (
        <p className="text-[12px] leading-relaxed text-muted">
          Matches a <span className="text-fg">{review.matchedCondition.field}</span> you wrote:
          “{review.matchedCondition.text}”
          {review.matchedCondition.terms?.length > 0 && (
            <span className="text-dim"> · on “{review.matchedCondition.terms.join(", ")}”</span>
          )}
        </p>
      ) : (
        <p className="text-[12px] leading-relaxed text-muted">{review.bearingReason}</p>
      )}

      <p className="text-[11px] leading-relaxed text-dim">
        {bearing.note} · {review.reason}
      </p>

      <div className="flex flex-wrap items-center gap-3 text-[11px]">
        <a className="text-muted underline decoration-dotted hover:text-fg" href={review.thesisDeepLink}>
          Open thesis
        </a>
        {review.event.sourceUrl && (
          <a
            className="text-muted underline decoration-dotted hover:text-fg"
            href={review.event.sourceUrl}
            target="_blank"
            rel="noreferrer noopener"
          >
            {review.event.source} source
          </a>
        )}
        <span className="text-dim">{review.version}</span>
      </div>
    </li>
  );
}

export default function ThesisReviewQueue() {
  const [state, setState] = useState({ status: "loading", data: null, error: "" });

  const load = useCallback(async () => {
    setState((prev) => ({ ...prev, status: "loading" }));
    try {
      const data = await listThesisEventReviews();
      setState({ status: "ready", data, error: "" });
    } catch (err) {
      if (err instanceof AuthRequiredError) {
        setState({ status: "guest", data: null, error: "" });
        return;
      }
      setState({ status: "error", data: null, error: err?.message || "unavailable" });
    }
  }, []);

  // One read when the section mounts. No interval: a review queue that polls is a notification
  // system, and this is a list you look at.
  useEffect(() => {
    load();
  }, [load]);

  const reviews = state.data?.reviews || [];
  const degraded = state.data?.degraded || [];

  return (
    <Panel
      title="Review queue"
      badges={<Badge variant="accent">deterministic match</Badge>}
      flush
    >
      {state.status === "loading" && (
        <p className="px-4 py-4 text-[12px] text-muted">Checking the calendar against your theses…</p>
      )}

      {state.status === "guest" && (
        <div className="px-4 py-4">
          <EmptyState title="Sign in to see your review queue">
            The queue is built from your own theses, so it is a private, per-user record.
          </EmptyState>
        </div>
      )}

      {state.status === "error" && (
        <div className="px-4 py-4">
          <EmptyState title="Review queue unavailable">{state.error}</EmptyState>
        </div>
      )}

      {degraded.length > 0 && (
        <p className="border-b border-line/60 px-4 py-2.5 text-[11.5px] text-warn">
          Degraded: {degraded.join(", ")}. An empty queue here is not evidence that nothing needs
          reviewing.
        </p>
      )}

      {state.status === "ready" && reviews.length === 0 && degraded.length === 0 && (
        <div className="px-4 py-4">
          <EmptyState title="Nothing to review">
            No scheduled event in the window touches a condition on your active theses.
          </EmptyState>
        </div>
      )}

      <ul>
        {reviews.map((review) => (
          <ReviewRow key={`${review.thesisId}-${review.eventId}`} review={review} />
        ))}
      </ul>

      {reviews.length > 0 && (
        <p className="border-t border-line px-4 py-2.5 text-[10.5px] leading-relaxed text-dim">
          Matching establishes that an event is about the same subject as something you wrote. It
          does not decide whether the event supports or challenges your thesis, and nothing here
          changes a thesis — that stays yours.
        </p>
      )}
    </Panel>
  );
}
