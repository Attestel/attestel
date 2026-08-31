import { useCallback, useEffect, useState } from "react";
import { fetchPredictionAutomationStatus } from "../../lib/api.js";
import { Badge, Button, EmptyState, Panel } from "../ui/index.js";

const STATUS_TONE = {
  reserved: "info",
  trained: "info",
  evaluating: "info",
  evaluated: "info",
  shadowing: "info",
  "shadow-complete": "ok",
  "training-failed": "warn",
  "evaluation-failed": "warn",
  "shadow-failed": "warn",
};

const RESULT_TONE = {
  "candidate-ahead": "ok",
  "champion-ahead": "warn",
  tied: "dim",
  unmeasured: "outline",
};

function shortVersion(value) {
  if (!value) return "—";
  return value.length > 18 ? `${value.slice(0, 8)}…${value.slice(-7)}` : value;
}

function percent(value) {
  if (value === null || value === undefined || !Number.isFinite(Number(value))) return "—";
  return `${(Number(value) * 100).toFixed(2)}%`;
}

function verdictFor(trial) {
  return trial?.evaluation?.verdict?.verdict || trial?.evaluation?.run?.report?.verdict || "—";
}

function TrialRow({ trial }) {
  const shadow = trial.shadow || {};
  return (
    <div className="border-b border-line/60 px-[22px] py-4 last:border-b-0">
      <div className="flex flex-wrap items-center gap-2">
        <span className="num text-[12.5px] font-semibold text-fg">
          {trial.ticker} · {trial.timeframe} · H{trial.horizon}
        </span>
        <Badge variant={STATUS_TONE[trial.status] || "outline"}>{trial.status}</Badge>
        <Badge variant={RESULT_TONE[shadow.result] || "outline"}>
          {shadow.result || "unmeasured"}
        </Badge>
      </div>

      <dl className="mt-3 grid grid-cols-2 gap-x-5 gap-y-1 text-[11.5px] sm:grid-cols-4">
        <div>
          <dt className="text-dim">trigger bar</dt>
          <dd className="num text-muted">{trial.triggerBar || "—"}</dd>
        </div>
        <div>
          <dt className="text-dim">data through</dt>
          <dd className="num text-muted">{trial.dataThrough || "—"}</dd>
        </div>
        <div title={trial.championModelVersion || ""}>
          <dt className="text-dim">champion</dt>
          <dd className="num text-muted">{shortVersion(trial.championModelVersion)}</dd>
        </div>
        <div title={trial.candidateModelVersion || ""}>
          <dt className="text-dim">challenger</dt>
          <dd className="num text-muted">{shortVersion(trial.candidateModelVersion)}</dd>
        </div>
        <div>
          <dt className="text-dim">evaluator</dt>
          <dd className="num text-muted">{verdictFor(trial)}</dd>
        </div>
        <div>
          <dt className="text-dim">paired bars</dt>
          <dd className="num text-muted">
            {shadow.pairedBars ?? 0} / {shadow.requiredPairedBars ?? "—"}
          </dd>
        </div>
        <div>
          <dt className="text-dim">challenger net</dt>
          <dd className="num text-muted">{percent(shadow.candidateTotalReturn)}</dd>
        </div>
        <div>
          <dt className="text-dim">champion net</dt>
          <dd className="num text-muted">{percent(shadow.championTotalReturn)}</dd>
        </div>
      </dl>

      {trial.error && <p className="mt-2 break-words text-[11.5px] text-warn">{trial.error}</p>}
    </div>
  );
}

export default function PredictionAutomation() {
  const [state, setState] = useState({ status: "loading", data: null, error: "" });

  const load = useCallback(async () => {
    setState((previous) => ({ ...previous, status: "loading" }));
    try {
      const data = await fetchPredictionAutomationStatus();
      setState({ status: "ready", data, error: "" });
    } catch (error) {
      setState({ status: "error", data: null, error: error?.message || "unavailable" });
    }
  }, []);

  useEffect(() => { load(); }, [load]);

  const data = state.data;
  const controller = data?.controller || {};
  const policy = data?.policy || {};
  const trials = data?.trials || [];

  return (
    <Panel
      title="Challenger automation"
      badges={
        <div className="flex items-center gap-2">
          <Badge variant={data?.enabled ? "ok" : "outline"}>
            {data?.enabled ? "enabled" : "disabled"}
          </Badge>
          <Badge variant={data?.available ? "ok" : "warn"}>
            {data?.storage || "unknown storage"}
          </Badge>
          {controller.leaseHeld && <Badge variant="info">controller active</Badge>}
          <Badge variant="outline">promotion manual</Badge>
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
        <EmptyState title="Challenger automation unavailable">{state.error}</EmptyState>
      )}
      {data?.signedOut && (
        <EmptyState title="Sign in to view challenger automation">
          The trial and shadow ledger is private operational evidence.
        </EmptyState>
      )}

      {data && !data.signedOut && !data.available && (
        <EmptyState title="Durable automation state unavailable">
          {data.reason || "PostgreSQL is required for autonomous trials."}
        </EmptyState>
      )}

      {data?.available && (
        <>
          <div className="grid grid-cols-2 gap-x-5 gap-y-2 border-b border-line/60 px-[22px] py-4 text-[11.5px] sm:grid-cols-4">
            <div>
              <span className="text-dim">trigger</span>
              <div className="num text-muted">{policy.trigger || "—"}</div>
            </div>
            <div>
              <span className="text-dim">trial budget</span>
              <div className="num text-muted">{policy.maxTrialsPerConfig ?? "—"} per config</div>
            </div>
            <div>
              <span className="text-dim">shadow floor</span>
              <div className="num text-muted">{policy.shadowMinPairedBars ?? "—"} paired bars</div>
            </div>
            <div>
              <span className="text-dim">last controller poll</span>
              <div className="num text-muted">{controller.lastPollAt || "never"}</div>
            </div>
          </div>
          <p className="border-b border-line/60 px-[22px] py-2 text-[11.5px] text-dim">
            configs: <span className="num text-muted">{(data.configs || []).join(", ") || "none"}</span>
          </p>
          {controller.lastError && (
            <p className="border-b border-line/60 px-[22px] py-3 text-[11.5px] text-warn">
              {controller.lastError}
            </p>
          )}
          {trials.map((trial) => <TrialRow key={trial.id} trial={trial} />)}
          {trials.length === 0 && (
            <EmptyState title="No autonomous trials yet">
              The controller waits for its fixed number of newer completed real bars. Disabled
              automation creates no trials.
            </EmptyState>
          )}
        </>
      )}

      <p className="border-t border-line px-[22px] py-3 text-[11.5px] leading-relaxed text-dim">
        This process may train immutable challengers, run the fixed evaluator and score a frozen
        challenger/champion pair on future bars. It cannot tune thresholds, promote a model, reset
        the official paper clock, place an order or use real money. Promotion stays an audited
        human action.
      </p>
    </Panel>
  );
}
