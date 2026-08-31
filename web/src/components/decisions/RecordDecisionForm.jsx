import { useEffect, useMemo, useState } from "react";
import { Button, Field, controlClass, EmptyState } from "../ui/index.js";
import { Tag } from "../terminal/bits.jsx";
import { cx } from "../../lib/cx.js";
import { listThesesV2 } from "../../lib/thesisApi.js";
import { listEvidence } from "../../lib/evidenceApi.js";
import { listReviews } from "../../lib/lensApi.js";
import { listTrades } from "../../lib/api.js";
import {
  ACTION_KINDS,
  CONFIDENCE_STEPS,
  HORIZONS,
  INTENTS,
  SIDES,
  decisionErrorMessage,
  recordDecision,
} from "../../lib/decisionApi.js";

// RecordDecisionForm — "why did I do this, and what did I know?"
//
// The form starts from a THESIS, not from a trade, because that is the direction the record runs: a
// decision is about a belief, and the trade (if there was one) is an optional consequence. Choosing
// an intent of "I decided not to act" hides the action controls entirely — a non-action with a side
// and a size is not a non-action, and the server refuses it anyway.
//
// Everything the form sends is user-owned. It cannot send a snapshot, a data-state label, or a model
// signal: those are composed server-side from the caller's own records, so the state a decision
// claims to have been made in cannot be typed in after the fact.

const textareaClass =
  "w-full rounded-md border border-line bg-panel2 px-2.5 py-2 text-[13px] leading-relaxed text-fg placeholder:text-muted/50 focus:outline-none focus-visible:border-accent";

// linesToList splits a textarea into trimmed lines. One line per item is the whole editing model —
// no tag widget, no comma parsing, nothing to get subtly wrong.
const linesToList = (s) =>
  String(s || "")
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);

function Chooser({ options, value, onChange, ariaLabel }) {
  return (
    <div role="radiogroup" aria-label={ariaLabel} className="flex flex-wrap gap-1.5">
      {options.map((o) => {
        const active = o.id === value;
        return (
          <button
            key={o.id}
            type="button"
            role="radio"
            aria-checked={active}
            onClick={() => onChange(o.id)}
            className={cx(
              "rounded-sm border px-2.5 py-1.5 text-left text-[12px] transition-colors",
              active
                ? "border-accent/60 bg-accent/10 text-fg"
                : "border-line bg-panel2 text-muted hover:border-line2 hover:text-fg/85"
            )}
          >
            <span className="block font-semibold">{o.label}</span>
            {o.blurb && <span className="mt-0.5 block max-w-[34ch] text-[11px] leading-snug text-muted">{o.blurb}</span>}
          </button>
        );
      })}
    </div>
  );
}

