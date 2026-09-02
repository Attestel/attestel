import { useCallback, useEffect, useRef, useState } from "react";
import { SectionShell } from "./Placeholder.jsx";
import { Button, EmptyState, SkeletonText, StatusPill } from "../ui/index.js";
import { Tag } from "../terminal/bits.jsx";
import { cx } from "../../lib/cx.js";
import {
  AgencyError,
  AuthRequiredError,
  PRIORITY_LABELS,
  PROVENANCE_LABELS,
  cancelAgencyRun,
  fetchAgencyRun,
  isTerminal,
  listAgencyRuns,
  pollAgencyRun,
  stageProgress,
  startAgencyRun,
} from "../../lib/agencyApi.js";

// AgencySection — start one bounded research run on the owner's own machine, watch truthful
// progress, and read the artifact it produced.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// THREE THINGS THIS SURFACE REFUSES TO DO, EACH OF WHICH WOULD BE EASY AND WRONG.
//
//  1. IT NEVER STARTS A RUN BY ITSELF. `startAgencyRun` is wired to one click. No effect, no mount,
//     no ticker change and no poll may reach it. Everything that runs on a timer here reads a
//     stored row.
//
//  2. IT NEVER INVENTS PROGRESS. There is no animated percentage and no elapsed-time estimate. The
//     progress shown is the stage the WORKER reported, out of the four the workflow declares. When
//     the worker has not reported one, the surface says "queued" and nothing else — a spinner that
//     implies motion during a queue is a lie about where the work is.
//
//  3. IT NEVER FLATTENS PROVENANCE. A `sourced` finding and an `inferred` one are visually
//     distinct, always, with the inferred one marked. Rendering them identically would present a
//     model's reading as a fact, which is the single most damaging thing this surface could do.
//
// The artifact carries no direction, no target and no probability — the schema has no field for
// them — so there is nothing here to suppress. The actionability block is displayed as served:
// NO_SIGNAL, with the six quantitative gates listed as un-evaluated.

const MAX_QUESTION = 500;

function Provenance({ value }) {
  const meta = PROVENANCE_LABELS[value] || PROVENANCE_LABELS.unknown;
  return (
    <span
      title={meta.help}
      className={cx(
        "shrink-0 rounded px-1.5 py-0.5 label-mono text-[10px] uppercase tracking-[0.08em]",
        meta.tone === "accent" && "bg-accent/15 text-accent",
        meta.tone === "warn" && "bg-warn/15 text-warn",
        meta.tone === "muted" && "border border-line2 text-muted",
      )}
    >
      {meta.label}
    </span>
  );
}

function Finding({ finding, sources }) {
  const cited = (finding.sourceIds || [])
    .map((id) => sources.find((s) => s.id === id))
    .filter(Boolean);
  return (
    <li className="flex flex-col gap-1 border-b border-line/60 py-2 last:border-b-0">
      <div className="flex items-start gap-2">
        <Provenance value={finding.provenance} />
        <span className="text-[13px] leading-relaxed text-fg">{finding.statement}</span>
      </div>
      {finding.basis && (
        // A calculated finding shows its arithmetic. A number nobody can re-derive is an assertion.
        <span className="ml-1 font-mono text-[11.5px] text-muted">basis: {finding.basis}</span>
      )}
      {cited.length > 0 && (
        <span className="ml-1 flex flex-wrap gap-x-2 gap-y-1 text-[11px] text-muted">
          {cited.map((s) => (
            // Citations are rendered as TEXT, not as links. The URL came back from an agent that
            // reads the open web; making it one click away from the owner's browser would turn a
            // prompt-injection result into navigation. The host is shown so it can be judged.
            <span key={s.id} title={s.url} className="rounded border border-line2 px-1.5 py-0.5">
              {s.id} · {hostOf(s.url)} · {s.publishedAt}
            </span>
          ))}
        </span>
      )}
    </li>
  );
}

