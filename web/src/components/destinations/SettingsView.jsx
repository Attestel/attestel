import { SubNav } from "../shell/SubNav.jsx";
import SettingsPanel from "../SettingsPanel.jsx";
import FeedbackPanel from "../FeedbackPanel.jsx";
import AutomationHealth from "../monitoring/AutomationHealth.jsx";
import PredictionAutomation from "../monitoring/PredictionAutomation.jsx";
import EvaluationRunner from "../monitoring/EvaluationRunner.jsx";
import { DestinationHeader } from "../shell/DestinationHeader.jsx";

// SettingsView — the Settings destination: account, experience, data and notification preferences, plus
// Help & feedback.
//
// Feedback is the former top-level destination, moved out of the customer feature list and reached from
// Settings (and from the account menu). The panel itself is unchanged.
//
// Access control is the feedback service's own (D-16, DECIDED 2026-07-31): a guest cannot submit, a
// signed-in tester sees only their own submissions, and the triage surface belongs to the
// FEEDBACK_ADMIN_UIDS allow-list. FeedbackPanel reflects that from the server's response — it does not
// decide it.
export default function SettingsView({
  subview,
  onSubview,
  level,
  onReviewRealityCheck,
  ticker,
  timeframe,
  data,
}) {
  return (
    <div className="flex flex-col gap-3">
      <div className="mx-auto flex w-full max-w-[860px] flex-col gap-4">
        <DestinationHeader view="settings" />
        <SubNav view="settings" subview={subview} onChange={onSubview} level={level} />
      </div>

      {subview === "preferences" && (
        <>
          <SettingsPanel onReviewRealityCheck={onReviewRealityCheck} />
          {/* Phase 1 operational health. It lives here rather than behind a tenth destination:
              the nine of §9.34 are fixed, and background-job health is a settings concern. */}
          <div className="mx-auto w-full max-w-[860px]">
            <AutomationHealth />
          </div>
          <div className="mx-auto w-full max-w-[860px]">
            <PredictionAutomation />
          </div>
          {/* The edge evaluator's in-app trigger. It lives beside the retrain control for the same
              reason: on the shell-less single-container deploy the app is the ONLY operator surface,
              and docs/VALIDATION_AND_GO_LIVE.md §2 needs both halves reachable. `run` is gated on
              the EVAL_ADMIN_UIDS allow-list server-side (empty = nobody); this panel shows the
              server's refusal rather than deciding anything itself. */}
          <div className="mx-auto w-full max-w-[860px]">
            <EvaluationRunner />
          </div>
          <div className="mx-auto w-full max-w-[860px]">
            <EvaluationRunner kind="event" />
          </div>
          <div className="mx-auto w-full max-w-[860px]">
            <EvaluationRunner kind="estimate" />
          </div>
        </>
      )}

      {subview === "help" && <FeedbackPanel ticker={ticker} timeframe={timeframe} data={data} />}
    </div>
  );
}
