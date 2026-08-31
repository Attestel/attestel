import { useCallback, useEffect, useRef, useState } from "react";
import {
  AdminRequiredError,
  AuthRequiredError,
  RunInFlightError,
  fetchEstimateSnapshotsStatus,
  fetchEventEvaluationStatus,
  fetchEvaluationStatus,
  runEstimateSnapshots,
  runEventEvaluation,
  runEvaluation,
} from "../../lib/api.js";
import { Badge, Button, EmptyState, Panel } from "../ui/index.js";

// EvaluationRunner — the in-app trigger for the offline edge-evaluation harness (Stage 1 /
// docs/VALIDATION_AND_GO_LIVE.md §2.3), and the smallest surface that can do the job.
//
// It exists because production is the single-container deploy where the operator has no shell: the
// harness was CLI-only, so a verdict could not be produced there at all. Retraining already had a
// button; this is the other half.
//
// What it must get right, and what the design is for:
//
//   * NO EDGE / INCONCLUSIVE / SUSPECT ARE RESULTS, NOT ERRORS. They arrive on a successful run and
//     are rendered as first-class outcomes, visually distinct from a failure. "There is no tradeable
//     edge here" is the finding the whole harness exists to be able to state.
//   * It offers no parameters. The run uses the deployment's own EVAL_* environment so the verdict
//     it mints carries the deployment's strategy version (contract §4.3).
//   * `current` is the flag paper gate 4 actually spends — served by the same computation /predict
//     uses. This panel displays it; it does not compute it, and nothing here can change a verdict.
//
// The status poll (while, and only while, a run is in flight) reads a status file. It starts
// nothing and it never reaches the model, so invariant #4 is untouched.

const POLL_MS = 5000;

// The verdict vocabulary. EDGE is the only tradeable one (contract §4, gate 4); the rest are honest
// answers, not faults — hence `warn`/`dim`, never `down`.
const VERDICT_TONE = {
  EDGE: "ok",
  "NO EDGE": "dim",
  INCONCLUSIVE: "dim",
  SUSPECT: "warn",
};

const STATE_TONE = { idle: "outline", running: "info", done: "ok", failed: "warn" };

function VerdictRow({ row }) {
  if (row.unreadable) {
    return (
      <div className="flex items-center gap-2 border-b border-line/60 px-[22px] py-3 last:border-b-0">
        <span className="num text-[12px] text-fg">{row.file}</span>
        <Badge variant="warn">unreadable</Badge>
        <span className="text-[11.5px] text-dim">a verdict nobody can parse is a verdict nobody has</span>
      </div>
    );
  }
  return (
    <div className="flex flex-col gap-1 border-b border-line/60 px-[22px] py-3 last:border-b-0 sm:flex-row sm:items-center sm:justify-between">
      <div className="flex items-center gap-2">
        <span className="num text-[12.5px] font-semibold text-fg">
          {row.ticker} · {row.timeframe} · H{row.horizon}
        </span>
        <Badge variant={VERDICT_TONE[row.verdict] || "dim"}>{row.verdict || "—"}</Badge>
        {row.current ? (
          <Badge variant="ok" title="matches the served record's strategy, methodology, completed-bar policy, and evidence floors">
            current
          </Badge>
        ) : (
          <Badge variant="outline" title="strategy, methodology, data policy, or evidence no longer matches — gate 4 refuses it">
            not current
          </Badge>
        )}
      </div>
      <div className="flex flex-col text-[11.5px] text-dim sm:items-end">
        <span className="num">{row.evaluatedAt || "—"}</span>
        {row.scope && <span>{row.scope}</span>}
      </div>
    </div>
  );
}

