import { useCallback, useEffect, useState } from "react";
import { Button, EmptyState, SkeletonText, useToast } from "../ui/index.js";
import { Micro, Tag } from "../terminal/bits.jsx";
import { useAuth } from "../../auth/AuthContext.jsx";
import { AuthRequiredError, decisionErrorMessage, deleteDecision, listDecisions, listOutcomes } from "../../lib/decisionApi.js";
import DecisionCard from "./DecisionCard.jsx";
import RecordDecisionForm from "./RecordDecisionForm.jsx";
import OutcomeReviewForm from "./OutcomeReviewForm.jsx";

// DecisionLog — the shared list-and-record surface, used by both Journal → Decisions and
// Research → Decision log.
//
// One component for both because they are the same records seen from two directions: Journal shows
// every decision you have made, Research shows the ones about the company in front of you. Rendering
// them from one place is what keeps "reviewed / awaiting review" counted identically in both.
//
// The header states review coverage — the share of decisions that later received an outcome review —
// because that is the habit this step exists to build, and an uncounted habit does not form.

export default function DecisionLog({
  ticker = "",
  thesisId = "",
  title,
  note,
  emptyHint,
  allowRecord = true,
  onOpenThesis,
}) {
  const { user, promptSignIn } = useAuth();
  const toast = useToast();

  const [state, setState] = useState({ status: "loading", decisions: [], outcomes: {} });
  const [recording, setRecording] = useState(false);
  const [reviewing, setReviewing] = useState(null); // { decision, existing }

  const load = useCallback(async () => {
    if (!user) {
      setState({ status: "guest", decisions: [], outcomes: {} });
      return;
    }
    setState((s) => ({ ...s, status: "loading" }));
    try {
      const [{ decisions }, outcomes] = await Promise.all([
        listDecisions({ ticker, thesisId }),
        listOutcomes({ ticker, thesisId }),
      ]);
      const byDecision = {};
      for (const o of outcomes) byDecision[o.decisionId] = o;
      setState({ status: "ready", decisions, outcomes: byDecision });
    } catch (err) {
      if (err instanceof AuthRequiredError) {
        setState({ status: "guest", decisions: [], outcomes: {} });
        return;
      }
      setState({ status: "error", decisions: [], outcomes: {}, message: decisionErrorMessage(err) });
    }
  }, [user, ticker, thesisId]);

  useEffect(() => {
    load();
  }, [load]);

  async function remove(decision) {
    const hasReview = Boolean(state.outcomes[decision.id]);
    const warning = hasReview
      ? "Delete this decision AND its outcome review? Both are gone for good."
      : "Delete this decision record? It is gone for good.";
    if (!window.confirm(warning)) return;
    try {
      await deleteDecision(decision.id);
      toast({ tone: "info", title: "Decision deleted" });
      load();
    } catch (err) {
      toast({ tone: "warn", title: decisionErrorMessage(err) });
    }
  }

  if (state.status === "guest") {
    return (
      <div className="px-[22px] py-5">
        <EmptyState title="Sign in to keep a decision record">
          Decisions and reviews are private to your account.{" "}
          <button type="button" onClick={promptSignIn} className="text-accent hover:brightness-125">
            Sign in
          </button>{" "}
          to record and review them.
        </EmptyState>
      </div>
    );
  }

  if (state.status === "error") {
    return (
      <p className="px-[22px] py-4 text-[12.5px] text-muted">
        {state.message || "Your decision log is unavailable — the journal service may be unreachable."}
      </p>
    );
  }

  const reviewedCount = state.decisions.filter((d) => state.outcomes[d.id]).length;

  return (
    <div className="flex flex-col">
      {/* record / review panes take over the top of the log while open */}
      {recording && (
        <div className="border-b border-line bg-panel2/20">
          <div className="flex h-9 items-center gap-2 border-b border-line px-[22px]">
            <Micro>Record a decision</Micro>
          </div>
          <RecordDecisionForm
            ticker={ticker}
            thesisId={thesisId}
            onCancel={() => setRecording(false)}
            onRecorded={() => {
              setRecording(false);
              toast({ tone: "info", title: "Decision recorded" });
              load();
            }}
          />
        </div>
      )}

      {reviewing && (
        <div className="border-b border-line bg-panel2/20">
          <div className="flex h-9 items-center gap-2 border-b border-line px-[22px]">
            <Micro>{reviewing.existing ? "Revise the review" : "Review the outcome"}</Micro>
          </div>
          <OutcomeReviewForm
            decision={reviewing.decision}
            existing={reviewing.existing}
            onCancel={() => setReviewing(null)}
            onSaved={() => {
              setReviewing(null);
              toast({ tone: "info", title: "Review saved" });
              load();
            }}
          />
        </div>
      )}

      {/* the log */}
      <div className="flex h-9 flex-wrap items-center gap-2 border-b border-line bg-panel2/30 px-[22px]">
        <Micro>{title || "Decisions"}</Micro>
        <span className="num text-[10.5px] text-muted/60">{state.decisions.length}</span>
        {state.decisions.length > 0 && (
          <Tag tone={reviewedCount === state.decisions.length ? "accent" : "outline"}>
            {reviewedCount}/{state.decisions.length} reviewed
          </Tag>
        )}
        {allowRecord && !recording && (
          <Button size="xs" variant="secondary" className="ml-auto" onClick={() => setRecording(true)}>
            Record decision
          </Button>
        )}
      </div>

      {note && (
        <p className="border-b border-line px-[22px] py-2.5 text-[11.5px] leading-relaxed text-muted">{note}</p>
      )}

      {state.status === "loading" ? (
        <div className="px-[22px] py-4">
          <SkeletonText lines={4} />
        </div>
      ) : state.decisions.length === 0 ? (
        <div className="px-[22px] py-5">
          <EmptyState title={ticker ? `No decisions recorded for ${ticker}` : "No decisions recorded yet"}>
            {emptyHint ||
              "When you act on a company — or deliberately don't — record why. Later you can review whether the reasoning was sound, separately from whether it made money."}
          </EmptyState>
        </div>
      ) : (
        <ul className="divide-y divide-line">
          {state.decisions.map((d) => (
            <li key={d.id}>
              <DecisionCard
                decision={d}
                outcome={state.outcomes[d.id] || null}
                onOpenThesis={onOpenThesis}
                onReview={(decision) => setReviewing({ decision, existing: null })}
                onRevise={(decision, existing) => setReviewing({ decision, existing })}
                onDelete={remove}
              />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
