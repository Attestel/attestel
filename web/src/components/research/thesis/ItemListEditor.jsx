import { useEffect, useRef } from "react";
import { cx } from "../../../lib/cx.js";
import { LIMITS, fmtWhen } from "../../../lib/thesisApi.js";
import ItemMonitoring from "../../monitoring/ItemMonitoring.jsx";
import { Icon } from "../../shell/icons.jsx";

// ItemListEditor — one of the five repeatable thesis lists (assumptions, catalysts, risks,
// invalidation conditions, next questions).
//
// Repeatable means REPEATABLE FIELDS, not a comma-separated blob: each row is its own labelled input
// backed by a stable server id, because Step 06 links evidence to one assumption and Step 08 watches
// one invalidation condition. A textarea of commas could not carry either link.
//
// Accessibility contract this component owns:
//   * the group is a <fieldset> with a real <legend>, so the list has one name;
//   * every row input has its own <label> (visually hidden — the legend already reads aloud);
//   * add/remove are real buttons with per-row accessible names ("Remove assumption 2");
//   * focus moves predictably: adding focuses the new row, removing focuses the row above it (or the
//     Add button when the list is now empty), so keyboard users are never dropped on <body>;
//   * the error is associated with the group through aria-describedby, not just coloured red.

const ROW_INPUT =
  "w-full min-w-0 rounded-md border border-line bg-panel2 px-2.5 py-1.5 text-[12.5px] text-fg transition-colors placeholder:text-muted/40 hover:border-line2 focus:outline-none focus-visible:border-accent";

// dateToUnix / unixToDate convert between the <input type="date"> value and the contract's unix
// seconds. Parsed as local midnight so the date the user typed is the date that is stored.
function dateToUnix(value) {
  if (!value) return null;
  const [y, m, d] = value.split("-").map(Number);
  if (!y || !m || !d) return null;
  return Math.floor(new Date(y, m - 1, d).getTime() / 1000);
}