// Pickers list the caller's OWN records. An empty list is stated plainly rather than hidden: "you
// have not captured evidence for this company" is useful information at the moment of deciding.
function RefPicker({ label, empty, items, selected, onToggle, render }) {
  return (
    <div className="flex flex-col gap-1.5">
      <span className="text-[11px] font-medium uppercase tracking-wide text-muted">{label}</span>
      {items === null ? (
        <span className="text-[11.5px] text-muted/70">loading…</span>
      ) : items.length === 0 ? (
        <span className="text-[11.5px] text-muted/70">{empty}</span>
      ) : (
        <ul className="flex max-h-44 flex-col gap-1 overflow-y-auto rounded-md border border-line bg-panel2/40 p-1.5">
          {items.map((it) => (
            <li key={it.id}>
              <label className="flex cursor-pointer items-start gap-2 rounded px-1.5 py-1 hover:bg-panel2">
                <input
                  type="checkbox"
                  checked={selected.includes(it.id)}
                  onChange={() => onToggle(it.id)}
                  className="mt-[3px] accent-accent"
                />
                <span className="min-w-0 text-[11.5px] leading-snug text-muted">{render(it)}</span>
              </label>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

export default function RecordDecisionForm({
  ticker,
  thesisId: initialThesisId = "",
  checkpointId = "",
  changeItemIds = [],
  monitoringRuleIds = [],
  onRecorded,
  onCancel,
}) {
  const [theses, setTheses] = useState(null);
  const [thesisId, setThesisId] = useState(initialThesisId);
  const [intent, setIntent] = useState("act");
  const [actionKind, setActionKind] = useState("open");
  const [side, setSide] = useState("long");
  const [horizon, setHorizon] = useState("months");
  const [rationale, setRationale] = useState("");
  const [confidenceStep, setConfidenceStep] = useState(3);
  const [conditions, setConditions] = useState("");
  const [questions, setQuestions] = useState("");
  const [evidence, setEvidence] = useState(null);
  const [reviews, setReviews] = useState(null);
  const [trades, setTrades] = useState(null);
  const [evidenceIds, setEvidenceIds] = useState([]);
  const [reviewIds, setReviewIds] = useState([]);
  const [tradeId, setTradeId] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    let alive = true;
    listThesesV2(ticker)
      .then((list) => {
        if (!alive) return;
        setTheses(list || []);
        if (!initialThesisId && list?.length) setThesisId(list[0].id);
      })
      .catch(() => alive && setTheses([]));
    listEvidence({ ticker, limit: 100 })
      .then((r) => alive && setEvidence(r.evidence || []))
      .catch(() => alive && setEvidence([]));
    listReviews({ ticker, limit: 50 })
      .then((r) => alive && setReviews(r || []))
      .catch(() => alive && setReviews([]));
    listTrades("all", "all")
      .then((all) => alive && setTrades((all || []).filter((t) => t.ticker === ticker)))
      .catch(() => alive && setTrades([]));
    return () => {
      alive = false;
    };
  }, [ticker, initialThesisId]);

  const thesis = useMemo(() => (theses || []).find((t) => t.id === thesisId) || null, [theses, thesisId]);
  const intentMeta = INTENTS.find((i) => i.id === intent);
  const needsAction = Boolean(intentMeta?.requiresAction);

  const toggle = (setter) => (id) =>
    setter((prev) => (prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id]));

  async function submit(e) {
    e.preventDefault();
    setError("");
    if (!thesisId) {
      setError("Pick the thesis this decision is about — a decision is always about a belief.");
      return;
    }
    if (!rationale.trim()) {
      setError("Write down your reasoning. That is the part worth reviewing later.");
      return;
    }
    setSaving(true);
    try {
      const decision = await recordDecision({
        ticker,
        thesisId,
        checkpointId,
        changeItemIds,
        monitoringRuleIds,
        intent,
        actionKind: needsAction ? actionKind : "none",
        actionSide: needsAction ? side : null,
        horizon,
        rationale: rationale.trim(),
        userConfidence: CONFIDENCE_STEPS[confidenceStep - 1].value,
        evidenceIds,
        reviewIds,
        openQuestions: linesToList(questions),
        conditionsToMonitor: linesToList(conditions),
        tradeId: needsAction && tradeId ? tradeId : null,
      });
      onRecorded?.(decision);
    } catch (err) {
      setError(decisionErrorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  if (theses !== null && theses.length === 0) {
    return (
      <div className="px-[22px] py-5">
        <EmptyState title={`No thesis for ${ticker} yet`}>
          A decision record points at the belief you acted on, and the exact version of it. Write the
          thesis first — then recording what you did about it takes a moment.
        </EmptyState>
      </div>
    );
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-4 px-[22px] py-4">
      <Field label="Thesis this decision is about">
        <select className={controlClass} value={thesisId} onChange={(e) => setThesisId(e.target.value)}>
          {(theses || []).map((t) => (
            <option key={t.id} value={t.id}>
              {(t.claim || t.text || "(untitled)").slice(0, 90)}
              {t.status ? ` — ${t.status}` : ""}
            </option>
          ))}
        </select>
      </Field>
      {thesis && (
        <p className="-mt-2 text-[11px] leading-relaxed text-muted/70">
          The wording, assumptions, invalidation conditions and confidence above are copied into this
          record as they are <em>right now</em>. Editing the thesis later will not change what this
          decision says you believed.
        </p>
      )}

      <div className="flex flex-col gap-1.5">
        <span className="text-[11px] font-medium uppercase tracking-wide text-muted">What did you decide?</span>
        <Chooser options={INTENTS} value={intent} onChange={setIntent} ariaLabel="Decision intent" />
      </div>

      {needsAction ? (
        <div className="flex flex-wrap gap-3">
          <div className="flex min-w-0 flex-col gap-1.5">
            <span className="text-[11px] font-medium uppercase tracking-wide text-muted">Action</span>
            <Chooser options={ACTION_KINDS} value={actionKind} onChange={setActionKind} ariaLabel="Action" />
          </div>
          <div className="flex min-w-0 flex-col gap-1.5">
            <span className="text-[11px] font-medium uppercase tracking-wide text-muted">Direction</span>
            <Chooser options={SIDES} value={side} onChange={setSide} ariaLabel="Side" />
          </div>
        </div>
      ) : (
        <p className="rounded-md border border-line bg-panel2/40 px-3 py-2 text-[11.5px] leading-relaxed text-muted">
          Recorded as a deliberate non-action. No position, no size, no price — just the reasoning and
          what you are watching. This is the record a trade log cannot hold.
        </p>
      )}

      <Field label="Why? (this is the part you will review)">
        <textarea
          className={textareaClass}
          rows={4}
          value={rationale}
          onChange={(e) => setRationale(e.target.value)}
          placeholder="What made this the right call with what you knew today?"
        />
      </Field>

      <div className="flex flex-wrap gap-3">
        <Field label="Horizon" className="w-[140px]">
          <select className={controlClass} value={horizon} onChange={(e) => setHorizon(e.target.value)}>
            {HORIZONS.map((h) => (
              <option key={h} value={h}>
                {h}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Your confidence" className="min-w-[220px] flex-1">
          <div className="flex items-center gap-2">
            <input
              type="range"
              min={1}
              max={5}
              step={1}
              value={confidenceStep}
              onChange={(e) => setConfidenceStep(Number(e.target.value))}
              className="w-full accent-accent"
              aria-label="Your confidence"
            />
            <span className="w-[92px] shrink-0 text-[11.5px] text-muted">
              {CONFIDENCE_STEPS[confidenceStep - 1].label}
            </span>
          </div>
        </Field>
      </div>

      <div className="grid gap-3 sm:grid-cols-2">
        <RefPicker
          label="Evidence you considered"
          empty={`No evidence captured for ${ticker} yet.`}
          items={evidence}
          selected={evidenceIds}
          onToggle={toggle(setEvidenceIds)}
          render={(ev) => (
            <>
              <span className="text-fg/85">{ev.title || ev.kind}</span>{" "}
              <Tag tone="outline">{ev.provider}</Tag>
              {ev.dataState === "synthetic" && <Tag tone="warn">synthetic</Tag>}
            </>
          )}
        />
        <RefPicker
          label="Reviews you read"
          empty="No saved lens reviews for this company yet."
          items={reviews}
          selected={reviewIds}
          onToggle={toggle(setReviewIds)}
          render={(rv) => (
            <>
              <span className="text-fg/85">{rv.lens?.name || rv.task || "review"}</span>{" "}
              <Tag tone="outline">{rv.task}</Tag>
            </>
          )}
        />
      </div>

      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Conditions to monitor (one per line)">
          <textarea
            className={textareaClass}
            rows={3}
            value={conditions}
            onChange={(e) => setConditions(e.target.value)}
            placeholder={"Hyperscaler capex guidance\nGross margin below 70%"}
          />
        </Field>
        <Field label="Still unresolved (one per line)">
          <textarea
            className={textareaClass}
            rows={3}
            value={questions}
            onChange={(e) => setQuestions(e.target.value)}
            placeholder={"How much of the backlog is non-cancellable?"}
          />
        </Field>
      </div>

      {needsAction && (
        <Field label="Link a trade you already recorded (optional)">
          <select className={controlClass} value={tradeId} onChange={(e) => setTradeId(e.target.value)}>
            <option value="">No linked trade</option>
            {(trades || []).map((t) => (
              <option key={t.id} value={t.id}>
                {t.entry?.date} · {t.side} · {t.entry?.price} × {t.entry?.size}
                {t.mode === "paper" ? " (simulated)" : ""}
              </option>
            ))}
          </select>
        </Field>
      )}

      {error && (
        <p className="rounded-md border border-down/40 bg-down/[0.08] px-3 py-2 text-[12px] text-down">{error}</p>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" variant="primary" size="sm" disabled={saving}>
          {saving ? "Recording…" : "Record decision"}
        </Button>
        {onCancel && (
          <Button type="button" variant="ghost" size="sm" onClick={onCancel}>
            Cancel
          </Button>
        )}
        <span className="text-[11px] text-muted/70">
          A record of your own decision. This app executes nothing and holds no money.
        </span>
      </div>
    </form>
  );
}
