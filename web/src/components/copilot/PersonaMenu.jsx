import { useEffect, useRef, useState } from "react";
import { cx } from "../../lib/cx.js";
import { useAuth } from "../../auth/AuthContext.jsx";
import { AuthRequiredError } from "../../lib/api.js";
import { Icon } from "../shell/icons.jsx";

// PersonaMenu — the Copilot "instructor" picker (Gemini-Gem-style custom instructions). A dropdown to
// choose the active persona plus inline create/edit/delete for the user's own personas. Built-ins
// (id starts with "builtin:") are read-only. Personas only shape the chat's ROLE/tone/teaching style;
// the grounded chat keeps its rules regardless — never a prediction, a number, or a buy/sell call.

const DEFAULT_LABEL = "Default";

function personaFace(p) {
  if (!p) return { emoji: <Icon name="compass" size={15} />, name: DEFAULT_LABEL };
  return { emoji: p.emoji || <Icon name="lens" size={15} />, name: p.name };
}

export default function PersonaMenu({
  personas = [],
  selectedId = null,
  onSelect,
  onCreate,
  onUpdate,
  onDelete,
}) {
  const { requireAuth, promptSignIn } = useAuth();
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState(null); // null | {id?, name, instructions, emoji}
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const rootRef = useRef(null);

  const selected = personas.find((p) => p.id === selectedId) || null;
  const face = personaFace(selected);

  // Close on outside click / Escape (unless the edit form is open — Escape backs out of that first).
  useEffect(() => {
    if (!open) return;
    const onDoc = (e) => {
      if (rootRef.current && !rootRef.current.contains(e.target)) {
        setOpen(false);
        setForm(null);
        setError("");
      }
    };
    const onKey = (e) => {
      if (e.key !== "Escape") return;
      e.stopPropagation();
      if (form) { setForm(null); setError(""); }
      else setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey, true);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey, true);
    };
  }, [open, form]);

  // Creating/editing an instructor is a WRITE — a guest is routed to sign-in before the form opens.
  const startCreate = () => requireAuth(() => { setError(""); setForm({ name: "", instructions: "", emoji: "" }); });
  const startEdit = (p) => requireAuth(() => { setError(""); setForm({ id: p.id, name: p.name, instructions: p.instructions, emoji: p.emoji || "" }); });

  const submit = async (e) => {
    e.preventDefault();
    if (!form.name.trim() || !form.instructions.trim()) {
      setError("Name and instructions are both required.");
      return;
    }
    setBusy(true);
    setError("");
    try {
      if (form.id) {
        await onUpdate(form.id, { name: form.name, instructions: form.instructions, emoji: form.emoji });
      } else {
        const created = await onCreate({ name: form.name, instructions: form.instructions, emoji: form.emoji });
        if (created?.id) onSelect(created.id);
      }
      setForm(null);
    } catch (err) {
      if (err instanceof AuthRequiredError) { promptSignIn(); return; } // session lapsed -> sign in
      setError(err.message || "Could not save the persona.");
    } finally {
      setBusy(false);
    }
  };

  const remove = (p) => requireAuth(async () => {
    setBusy(true);
    setError("");
    try {
      await onDelete(p.id);
      if (selectedId === p.id) onSelect(null);
    } catch (err) {
      if (err instanceof AuthRequiredError) { promptSignIn(); return; }
      setError(err.message || "Could not delete the persona.");
    } finally {
      setBusy(false);
    }
  });

  const builtins = personas.filter((p) => p.builtin);
  const mine = personas.filter((p) => !p.builtin);

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="menu"
        aria-expanded={open}
        title="Choose an instructor persona"
        className={cx(
          "flex items-center gap-1.5 rounded-full border px-2 py-1.5 font-mono text-[11px] transition-colors",
          selected
            ? "border-accent/50 bg-accent/10 text-fg"
            : "border-line2 text-fg/85 hover:border-accent/50 hover:text-fg"
        )}
      >
        <span aria-hidden="true">{face.emoji}</span>
        <span className="max-w-[92px] truncate">{face.name}</span>
        <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" aria-hidden="true">
          <path d="M6 9l6 6 6-6" />
        </svg>
      </button>

      {open && (
        <div
          role="menu"
          className="absolute right-0 z-10 mt-1.5 max-h-[420px] w-[288px] overflow-y-auto rounded-lg border border-line bg-panel shadow-2xl"
        >
          {form ? (
            <form onSubmit={submit} className="p-3">
              <div className="mb-2 label-mono text-muted">
                {form.id ? "Edit instructor" : "New instructor"}
              </div>
              <div className="flex gap-2">
                <input
                  value={form.emoji}
                  onChange={(e) => setForm({ ...form, emoji: e.target.value })}
                  placeholder="—"
                  aria-label="Emoji"
                  className="w-12 rounded-sm border border-line2 bg-panel2 px-2 py-2 text-center text-[15px] focus:border-accent/60 focus:outline-none"
                />
                <input
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                  placeholder="Name (e.g. Options Coach)"
                  aria-label="Persona name"
                  maxLength={60}
                  className="flex-1 rounded-sm border border-line2 bg-panel2 px-2.5 py-2 text-[13px] text-fg placeholder:text-muted/60 focus:border-accent/60 focus:outline-none"
                />
              </div>
              <textarea
                value={form.instructions}
                onChange={(e) => setForm({ ...form, instructions: e.target.value })}
                placeholder="Instructions — how should the Copilot behave? (role, tone, what to focus on, how to explain)"
                aria-label="Persona instructions"
                rows={5}
                maxLength={2000}
                className="mt-2 w-full resize-none rounded-sm border border-line2 bg-panel2 px-2.5 py-2 text-[12.5px] leading-relaxed text-fg placeholder:text-muted/60 focus:border-accent/60 focus:outline-none"
              />
              <p className="mt-1.5 text-[10.5px] leading-relaxed text-muted/70">
                Shapes tone &amp; teaching style only. It can't unlock price predictions or buy/sell calls.
              </p>
              {error && <p className="mt-1.5 text-[11.5px] text-down">{error}</p>}
              <div className="mt-2.5 flex justify-end gap-2">
                <button
                  type="button"
                  onClick={() => { setForm(null); setError(""); }}
                  className="rounded-full border border-line2 px-3 py-1.5 text-[12px] text-muted hover:text-fg"
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  disabled={busy}
                  className="rounded-full bg-fg px-3.5 py-1.5 text-[12px] font-semibold text-bg transition-colors duration-200 ease-premium hover:bg-white disabled:opacity-40"
                >
                  {form.id ? "Save" : "Create"}
                </button>
              </div>
            </form>
          ) : (
            <div className="py-1.5">
              <PersonaRow
                active={!selectedId}
                emoji={<Icon name="compass" size={15} />}
                name={DEFAULT_LABEL}
                sub="Grounded assistant, no persona"
                onClick={() => { onSelect(null); setOpen(false); }}
              />

              <div className="mt-1 px-3 pb-1 pt-2 label-mono text-muted">Presets</div>
              {builtins.map((p) => (
                <PersonaRow
                  key={p.id}
                  active={selectedId === p.id}
                  emoji={p.emoji || <Icon name="lens" size={15} />}
                  name={p.name}
                  sub={p.instructions}
                  onClick={() => { onSelect(p.id); setOpen(false); }}
                />
              ))}

              <div className="mt-1 flex items-center justify-between px-3 pb-1 pt-2">
                <span className="label-mono text-muted">Your instructors</span>
                <button
                  type="button"
                  onClick={startCreate}
                  className="font-mono text-[10px] text-accent hover:brightness-125"
                >
                  + New
                </button>
              </div>
              {mine.length === 0 && (
                <p className="px-3 py-1.5 text-[11.5px] leading-relaxed text-muted/70">
                  None yet. Create one to teach the Copilot how you like to be helped.
                </p>
              )}
              {mine.map((p) => (
                <PersonaRow
                  key={p.id}
                  active={selectedId === p.id}
                  emoji={p.emoji || <Icon name="lens" size={15} />}
                  name={p.name}
                  sub={p.instructions}
                  onClick={() => { onSelect(p.id); setOpen(false); }}
                  onEdit={() => startEdit(p)}
                  onDelete={() => remove(p)}
                />
              ))}
              {error && <p className="px-3 py-1.5 text-[11.5px] text-down">{error}</p>}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function PersonaRow({ active, emoji, name, sub, onClick, onEdit, onDelete }) {
  return (
    <div
      className={cx(
        "group flex items-start gap-2.5 px-3 py-2 transition-colors",
        active ? "bg-accent/10" : "hover:bg-panel2"
      )}
    >
      <button type="button" role="menuitemradio" aria-checked={active} onClick={onClick} className="flex min-w-0 flex-1 items-start gap-2.5 text-left">
        <span className="mt-[1px] text-[15px]" aria-hidden="true">{emoji}</span>
        <span className="min-w-0 flex-1">
          <span className={cx("block text-[13px] font-medium", active ? "text-fg" : "text-fg/85")}>{name}</span>
          {sub && <span className="mt-0.5 block truncate text-[11px] text-muted/70">{sub}</span>}
        </span>
        {active && (
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round" className="mt-0.5 shrink-0 text-accent" aria-hidden="true">
            <path d="M20 6L9 17l-5-5" />
          </svg>
        )}
      </button>
      {(onEdit || onDelete) && (
        <span className="flex shrink-0 gap-1 opacity-0 transition-opacity group-hover:opacity-100">
          {onEdit && (
            <button type="button" onClick={onEdit} aria-label={`Edit ${name}`} className="rounded p-1 text-muted hover:text-fg">
              <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><path d="M12 20h9M16.5 3.5a2.1 2.1 0 013 3L7 19l-4 1 1-4z" /></svg>
            </button>
          )}
          {onDelete && (
            <button type="button" onClick={onDelete} aria-label={`Delete ${name}`} className="rounded p-1 text-muted hover:text-down">
              <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round"><path d="M3 6h18M8 6V4h8v2M6 6l1 14h10l1-14" /></svg>
            </button>
          )}
        </span>
      )}
    </div>
  );
}
