import { useCallback, useEffect, useMemo, useState } from "react";
import { cx } from "../../lib/cx.js";
import { useAuth } from "../../auth/AuthContext.jsx";
import { useToast } from "../ui/index.js";
import ReviewCard from "./ReviewCard.jsx";
import { listEvidence } from "../../lib/evidenceApi.js";
import {
  AuthRequiredError,
  LensError,
  TASK_BY_ID,
  lensErrorMessage,
  listLenses,
  listReviews,
  runLens,
  tasksForTarget,
} from "../../lib/lensApi.js";
import { Icon } from "../shell/icons.jsx";

// LensRunner — the object-first entry point: named task buttons ON a research object.
//
// This replaces "open a chat and hope". Every run starts from a target the user is already looking
// at, a named method, and an explicit evidence selection — which is what makes the resulting review
// traceable rather than merely plausible.
//
// Rules this component enforces on the UI side:
//
//   · NOTHING RUNS BY ITSELF. There is no effect that calls runLens. Mounting this component, having
//     it re-render, switching ticker, or a quote tick all produce zero model calls (§3.8). Only a
//     click on a task button does. The saved-review LIST is fetched on mount — that is a cheap
//     journal read, not a model call.
//   · EVIDENCE IS CHOSEN, NOT ASSUMED. The user ticks which records the lens may cite, and the count
//     is shown on the button, so nobody is surprised by what the review was allowed to see.
//   · A GUEST IS ROUTED TO SIGN-IN before a run, not shown a dead 401.

function EvidencePicker({ records, selected, onToggle, onAll, onNone }) {
  if (!records.length) {
    return (
      <p className="text-[11.5px] leading-relaxed text-muted">
        No evidence is attached to this yet. A lens can still reason about the claim, but with nothing
        to cite every finding will be marked as inference. Attach evidence first for a review that can
        point at something.
      </p>
    );
  }
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-[10.5px] font-semibold uppercase tracking-wide text-muted">
          Evidence the lens may cite
        </span>
        <button type="button" onClick={onAll} className="text-[10.5px] text-info hover:underline">
          all
        </button>
        <button type="button" onClick={onNone} className="text-[10.5px] text-muted hover:underline">
          none
        </button>
      </div>
      <ul className="flex max-h-[168px] flex-col gap-1 overflow-y-auto">
        {records.map((e) => (
          <li key={e.id}>
            <label className="flex cursor-pointer items-start gap-2 rounded px-1 py-0.5 hover:bg-panel2/60">
              <input
                type="checkbox"
                checked={selected.has(e.id)}
                onChange={() => onToggle(e.id)}
                className="mt-0.5 h-3.5 w-3.5 shrink-0 accent-[var(--color-accent)]"
              />
              <span className="min-w-0 flex-1 break-words text-[11.5px] leading-snug text-fg/85">
                {e.title || e.id}
                {e.dataState === "synthetic" && (
                  <span className="ml-1.5 text-[10px] font-semibold uppercase text-warn">synthetic</span>
                )}
              </span>
            </label>
          </li>
        ))}
      </ul>
    </div>
  );
}