function hostOf(url) {
  try {
    return new URL(url).host;
  } catch {
    return "unknown source";
  }
}

function FindingList({ title, findings, sources }) {
  if (!findings?.length) return null;
  return (
    <div className="px-[22px] py-3">
      <div className="mb-1 text-[12px] font-semibold uppercase tracking-[0.08em] text-muted">
        {title}
      </div>
      <ul className="flex flex-col">
        {findings.map((f, i) => (
          <Finding key={i} finding={f} sources={sources} />
        ))}
      </ul>
    </div>
  );
}

function Artifact({ artifact }) {
  const sources = artifact.sources || [];
  const priority = PRIORITY_LABELS[artifact.researchPriority] || PRIORITY_LABELS.unknown;
  return (
    <div className="flex flex-col divide-y divide-line">
      <div className="flex flex-wrap items-center gap-2 px-[22px] py-3">
        <Tag tone="outline">{artifact.researchPriority}</Tag>
        <span className="text-[13px] text-fg">{priority.label}</span>
        <span className="ml-auto text-[11px] text-muted">
          as of {artifact.asOf} · {sources.length} source{sources.length === 1 ? "" : "s"}
        </span>
      </div>

      {artifact.veto?.raised && (
        <div className="bg-warn/10 px-[22px] py-3">
          <div className="text-[12px] font-semibold text-warn">
            Risk veto — new exposure only
          </div>
          <ul className="mt-1 list-disc pl-4 text-[12.5px] leading-relaxed text-fg">
            {(artifact.veto.reasons || []).map((r, i) => (
              <li key={i}>{r}</li>
            ))}
          </ul>
          <p className="mt-1.5 text-[11px] leading-relaxed text-muted">
            A research veto can only ever withhold NEW exposure. It cannot create a position,
            strengthen one, or stand in the way of closing or reducing one.
          </p>
        </div>
      )}

      <div className="px-[22px] py-3">
        <div className="mb-1 text-[12px] font-semibold uppercase tracking-[0.08em] text-muted">
          Chair conclusion
        </div>
        <p className="text-[13px] leading-relaxed text-fg">{artifact.chairConclusion?.conclusion}</p>
      </div>

      <div className="grid gap-0 md:grid-cols-2 md:divide-x md:divide-line">
        <div>
          <div className="px-[22px] pt-3 text-[12px] font-semibold uppercase tracking-[0.08em] text-muted">
            Thesis
          </div>
          <p className="px-[22px] pt-1 text-[13px] leading-relaxed text-fg">
            {artifact.thesis?.statement}
          </p>
          <FindingList title="Support" findings={artifact.thesis?.support} sources={sources} />
        </div>
        <div>
          <div className="px-[22px] pt-3 text-[12px] font-semibold uppercase tracking-[0.08em] text-muted">
            Anti-thesis
          </div>
          <p className="px-[22px] pt-1 text-[13px] leading-relaxed text-fg">
            {artifact.antiThesis?.statement}
          </p>
          <FindingList title="Support" findings={artifact.antiThesis?.support} sources={sources} />
        </div>
      </div>

      <FindingList title="Risk findings" findings={artifact.riskFindings} sources={sources} />

      {artifact.contradictions?.length > 0 && (
        <div className="px-[22px] py-3">
          <div className="mb-1 text-[12px] font-semibold uppercase tracking-[0.08em] text-muted">
            Contradictions between sources
          </div>
          <ul className="list-disc pl-4 text-[13px] leading-relaxed text-fg">
            {artifact.contradictions.map((c, i) => (
              <li key={i}>
                {c.statement}{" "}
                <span className="text-[11px] text-muted">({(c.sourceIds || []).join(", ")})</span>
              </li>
            ))}
          </ul>
        </div>
      )}

      {artifact.unresolvedQuestions?.length > 0 && (
        <div className="px-[22px] py-3">
          <div className="mb-1 text-[12px] font-semibold uppercase tracking-[0.08em] text-muted">
            Still unresolved
          </div>
          <ul className="list-disc pl-4 text-[13px] leading-relaxed text-fg">
            {artifact.unresolvedQuestions.map((q, i) => (
              <li key={i}>{q}</li>
            ))}
          </ul>
        </div>
      )}

      {/* Every stage, including the ones that found nothing. A stage that produced no findings is a
          fact about the research, and hiding it would make the artifact look more complete. */}
      {(artifact.stages || []).map((stage) => (
        <div key={stage.profile}>
          <div className="flex items-center gap-2 px-[22px] pt-3">
            <span className="label-mono text-[11px] uppercase tracking-[0.08em] text-muted">
              {stage.profile}
            </span>
            <Tag tone={stage.status === "ok" ? "outline" : "warn"}>{stage.status}</Tag>
          </div>
          {stage.findings?.length ? (
            <FindingList title="" findings={stage.findings} sources={sources} />
          ) : (
            <p className="px-[22px] py-2 text-[12px] text-muted">No findings from this stage.</p>
          )}
        </div>
      ))}

      {sources.length > 0 && (
        <div className="px-[22px] py-3">
          <div className="mb-1 text-[12px] font-semibold uppercase tracking-[0.08em] text-muted">
            Sources
          </div>
          <ul className="flex flex-col gap-1 text-[12px] text-muted">
            {sources.map((s) => (
              <li key={s.id}>
                <span className="font-mono">{s.id}</span> · {s.title} · {hostOf(s.url)} ·{" "}
                {s.publishedAt}
              </li>
            ))}
          </ul>
        </div>
      )}

      {artifact.degraded?.length > 0 && (
        <div className="bg-warn/10 px-[22px] py-2.5 text-[12px] text-warn">
          {artifact.degraded.join(" · ")}
        </div>
      )}
    </div>
  );
}

