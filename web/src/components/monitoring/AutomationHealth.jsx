import { useCallback, useEffect, useState } from "react";
import { fetchAutomationStatus } from "../../lib/api.js";
import { Badge, Button, EmptyState, Panel } from "../ui/index.js";
import { cx } from "../../lib/cx.js";

// AutomationHealth — Phase 1's operational surface, rendered inside Settings → Preferences.
//
// It answers four operator questions and nothing else: is this lane switched on, when did it last
// succeed, when did it last fail, and how deep is its queue. It adds no primary destination — the
// nine of §9.34 are untouched — and it deliberately offers NO "run now" button:
//
//   Starting a background lane from a page would be a job caused by a UI event, and two of the five
//   lanes call the model. Invariant #4 forbids that without qualification. The production clock is
//   a separate model-free process; this panel only reads what runs recorded.
//
// It also refreshes only when the user asks. A poll here would be a timer in the client driving a
// server read every N seconds, which is the habit the rest of this app has already given up.

const TERMINAL_TONE = {
  success: "ok",
  degraded: "warn",
  failure: "down",
  skipped: "dim",
  running: "info",
};

const QUOTA_TONE = {
  ok: "ok",
  warning: "warn",
  exhausted: "down",
  blocked: "warn",
};

function relative(seconds) {
  if (seconds === null || seconds === undefined) return "never";
  const s = Math.max(0, Math.floor(seconds));
  if (s < 90) return `${s}s ago`;
  if (s < 5400) return `${Math.floor(s / 60)}m ago`;
  if (s < 172800) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

function cadence(seconds) {
  if (!seconds) return "—";
  if (seconds % 3600 === 0) return `${seconds / 3600}h`;
  if (seconds % 60 === 0) return `${seconds / 60}m`;
  return `${seconds}s`;
}

function LaneRow({ lane }) {
  const tone = lane.running ? "info" : TERMINAL_TONE[lane.lastStatus] || "dim";
  return (
    <div className="flex flex-col gap-2 border-b border-line/60 px-[22px] py-4 last:border-b-0 sm:flex-row sm:items-start sm:justify-between">
      <div className="min-w-0">
        <div className="flex items-center gap-2">
          <span className="num text-[13px] font-semibold text-fg">{lane.lane}</span>
          {lane.enabled ? (
            <Badge variant="ok">enabled</Badge>
          ) : (
            <Badge variant="outline">disabled</Badge>
          )}
          {lane.running && <Badge variant="info" dot pulse>running</Badge>}
        </div>
        <p className="mt-1 max-w-[420px] text-[12px] leading-relaxed text-muted">{lane.summary}</p>
        {!lane.enabled && lane.laneFlagEnv && (
          <p className="mt-1 text-[11.5px] text-dim">
            Set <span className="num text-muted">AUTOMATION_ENABLED</span> and{" "}
            <span className="num text-muted">{lane.laneFlagEnv}</span> to <span className="num text-muted">true</span>,
            {lane.runner === "events" ? (
              <> then the production clock can pick up the lane when it is due.</>
            ) : (
              <> then run <span className="num text-muted">make automate-once</span> explicitly.</>
            )}
          </p>
        )}
        {lane.lastError && (
          <p className="mt-1 max-w-[420px] break-words text-[11.5px] text-down">{lane.lastError}</p>
        )}
      </div>

      <dl className="grid shrink-0 grid-cols-2 gap-x-6 gap-y-1 text-[11.5px] sm:grid-cols-1 sm:text-right">
        <div className="flex gap-2 sm:justify-end">
          <dt className="text-dim">last outcome</dt>
          <dd className={cx("num", tone === "down" ? "text-down" : tone === "warn" ? "text-warn" : "text-muted")}>
            {lane.lastStatus || "—"}
          </dd>
        </div>
        <div className="flex gap-2 sm:justify-end">
          <dt className="text-dim">last success</dt>
          <dd className="num text-muted">{relative(lane.secondsSinceLastSuccess)}</dd>
        </div>
        <div className="flex gap-2 sm:justify-end">
          <dt className="text-dim">last failure</dt>
          <dd className="num text-muted">{lane.lastFailureAt || "none"}</dd>
        </div>
        <div className="flex gap-2 sm:justify-end">
          <dt className="text-dim">cadence</dt>
          <dd className="num text-muted">{cadence(lane.intervalSeconds)}</dd>
        </div>
        <div className="flex gap-2 sm:justify-end">
          <dt className="text-dim">next eligible</dt>
          <dd className="num text-muted">{lane.nextEligibleAt || "now"}</dd>
        </div>
        {lane.consecutiveFailures > 0 && (
          <div className="flex gap-2 sm:justify-end">
            <dt className="text-dim">consecutive failures</dt>
            <dd className="num text-down">{lane.consecutiveFailures}</dd>
          </div>
        )}
      </dl>
    </div>
  );
}

function QuotaRow({ quota }) {
  return (
    <div className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-3 border-b border-line/60 py-2 last:border-b-0 sm:grid-cols-[minmax(0,1fr)_90px_90px_auto]">
      <span className="num truncate text-[12px] text-fg">{quota.provider}</span>
      <span className="num text-right text-[11.5px] text-muted">
        {quota.calls} / {quota.limit}
      </span>
      <span className="hidden text-right text-[11.5px] text-dim sm:block">
        {quota.remaining} left
      </span>
      <Badge variant={QUOTA_TONE[quota.status] || "dim"}>{quota.status}</Badge>
    </div>
  );
}

export default function AutomationHealth() {
  const [state, setState] = useState({ status: "loading", data: null, error: "" });

  const load = useCallback(async () => {
    setState((prev) => ({ ...prev, status: "loading" }));
    try {
      const data = await fetchAutomationStatus();
      setState({ status: "ready", data, error: "" });
    } catch (err) {
      setState({ status: "error", data: null, error: err?.message || "unavailable" });
    }
  }, []);

  // One read on mount, and one per explicit refresh. No interval.
  useEffect(() => {
    load();
  }, [load]);

  const data = state.data;
  const lanes = data?.lanes || [];
  const runs = data?.recentRuns || [];
  const degraded = data?.degraded || [];
  const quotas = data?.providerQuotas || [];
  const quotaSummary = data?.quotaSummary;
  const productionScheduler = data?.productionScheduler;

  return (
    <Panel
      title="Automation health"
      badges={
        <div className="flex items-center gap-2">
          {data?.automationEnabled ? (
            <Badge variant="ok">automation on</Badge>
          ) : (
            <Badge variant="outline">automation off</Badge>
          )}
          {productionScheduler?.enabled ? (
            <Badge variant="info">production clock on</Badge>
          ) : (
            <Badge variant="outline">production clock off</Badge>
          )}
        </div>
      }
      actions={
        <Button variant="ghost" onClick={load} disabled={state.status === "loading"}>
          {state.status === "loading" ? "Refreshing…" : "Refresh"}
        </Button>
      }
      flush
    >
      {state.status === "error" && (
        <EmptyState title="Automation health unavailable">{state.error}</EmptyState>
      )}

      {data?.signedOut && (
        <EmptyState title="Sign in to view automation health">
          Lane state describes what this deployment runs in the background, so it is not public.
        </EmptyState>
      )}

      {degraded.length > 0 && (
        <p className="border-b border-line/60 px-[22px] py-3 text-[12px] text-warn">
          Degraded: {degraded.join(", ")}. Lane state below may be incomplete — it is not evidence
          that nothing is wrong.
        </p>
      )}

      {lanes.map((lane) => (
        <LaneRow key={lane.lane} lane={lane} />
      ))}

      {state.status === "ready" && !data?.signedOut && lanes.length === 0 && (
        <EmptyState title="No lanes reported">
          The events service returned no lane state. Nothing has been scheduled.
        </EmptyState>
      )}

      {quotas.length > 0 && (
        <div className="border-t border-line px-[22px] py-4">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div className="label-mono text-muted">Provider budget monitor</div>
            {quotaSummary?.attentionRequired ? (
              <Badge variant="warn">attention</Badge>
            ) : (
              <Badge variant="ok">within events budget</Badge>
            )}
          </div>
          <p className="mt-1 text-[11.5px] leading-relaxed text-dim">
            {quotaSummary?.scopeNote || "Events-ingestion provider calls only."}
            {quotaSummary?.resetAt ? ` Resets ${quotaSummary.resetAt}.` : ""}
          </p>
          <div className="mt-2">
            {quotas.map((quota) => <QuotaRow key={quota.provider} quota={quota} />)}
          </div>
        </div>
      )}

      {runs.length > 0 && (
        <div className="border-t border-line px-[22px] py-4">
          <div className="mb-2 label-mono text-muted">Recent runs</div>
          <ul className="flex flex-col gap-1">
            {runs.slice(0, 8).map((run) => (
              <li key={run.id} className="flex flex-wrap items-center gap-2 text-[11.5px]">
                <span className="num text-muted">{run.startedAt}</span>
                <span className="num text-fg">{run.lane}</span>
                <Badge variant={TERMINAL_TONE[run.status] || "dim"}>{run.status}</Badge>
                <span className="text-dim">
                  read {run.recordsRead} · wrote {run.recordsWritten} · skipped {run.recordsSkipped}
                  {run.queueDepth !== null && run.queueDepth !== undefined
                    ? ` · queue ${run.queueDepth}`
                    : ""}
                </span>
                {run.lastError && <span className="text-down">{run.lastError}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}

      <p className="border-t border-line px-[22px] py-3 text-[11.5px] leading-relaxed text-dim">
        The production clock can start only the model-free ingest and reaction lanes. Model lanes
        still require the explicit <span className="num text-muted">make automate-once</span>{" "}
        operator command. Nothing on this page can start a lane.
      </p>
    </Panel>
  );
}