export default function LensRunner({
  target,            // { type, id }
  ticker,
  timeframe = "1D",
  thesisId = "",
  title = "Apply a research lens",
  onAcceptAsQuestion,
  compact = false,
}) {
  const { user, requireAuth } = useAuth();
  const toast = useToast();

  const [lenses, setLenses] = useState([]);
  const [evidence, setEvidence] = useState([]);
  const [selected, setSelected] = useState(() => new Set());
  const [reviews, setReviews] = useState([]);
  const [running, setRunning] = useState(null); // task id in flight
  const [err, setErr] = useState(null);
  const [customLens, setCustomLens] = useState("");
  const [instruction, setInstruction] = useState("");

  const tasks = useMemo(() => tasksForTarget(target?.type), [target?.type]);
  const custom = useMemo(() => lenses.filter((l) => !l.builtin), [lenses]);

  // Catalogue + saved reviews + attachable evidence. All cheap reads; NO model call.
  useEffect(() => {
    let alive = true;
    listLenses()
      .then((r) => alive && setLenses(r.lenses))
      .catch(() => alive && setLenses([]));
    return () => { alive = false; };
  }, [user?.id]);

  const loadReviews = useCallback(() => {
    if (!target?.id) return;
    listReviews(thesisId ? { thesisId } : { ticker })
      .then((r) => setReviews(r.reviews))
      .catch(() => setReviews([]));
  }, [target?.id, thesisId, ticker]);

  useEffect(loadReviews, [loadReviews]);

  useEffect(() => {
    let alive = true;
    listEvidence(thesisId ? { thesisId } : { ticker, limit: 50 })
      .then((r) => {
        if (!alive) return;
        const list = r?.evidence || r || [];
        setEvidence(list);
        // Pre-select everything attached: the user attached it to this thesis on purpose, so it is
        // the sensible default. They can still narrow it before running.
        setSelected(new Set(list.map((e) => e.id)));
      })
      .catch(() => alive && setEvidence([]));
    return () => { alive = false; };
  }, [thesisId, ticker, user?.id]);

  const evidenceById = useMemo(
    () => Object.fromEntries(evidence.map((e) => [e.id, e])),
    [evidence]
  );

  const toggle = (id) =>
    setSelected((s) => {
      const next = new Set(s);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  const run = (task) => {
    if (!requireAuth()) return; // guest -> sign-in, before any request
    if (task === "apply_custom_framework" && !customLens) {
      setErr("Choose one of your own lenses first.");
      return;
    }
    setErr(null);
    setRunning(task);
    runLens({
      task,
      target,
      ticker,
      timeframe,
      lensId: task === "apply_custom_framework" ? customLens : (TASK_BY_ID[task]?.defaultLens || ""),
      evidenceIds: [...selected],
      userInstruction: task === "apply_custom_framework" ? instruction : "",
    })
      .then((review) => {
        if (review) setReviews((r) => [review, ...r]);
        toast({ title: "Review saved", tone: "success" });
      })
      .catch((e) => {
        if (e instanceof AuthRequiredError) return; // requireAuth already routed
        setErr(lensErrorMessage(e));
        if (e instanceof LensError && e.isUnavailable) {
          toast({ title: "Review service unavailable", message: lensErrorMessage(e), tone: "error" });
        }
      })
      .finally(() => setRunning(null));
  };

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-col gap-2">
        <div className="flex flex-wrap items-baseline gap-x-2">
          <h3 className="text-[12px] font-semibold text-fg">{title}</h3>
          <span className="text-[11px] text-muted/70">
            Nothing runs until you pick one — no review starts on its own.
          </span>
        </div>

        <div className="flex flex-wrap gap-1.5">
          {tasks.map((t) => (
            <button
              key={t.id}
              type="button"
              onClick={() => run(t.id)}
              disabled={Boolean(running)}
              title={t.blurb}
              className={cx(
                "rounded-full border px-2.5 py-1.5 text-[11.5px] font-semibold transition-colors disabled:opacity-40",
                running === t.id
                  ? "border-accent/70 bg-accent/15 text-accent"
                  : "border-line bg-panel2 text-fg/90 hover:border-accent/60 hover:text-accent"
              )}
            >
              {running === t.id ? "Running…" : t.label}
              {selected.size > 0 && t.id !== "review_with_perspectives" && (
                <span className="ml-1.5 font-mono text-[10px] text-muted/70">{selected.size}</span>
              )}
            </button>
          ))}
        </div>

        {/* Custom Specialist needs one of the user's own lenses; the field appears with it. */}
        {tasks.some((t) => t.id === "apply_custom_framework") && (
          <div className="flex flex-wrap items-center gap-2">
            <label htmlFor="lens-custom" className="text-[10.5px] uppercase tracking-wide text-muted">
              Your lens
            </label>
            <select
              id="lens-custom"
              value={customLens}
              onChange={(e) => setCustomLens(e.target.value)}
              className="rounded-md border border-line bg-panel2 px-2 py-1 text-[11.5px] text-fg focus:outline-none focus-visible:border-accent"
            >
              <option value="">
                {custom.length ? "Choose one of your lenses…" : "No custom lenses yet"}
              </option>
              {custom.map((l) => (
                <option key={l.id} value={l.id}>
                  {l.emoji ? `${l.emoji} ` : ""}
                  {l.name}
                </option>
              ))}
            </select>
            <input
              value={instruction}
              onChange={(e) => setInstruction(e.target.value)}
              maxLength={1000}
              placeholder="Anything specific to focus on (optional)"
              aria-label="Extra instruction for your lens"
              className="min-w-0 flex-1 rounded-md border border-line bg-panel2 px-2 py-1 text-[11.5px] text-fg placeholder:text-muted/40 focus:outline-none focus-visible:border-accent"
            />
          </div>
        )}

        <EvidencePicker
          records={evidence}
          selected={selected}
          onToggle={toggle}
          onAll={() => setSelected(new Set(evidence.map((e) => e.id)))}
          onNone={() => setSelected(new Set())}
        />

        {err && (
          <p role="alert" className="flex items-start gap-1.5 text-[11.5px] text-down">
            <Icon name="close" size={11} />
            <span>{err}</span>
          </p>
        )}
      </div>

      {reviews.length > 0 && (
        <div className="flex flex-col gap-2">
          <p className="text-[10.5px] font-semibold uppercase tracking-wide text-muted">
            Saved reviews
            <span className="ml-1.5 font-mono normal-case tracking-normal text-muted/50">
              {reviews.length}
            </span>
          </p>
          {reviews.map((r, i) => (
            <ReviewCard
              key={r.id}
              review={r}
              evidenceById={evidenceById}
              onAcceptAsQuestion={onAcceptAsQuestion}
              defaultOpen={i === 0}
              compact={compact}
            />
          ))}
        </div>
      )}
    </div>
  );
}