function Actionability({ block }) {
  if (!block) return null;
  return (
    <div className="border-t border-line bg-panel2/40 px-[22px] py-3">
      <div className="flex flex-wrap items-center gap-2">
        <StatusPill tone="neutral">{block.evidenceState}</StatusPill>
        <span className="text-[12.5px] text-fg">{block.action}</span>
      </div>
      <p className="mt-1.5 text-[11.5px] leading-relaxed text-muted">{block.note}</p>
      <div className="mt-2 flex flex-wrap gap-1.5">
        {(block.gates || []).map((g) => (
          <span
            key={g.name}
            title={g.detail}
            className="rounded border border-line2 px-1.5 py-0.5 label-mono text-[10px] text-muted"
          >
            {g.name}: not evaluated
          </span>
        ))}
      </div>
    </div>
  );
}

export default function AgencySection({ ticker }) {
  const [question, setQuestion] = useState("");
  const [runs, setRuns] = useState(null);
  const [active, setActive] = useState(null);
  const [error, setError] = useState("");
  const [ownerOnly, setOwnerOnly] = useState(false);
  const [starting, setStarting] = useState(false);
  const abortRef = useRef(null);

  // Reading the run list on mount is safe: it is a stored read that starts nothing.
  const refresh = useCallback(async () => {
    try {
      const body = await listAgencyRuns(10);
      setRuns(body.runs || []);
      setError("");
      setOwnerOnly(false);
    } catch (err) {
      setRuns([]);
      if (isOwnerOnly(err)) {
        setOwnerOnly(true);
        setError("");
      } else {
        setError(describe(err));
      }
    }
  }, []);

  useEffect(() => {
    refresh();
    return () => abortRef.current?.abort();
  }, [refresh]);

  // follow polls ONE run to a terminal state. It reads; it never re-creates.
  const follow = useCallback(
    async (runId) => {
      abortRef.current?.abort();
      const controller = new AbortController();
      abortRef.current = controller;
      try {
        const final = await pollAgencyRun(runId, {
          signal: controller.signal,
          onTick: (run) => setActive(run),
        });
        if (final) setActive(final);
        refresh();
      } catch (err) {
        setError(describe(err));
      }
    },
    [refresh],
  );

  // selectRun is the ONE path by which a run from the list becomes the active one.
  //
  // IT ABORTS FIRST, FOR EVERY RUN, TERMINAL OR NOT. The previous version only aborted on the
  // non-terminal branch (via `follow`), so clicking a finished run while another was still being
  // polled left that poll alive: its next tick called `setActive` and repainted the run the user
  // had just navigated away from. A terminal run is exactly when a user is most likely to click —
  // they are browsing history while something else runs — so that was the common case, not the
  // edge one.
  const selectRun = useCallback(
    async (run) => {
      abortRef.current?.abort();
      abortRef.current = null;
      setActive(run);
      if (!isTerminal(run.status)) {
        follow(run.id);
        return;
      }
      // The list omits artifacts (a list of 256 KiB documents is a download), so a terminal run
      // needs one read to fill in its body. A fresh signal, so navigating away mid-read discards it.
      const controller = new AbortController();
      abortRef.current = controller;
      try {
        const full = await fetchAgencyRun(run.id, { signal: controller.signal });
        if (!controller.signal.aborted) setActive(full);
      } catch (err) {
        if (!controller.signal.aborted && err?.name !== "AbortError") setError(describe(err));
      }
    },
    [follow],
  );

  // THE ONLY CALL THAT QUEUES WORK. Wired to a click, and to nothing else.
  async function onStart() {
    const q = question.trim();
    if (!q || starting) return;
    setStarting(true);
    setError("");
    try {
      const body = await startAgencyRun(ticker, q);
      setActive(body.run);
      refresh();
      if (!isTerminal(body.run?.status)) follow(body.run.id);
    } catch (err) {
      setError(describe(err));
    } finally {
      setStarting(false);
    }
  }

  async function onCancel(runId) {
    try {
      const run = await cancelAgencyRun(runId);
      setActive(run);
      abortRef.current?.abort();
      refresh();
    } catch (err) {
      setError(describe(err));
    }
  }

  const progress = active ? stageProgress(active) : null;

  return (
    <SectionShell
      title="Research agency"
      tags={<Tag tone="outline">local · {ticker}</Tag>}
      note={
        "One bounded research run, executed by Hermes agents on your own computer. Nothing runs " +
        "here until a local worker claims the job. The result is cited research — never a " +
        "recommendation, a target, or a signal."
      }
    >
      {ownerOnly && (
        <div className="px-[22px] py-5">
          <EmptyState title="The research agency is owner-only on this deployment">
            Research runs execute Hermes agents on the deployment owner&apos;s own computer, so only
            an account listed in <code className="rounded bg-panel2 px-1">AGENCY_OWNER_UIDS</code>{" "}
            can start one. Nothing here is hidden from you by accident — see
            docs/HERMES_AGENCY.md.
          </EmptyState>
        </div>
      )}

      <div className={cx("flex flex-col gap-3 px-[22px] py-4", ownerOnly && "hidden")}>
        <label className="text-[12px] font-semibold text-fg" htmlFor="agency-question">
          What do you want researched about {ticker}?
        </label>
        <textarea
          id="agency-question"
          rows={3}
          value={question}
          maxLength={MAX_QUESTION}
          onChange={(e) => setQuestion(e.target.value)}
          placeholder="e.g. Why did reported gross margin move between the last two quarters?"
          className="w-full rounded-lg border border-line2 bg-panel2 px-3 py-2 text-[13px] text-fg outline-none focus:border-accent/60"
        />
        <div className="flex items-center gap-3">
          <Button onClick={onStart} disabled={starting || !question.trim()}>
            {starting ? "Queueing…" : "Queue research run"}
          </Button>
          <span className="text-[11px] text-muted">
            {question.length}/{MAX_QUESTION}
          </span>
        </div>
        {error && <p className="text-[12px] text-warn">{error}</p>}
      </div>

      {active && (
        <div className="border-t border-line">
          <div className="flex flex-wrap items-center gap-2 px-[22px] py-3">
            <StatusPill tone={toneFor(active.status)}>{active.status}</StatusPill>
            <span className="text-[12.5px] text-fg">{active.question}</span>
            {/* Honest progress: the stage the worker reported, or nothing at all. */}
            {progress?.label ? (
              <span className="ml-auto text-[11.5px] text-muted">
                stage {progress.index}/{progress.total} · {progress.label}
              </span>
            ) : active.status === "queued" ? (
              <span className="ml-auto text-[11.5px] text-muted">
                waiting for a local worker to claim it
              </span>
            ) : null}
            {!isTerminal(active.status) && (
              <Button variant="ghost" onClick={() => onCancel(active.id)}>
                Cancel
              </Button>
            )}
          </div>

          {active.error && (
            <p className="border-t border-line bg-warn/10 px-[22px] py-2.5 text-[12px] text-warn">
              {active.error}
            </p>
          )}

          {active.status === "queued" || active.status === "claimed" || active.status === "running" ? (
            <div className="px-[22px] py-4">
              <SkeletonText lines={2} />
            </div>
          ) : null}

          {active.artifact ? <Artifact artifact={active.artifact} /> : null}
          <Actionability block={active.actionability} />
          <p className="border-t border-line px-[22px] py-2.5 text-[11px] leading-relaxed text-muted">
            {active.disclaimer}
          </p>
        </div>
      )}

      {ownerOnly ? null : runs === null ? (
        <div className="px-[22px] py-4">
          <SkeletonText lines={2} />
        </div>
      ) : runs.length === 0 ? (
        <div className="px-[22px] py-4">
          <EmptyState title="No research runs yet">
            Ask a question above. The run is queued here and picked up by the bridge running on your
            own machine.
          </EmptyState>
        </div>
      ) : (
        <div className="border-t border-line">
          <div className="px-[22px] pt-3 text-[12px] font-semibold uppercase tracking-[0.08em] text-muted">
            Recent runs
          </div>
          <ul className="flex flex-col">
            {runs.map((run) => (
              <li key={run.id} className="border-b border-line/60 last:border-b-0">
                <button type="button" onClick={() => selectRun(run)}
                  className="flex w-full items-center gap-2 px-[22px] py-2.5 text-left hover:bg-panel2/60"
                >
                  <StatusPill tone={toneFor(run.status)}>{run.status}</StatusPill>
                  <span className="truncate text-[12.5px] text-fg">{run.question}</span>
                  <span className="ml-auto shrink-0 text-[11px] text-muted">{run.ticker}</span>
                </button>
              </li>
            ))}
          </ul>
        </div>
      )}
    </SectionShell>
  );
}

// toneFor maps a run state onto StatusPill's tone vocabulary (ui/Status.jsx TONES:
// bull | bear | neutral | caution | info).
//
// A COMPLETED RESEARCH RUN IS `info`, NOT `bull`. The colour discipline in Status.jsx is explicit
// that bull/bear are the DATA colours — price direction, thesis strengthened or challenged. A
// research run finishing says nothing about direction, and painting it green would be the same
// category error this whole lane exists to avoid.
function toneFor(status) {
  if (status === "completed") return "info";
  if (status === "failed" || status === "expired") return "caution";
  return "neutral";
}

function describe(err) {
  if (err instanceof AuthRequiredError) return "Sign in to use the research agency.";
  if (err instanceof AgencyError) return err.message;
  return "Something went wrong reading the research runs.";
}

// isOwnerOnly detects the journal's 403. It is not an error the user can act on by retrying, so the
// section renders it as a state with the configuration hint rather than as a red failure line.
function isOwnerOnly(err) {
  return err instanceof AgencyError && err.status === 403;
}