export default function EvaluationRunner({ kind = "price" }) {
  const isEvent = kind === "event";
  const isEstimate = kind === "estimate";
  const [status, setStatus] = useState(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [starting, setStarting] = useState(false);
  const timer = useRef(null);

  const load = useCallback(async () => {
    try {
      const fetchStatus = isEstimate
        ? fetchEstimateSnapshotsStatus
        : isEvent ? fetchEventEvaluationStatus : fetchEvaluationStatus;
      setStatus(await fetchStatus());
      setError("");
    } catch (err) {
      setError(err?.message || "unavailable");
    }
  }, [isEstimate, isEvent]);

  useEffect(() => {
    load();
  }, [load]);

  // Poll ONLY while a run is in flight, and stop the moment it is not. A permanent interval here
  // would be a timer driving a server read forever, which this app has given up everywhere else.
  useEffect(() => {
    if (status?.state !== "running") {
      if (timer.current) {
        clearInterval(timer.current);
        timer.current = null;
      }
      return undefined;
    }
    timer.current = setInterval(load, POLL_MS);
    return () => {
      clearInterval(timer.current);
      timer.current = null;
    };
  }, [status?.state, load]);

  const start = useCallback(async () => {
    setStarting(true);
    setError("");
    setNotice("");
    try {
      const run = isEstimate ? runEstimateSnapshots : isEvent ? runEventEvaluation : runEvaluation;
      const started = await run();
      setNotice(
        `Started ${started?.startedAt || ""} — ${isEstimate ? "collecting due snapshots" : "this takes minutes"}.`
      );
      await load();
    } catch (err) {
      if (err instanceof RunInFlightError) {
        setNotice("A run is already in flight.");
        await load();
      } else if (err instanceof AdminRequiredError || err instanceof AuthRequiredError) {
        // The server's own message, verbatim: it names EVAL_ADMIN_UIDS, which is what the operator
        // needs in order to fix it. Rewriting it here would hide the cause.
        setError(err.message);
      } else {
        setError(
          err?.message || (isEstimate
            ? "could not start estimate collection"
            : `could not start the ${isEvent ? "PEAD " : ""}evaluation`)
        );
      }
    } finally {
      setStarting(false);
    }
  }, [isEstimate, isEvent, load]);

  const state = status?.state || "idle";
  const running = state === "running";
  const verdicts = status?.verdicts || [];
  const report = status?.latestReport;
  const collector = status?.collectorResult;
  const title = isEstimate ? "Forward estimate snapshots" : isEvent ? "PEAD event study" : "Edge evaluation";
  const action = isEstimate ? "Capture due estimates" : isEvent ? "Run PEAD audit" : "Run edge evaluation";

  return (
    <Panel
      title={title}
      badges={<Badge variant={STATE_TONE[state] || "outline"} dot={running} pulse={running}>{state}</Badge>}
      actions={
        <div className="flex items-center gap-2">
          <Button variant="ghost" onClick={load} disabled={starting}>
            Refresh
          </Button>
          <Button onClick={start} disabled={starting || running || status?.signedOut}>
            {starting ? "Starting…" : running ? "Running…" : action}
          </Button>
        </div>
      }
      flush
    >
      {status?.signedOut && (
        <EmptyState title="Sign in to view the evaluation state">
          What this deployment has evaluated is operational detail, not public content.
        </EmptyState>
      )}

      {error && (
        <p className="border-b border-line/60 px-[22px] py-3 text-[12px] text-down">{error}</p>
      )}
      {notice && (
        <p className="border-b border-line/60 px-[22px] py-3 text-[12px] text-muted">{notice}</p>
      )}

      <div className="border-b border-line/60 px-[22px] py-4 text-[12px] leading-relaxed text-muted">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
          <span>
            started <span className="num text-fg">{status?.startedAt || "—"}</span>
          </span>
          <span>
            finished <span className="num text-fg">{status?.finishedAt || "—"}</span>
          </span>
          {status?.exitCode !== null && status?.exitCode !== undefined && (
            <span>
              exit <span className="num text-fg">{status.exitCode}</span>
            </span>
          )}
          {status?.refusal && <Badge variant="warn">refused</Badge>}
        </div>
        {status?.exitMeaning && <p className="mt-1 text-[11.5px] text-dim">{status.exitMeaning}</p>}
        {status?.note && <p className="mt-1 text-[11.5px] text-warn">{status.note}</p>}
      </div>

      {report && (
        <div className="border-b border-line/60 px-[22px] py-4">
          <div className="mb-2 flex flex-wrap items-center gap-2">
            <span className="label-mono text-muted">Latest report</span>
            <span className="num text-[11.5px] text-dim">{report.file}</span>
            {report.verdict && (
              <Badge variant={VERDICT_TONE[report.verdict] || "dim"}>{report.verdict}</Badge>
            )}
          </div>
          {report.meaning && <p className="text-[11.5px] text-muted">{report.meaning}</p>}
          <div className="mt-2 flex flex-col gap-3">
            {Object.entries(report.byHorizon || {}).map(([h, block]) => (
              <div key={h} className="rounded-md border border-line/70 bg-panel2/40 px-3 py-2.5">
                <div className="flex items-center gap-1.5 text-[11.5px] text-dim">
                  <span className="num text-muted">H{h}</span>
                  <Badge variant={VERDICT_TONE[block?.verdict] || "dim"}>{block?.verdict || "—"}</Badge>
                </div>
                {block?.checklist?.length > 0 && (
                  <ul className="mt-2 space-y-1 font-mono text-[11px] leading-relaxed">
                    {block.checklist.map((line) => (
                      <li key={line} className={line.startsWith("FAIL") ? "text-warn" : "text-dim"}>{line}</li>
                    ))}
                  </ul>
                )}
              </div>
            ))}
          </div>
        </div>
      )}

      {isEstimate && collector && (
        <div className="border-b border-line/60 px-[22px] py-4 text-[12px] text-muted">
          <div className="mb-2 flex flex-wrap items-center gap-2">
            <span className="label-mono text-muted">Latest collection</span>
            <Badge variant={collector.state === "done" ? "ok" : "warn"}>
              {collector.state || "unknown"}
            </Badge>
            {collector.quotaExhausted && <Badge variant="warn">quota reached</Badge>}
          </div>
          <div className="flex flex-wrap gap-x-4 gap-y-1">
            <span>API calls <span className="num text-fg">{collector.apiCalls ?? "—"}</span></span>
            <span>due <span className="num text-fg">{collector.due ?? 0}</span></span>
            <span>captured <span className="num text-fg">{collector.captured?.length ?? 0}</span></span>
            <span>existing <span className="num text-fg">{collector.skippedExisting?.length ?? 0}</span></span>
            <span>actuals <span className="num text-fg">{collector.actualsRefreshed?.length ?? 0}</span></span>
            <span>provider errors <span className="num text-fg">{collector.providerErrors?.length ?? 0}</span></span>
          </div>
          {collector.reason && <p className="mt-2 text-warn">{collector.reason}</p>}
          {collector.captured?.length > 0 && (
            <p className="mt-2 text-[11.5px] text-dim">
              {collector.captured.map((row) => `${row.ticker} ${row.stage}`).join(" · ")}
            </p>
          )}
        </div>
      )}

      {!isEvent && !isEstimate && (verdicts.length > 0 ? (
        verdicts.map((row) => <VerdictRow key={row.file} row={row} />)
      ) : (
        !status?.signedOut && (
          <EmptyState title="No verdicts stored yet">
            Nothing has been evaluated in this deployment. Retrain each config on real data first
            (§2.2), then run the evaluation.
          </EmptyState>
        )
      ))}

      {isEvent && !report && !status?.signedOut && (
        <EmptyState title="No PEAD report stored yet">
          The audit needs durable earnings history plus real stock and SPY bars. Its results are
          research evidence only and never unlock the paper-trading gate.
        </EmptyState>
      )}

      {isEstimate && !collector && !status?.signedOut && !running && (
        <EmptyState title="No collection pass recorded yet">
          Start one bounded pass to capture any T−7 or T−1 estimates due today.
        </EmptyState>
      )}

      {status?.logTail?.length > 0 && (
        <details className="border-t border-line px-[22px] py-3">
          <summary className="cursor-pointer text-[11.5px] text-muted">
            Run log — last {status.logTail.length} lines
          </summary>
          <pre className="mt-2 max-h-64 overflow-auto whitespace-pre-wrap break-words text-[11px] leading-relaxed text-dim">
            {status.logTail.join("\n")}
          </pre>
        </details>
      )}

      <p className="border-t border-line px-[22px] py-3 text-[11.5px] leading-relaxed text-dim">
        {isEstimate ? (
          <>
            This spends provider calls only for due, not-yet-stored ticker/fiscal-period stages and
            writes immutable PostgreSQL evidence. Run it once per calendar day; repeated same-day
            runs skip existing snapshots. It never changes a PEAD verdict directly.
          </>
        ) : isEvent ? (
          <>
            PEAD is evaluated on stock-minus-SPY daily returns with report-era SUE thresholds and
            an untouched date holdout. Historical estimates remain descriptive; only individually
            captured pre-release snapshots can enter the forward-verified sample. This report is
            separate from the price-model verdict and cannot open a paper position.
          </>
        ) : (
          <>
            <span className="text-warn">NO EDGE</span>, <span className="text-warn">INCONCLUSIVE</span>{" "}
            and <span className="text-warn">SUSPECT</span> are results, not failures — a run that returns
            one worked. A persistent negative answer means <span className="text-fg">DISABLE, not tune</span>:
            fix the cause, never the gate. Do not hand-write a verdict file, relax the evaluator, or
            re-run with different thresholds until one passes. The run takes no parameters for exactly
            that reason, and it is CPU-heavy — the signal will be slower to load while it is going.
          </>
        )}
      </p>
    </Panel>
  );
}