function unixToDate(unix) {
  if (!unix) return "";
  const d = new Date(unix * 1000);
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

export default function ItemListEditor({
  spec,
  rows,
  onChange,
  error,
  disabled = false,
  idPrefix,
  // ticker + thesisId mount Step 08's per-item monitoring beside the item it watches. Both are
  // absent while a thesis is being CREATED, and a row that has not been saved yet has no server id
  // to attach a rule to — in either case no control renders, because a rule against an id the
  // server has never seen cannot exist.
  ticker,
  thesisId,
}) {
  const inputRefs = useRef([]);
  const addRef = useRef(null);
  // focusAfter carries the row index to focus once React has committed the new list length. Focusing
  // inside the click handler would target the pre-render DOM.
  const focusAfter = useRef(null);

  useEffect(() => {
    const target = focusAfter.current;
    if (target == null) return;
    focusAfter.current = null;
    if (target === -1) addRef.current?.focus();
    else inputRefs.current[target]?.focus();
  }, [rows.length]);

  const groupId = `${idPrefix}-${spec.field}`;
  const errorId = `${groupId}-error`;
  const helpId = `${groupId}-help`;
  const atCap = rows.length >= LIMITS.itemsPerList;

  const setRow = (i, patch) => {
    onChange(rows.map((r, idx) => (idx === i ? { ...r, ...patch } : r)));
  };

  const addRow = () => {
    if (atCap) return;
    // A new row carries no id; the server mints one on save (resolveItems).
    onChange([...rows, { id: "", text: "", createdAt: null, resolvedAt: null, dueAt: null }]);
    focusAfter.current = rows.length;
  };

  const removeRow = (i) => {
    onChange(rows.filter((_, idx) => idx !== i));
    focusAfter.current = rows.length - 1 === 0 ? -1 : Math.max(0, i - 1);
  };

  // Enter in a row input adds the next row — the fast path for writing a list, without turning Enter
  // into a form submit (which would save a half-written thesis).
  const onKeyDown = (e, i) => {
    if (e.key !== "Enter") return;
    e.preventDefault();
    if (i === rows.length - 1 && rows[i].text.trim() !== "") addRow();
    else inputRefs.current[i + 1]?.focus();
  };

  return (
    <fieldset
      className="min-w-0"
      aria-describedby={cx(helpId, error && errorId)}
      aria-invalid={error ? "true" : undefined}
    >
      <legend className="text-[11px] font-semibold uppercase tracking-wide text-muted">
        {spec.label}
        <span className="ml-1.5 font-mono normal-case tracking-normal text-muted/50">
          {rows.length}/{LIMITS.itemsPerList}
        </span>
      </legend>

      <p id={helpId} className="mt-1 text-[11.5px] leading-relaxed text-muted/80">
        {spec.help}
      </p>

      {error && (
        <p id={errorId} role="alert" className="mt-1.5 flex items-start gap-1.5 text-[11.5px] text-down">
          <Icon name="close" size={11} />
          <span>{error}</span>
        </p>
      )}

      <ul className="mt-2 flex flex-col gap-1.5">
        {rows.map((row, i) => {
          const rowId = `${groupId}-${i}`;
          const answered = spec.hasResolved && row.resolvedAt != null;
          return (
            <li key={row.id || `new-${i}`} className="flex flex-col gap-1.5">
              <div className="flex items-start gap-1.5">
                <span
                  aria-hidden="true"
                  className="mt-2 w-4 shrink-0 text-right font-mono text-[10px] text-muted/50"
                >
                  {i + 1}
                </span>
                <div className="min-w-0 flex-1">
                  <label htmlFor={rowId} className="sr-only">
                    {spec.singular} {i + 1}
                  </label>
                  <input
                    id={rowId}
                    ref={(el) => (inputRefs.current[i] = el)}
                    value={row.text}
                    disabled={disabled}
                    maxLength={LIMITS.itemText}
                    placeholder={spec.placeholder}
                    onChange={(e) => setRow(i, { text: e.target.value })}
                    onKeyDown={(e) => onKeyDown(e, i)}
                    className={cx(ROW_INPUT, answered && "text-muted line-through")}
                  />
                </div>
                <button
                  type="button"
                  disabled={disabled}
                  onClick={() => removeRow(i)}
                  aria-label={`Remove ${spec.singular} ${i + 1}`}
                  className="mt-0.5 rounded-full border border-line px-2 py-1.5 text-[12px] leading-none text-muted transition-colors hover:border-down/60 hover:text-down disabled:opacity-40"
                >
                  <Icon name="close" size={11} />
                </button>
              </div>

              {/* Per-list extras. Both are optional and belong to exactly one list (§1.2). */}
              {spec.hasDueAt && (
                <div className="ml-[22px] flex flex-wrap items-center gap-1.5">
                  <label
                    htmlFor={`${rowId}-due`}
                    className="text-[10.5px] uppercase tracking-wide text-muted/70"
                  >
                    Expected date
                  </label>
                  <input
                    id={`${rowId}-due`}
                    type="date"
                    disabled={disabled}
                    value={unixToDate(row.dueAt)}
                    onChange={(e) => setRow(i, { dueAt: dateToUnix(e.target.value) })}
                    className="rounded-md border border-line bg-panel2 px-2 py-1 text-[11.5px] text-fg focus:outline-none focus-visible:border-accent"
                  />
                  {row.dueAt != null && (
                    <button
                      type="button"
                      disabled={disabled}
                      onClick={() => setRow(i, { dueAt: null })}
                      className="text-[11px] text-muted underline decoration-dotted hover:text-fg"
                    >
                      clear
                    </button>
                  )}
                </div>
              )}

              {/* Step 08's monitoring, rendered beside the item it watches. Only for a SAVED row on
                  a list `alerts` can represent, and only once the thesis itself exists. */}
              {spec.monitorKind && thesisId && row.id && (
                <div className="ml-[22px]">
                  <ItemMonitoring
                    ticker={ticker}
                    thesisId={thesisId}
                    item={row}
                    kind={spec.monitorKind}
                    canEvaluate={Boolean(spec.monitorEvaluable)}
                  />
                </div>
              )}

              {spec.hasResolved && (
                <div className="ml-[22px] flex flex-wrap items-center gap-2">
                  <label
                    htmlFor={`${rowId}-answered`}
                    className="flex items-center gap-1.5 text-[11.5px] text-muted"
                  >
                    <input
                      id={`${rowId}-answered`}
                      type="checkbox"
                      disabled={disabled}
                      checked={answered}
                      onChange={(e) =>
                        setRow(i, { resolvedAt: e.target.checked ? Math.floor(Date.now() / 1000) : null })
                      }
                      className="h-3.5 w-3.5 accent-[var(--color-accent)]"
                    />
                    Answered
                  </label>
                  {answered && (
                    <span className="text-[11px] text-muted/60">{fmtWhen(row.resolvedAt)}</span>
                  )}
                </div>
              )}
            </li>
          );
        })}
      </ul>

      <button
        type="button"
        ref={addRef}
        disabled={disabled || atCap}
        onClick={addRow}
        className="mt-2 rounded-full border border-dashed border-line px-2.5 py-1.5 text-[11.5px] font-semibold text-muted transition-colors hover:border-accent/60 hover:text-accent disabled:opacity-40"
      >
        + Add {spec.singular}
      </button>
      {atCap && (
        <span className="ml-2 text-[11px] text-muted/70">
          Limit of {LIMITS.itemsPerList} reached.
        </span>
      )}
    </fieldset>
  );
}
