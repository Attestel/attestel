import { useEffect, useState } from "react";
import { Badge, Button, EmptyState, Field, SkeletonText, controlClass, useToast } from "../ui/index.js";
import { cx } from "../../lib/cx.js";
import { useAuth } from "../../auth/AuthContext.jsx";
import { createPersona, deletePersona, listPersonas, updatePersona } from "../../lib/api.js";
import { fmtWhenExact } from "../../lib/libraryApi.js";
import { Icon } from "../shell/icons.jsx";

// LensManager — the reusable research methods, and where you write your own.
//
// A lens is a METHOD, not a personality: "apply my semiconductor checklist", "attack this thesis",
// "explain this in plain English". Library is where they live between uses, which is what makes a
// custom lens a reusable asset rather than a one-off prompt.
//
// Three things this surface has to get right:
//
//  1. **Built-in versus yours.** Built-ins ship with the app, are identical for every user, and cannot
//     be edited or deleted — duplicating one is how you get a version you own. That boundary is the
//     llm service's, not a UI convention, and the UI states it rather than hiding the disabled buttons.
//  2. **Version and update time are visible.** A saved review records the lens version it ran under, so
//     a review from before an edit is attributable to the wording that produced it. Hiding the update
//     time would make old reviews look like they came from the current text.
//  3. **Instructions cannot loosen grounding.** A lens layers on top of the hard rules; it can never
//     unlock a price target, a buy/sell call, or an invented number. That is enforced in the llm
//     service (PERSONA_GUARD + the banned-language validation), not here — so this surface says so
//     plainly instead of implying the browser is the guardrail.

const NAME_MAX = 60;
const INSTRUCTIONS_MAX = 2000;

// lensIdOf maps a persona onto the frozen lens id space (PARALLEL_CONTRACTS §3.1): the four builtin:*
// ids are used verbatim; a user persona is addressed as persona:<id>. Shown so a saved review's
// `lens.id` can be matched back to the lens it came from.
function lensIdOf(p) {
  return p.builtin ? p.id : `persona:${p.id}`;
}

export default function LensManager({ onApplyLens, onOpenCopilot }) {
  const { user, promptSignIn } = useAuth();
  const toast = useToast();
  const [lenses, setLenses] = useState(null);
  const [err, setErr] = useState("");
  const [editing, setEditing] = useState(null); // persona being edited, or {} for a new one
  const [busy, setBusy] = useState("");
  const [confirmDelete, setConfirmDelete] = useState("");

  const load = () => {
    setErr("");
    listPersonas()
      .then(setLenses)
      .catch((e) => {
        setLenses([]);
        setErr(e.message);
      });
  };

  useEffect(load, []);

  const builtin = (lenses || []).filter((l) => l.builtin);
  const custom = (lenses || []).filter((l) => !l.builtin);

  const save = async (draft) => {
    setBusy("save");
    try {
      if (draft.id) {
        await updatePersona(draft.id, { name: draft.name, instructions: draft.instructions, emoji: draft.emoji });
        toast({ tone: "success", title: "Lens updated", message: "Reviews saved before now keep the version they ran under." });
      } else {
        await createPersona({ name: draft.name, instructions: draft.instructions, emoji: draft.emoji });
        toast({ tone: "success", title: "Lens created" });
      }
      setEditing(null);
      load();
    } catch (e) {
      toast({ tone: "error", title: "Could not save the lens", message: e.message });
    } finally {
      setBusy("");
    }
  };

  const remove = async (id) => {
    setBusy(id);
    try {
      await deletePersona(id);
      setConfirmDelete("");
      toast({ tone: "success", title: "Lens deleted" });
      load();
    } catch (e) {
      toast({ tone: "error", title: "Could not delete the lens", message: e.message });
    } finally {
      setBusy("");
    }
  };

  return (
    <section className="overflow-hidden surface-card">
      <div className="flex flex-wrap items-center gap-x-2.5 gap-y-2 border-b border-line px-[22px] py-3">
        <span className="text-[14px] font-semibold text-fg">Research lenses</span>
        <span className="min-w-[240px] flex-1 text-[11.5px] leading-relaxed text-muted">
          A lens is a method you can re-apply to any research object. Built-ins ship with the app; a
          Custom Specialist is yours.
        </span>
        {user ? (
          <Button variant="primary" size="xs" onClick={() => setEditing({ name: "", instructions: "", emoji: "" })}>
            New custom lens
          </Button>
        ) : (
          <Button variant="secondary" size="xs" onClick={promptSignIn}>
            Sign in to write one
          </Button>
        )}
      </div>

      <p className="border-b border-line bg-panel2/30 px-[22px] py-2.5 text-[11.5px] leading-relaxed text-muted">
        A lens shapes how a question is approached — it never loosens the rules. No lens can produce a
        price target, a buy/sell call, or a number that is not in the supplied evidence; that is enforced
        by the model service, not by this page.
      </p>

      {err && (
        <div className="px-[22px] py-4 text-[12.5px] text-muted">
          The lens list could not be read ({err}). The built-in lenses still work wherever they are applied.
        </div>
      )}

      {!err && lenses === null && (
        <div className="px-[22px] py-5">
          <SkeletonText lines={4} />
        </div>
      )}

      {!err && lenses !== null && (
        <>
          <GroupHeader
            label="Built-in"
            note="Identical for everyone, read-only, always available — including offline."
          />
          <LensList
            items={builtin}
            onApply={onApplyLens}
            onOpenCopilot={onOpenCopilot}
          />

          <GroupHeader
            label="Your Custom Specialists"
            note="Your own frameworks and checklists. Editing one changes future reviews, not past ones."
          />
          {custom.length === 0 ? (
            <div className="px-[22px] py-4">
              <EmptyState
                title="No custom lenses yet"
                action={
                  user ? (
                    <Button variant="secondary" size="sm" onClick={() => setEditing({ name: "", instructions: "", emoji: "" })}>
                      Write your first lens
                    </Button>
                  ) : (
                    <Button variant="secondary" size="sm" onClick={promptSignIn}>
                      Sign in
                    </Button>
                  )
                }
              >
                A Custom Specialist encodes how <em>you</em> examine a company — a sector checklist, a
                quality framework, the five questions you always ask. Write it once and re-apply it.
              </EmptyState>
            </div>
          ) : (
            <LensList
              items={custom}
              onApply={onApplyLens}
              onOpenCopilot={onOpenCopilot}
              onEdit={setEditing}
              onDelete={remove}
              confirmDelete={confirmDelete}
              onAskDelete={setConfirmDelete}
              busy={busy}
            />
          )}
        </>
      )}

      {editing && (
        <LensEditor
          draft={editing}
          busy={busy === "save"}
          onCancel={() => setEditing(null)}
          onSave={save}
        />
      )}
    </section>
  );
}

function GroupHeader({ label, note }) {
  return (
    <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1 border-y border-line bg-panel2/30 px-[22px] py-2">
      <span className="label-mono text-muted">{label}</span>
      <span className="text-[11px] text-muted">{note}</span>
    </div>
  );
}

function LensList({ items, onApply, onOpenCopilot, onEdit, onDelete, confirmDelete, onAskDelete, busy }) {
  return (
    <ul className="divide-y divide-line">
      {items.map((l) => (
        <li key={l.id} className="flex flex-wrap gap-3 px-[22px] py-3.5">
          <span className="mt-0.5 shrink-0 text-[15px]" aria-hidden="true">
            {l.emoji || <Icon name="lens" size={15} className="text-dim" />}
          </span>
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-[13px] font-semibold text-fg">{l.name}</span>
              <Badge variant={l.builtin ? "outline" : "accent"}>{l.builtin ? "built-in" : "yours"}</Badge>
              <span className="font-mono text-[10px] text-muted/70">{lensIdOf(l)}</span>
            </div>
            <p className="mt-1 line-clamp-3 text-[11.5px] leading-relaxed text-muted">{l.instructions}</p>
            <p className="mt-1 text-[11px] text-muted/80">
              {l.builtin
                ? "Version: ships with the app — reviews cite this lens by id."
                : `Version: last updated ${fmtWhenExact(unixOfIso(l.updatedAt))} — saved reviews record the version they ran under.`}
            </p>
          </div>
          <div className="flex shrink-0 flex-wrap items-start gap-1.5">
            {onApply ? (
              <Button variant="secondary" size="xs" onClick={() => onApply(l.id)}>
                Apply
              </Button>
            ) : (
              onOpenCopilot && (
                <Button variant="secondary" size="xs" onClick={onOpenCopilot}>
                  Apply in Copilot
                </Button>
              )
            )}
            {onEdit && (
              <Button variant="ghost" size="xs" onClick={() => onEdit(l)}>
                Edit
              </Button>
            )}
            {onDelete &&
              (confirmDelete === l.id ? (
                <>
                  <Button variant="danger" size="xs" disabled={busy === l.id} onClick={() => onDelete(l.id)}>
                    {busy === l.id ? "Deleting…" : "Delete"}
                  </Button>
                  <Button variant="ghost" size="xs" onClick={() => onAskDelete("")}>
                    Keep
                  </Button>
                </>
              ) : (
                <Button variant="ghost" size="xs" onClick={() => onAskDelete(l.id)}>
                  Delete
                </Button>
              ))}
          </div>
        </li>
      ))}
    </ul>
  );
}

function unixOfIso(v) {
  if (!v) return 0;
  const ms = Date.parse(v);
  return Number.isNaN(ms) ? 0 : Math.floor(ms / 1000);
}

// LensEditor is deliberately plain: a name, an optional emoji, and the instructions. The guidance under
// the textarea describes what a lens is FOR — a method — because the difference between a useful custom
// lens and a useless one is whether it encodes a repeatable question set.
function LensEditor({ draft, busy, onCancel, onSave }) {
  const [name, setName] = useState(draft.name || "");
  const [emoji, setEmoji] = useState(draft.emoji || "");
  const [instructions, setInstructions] = useState(draft.instructions || "");
  const valid = name.trim() && instructions.trim();

  return (
    <div className="border-t border-line bg-panel2/30 px-[22px] py-4">
      <h3 className="text-[13px] font-semibold text-fg">{draft.id ? "Edit lens" : "New Custom Specialist lens"}</h3>
      <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-[1fr_88px]">
        <Field label={`Name (max ${NAME_MAX})`}>
          <input
            className={controlClass}
            value={name}
            maxLength={NAME_MAX}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. Semiconductor cycle checklist"
          />
        </Field>
        <Field label="Emoji">
          <input className={controlClass} value={emoji} maxLength={8} onChange={(e) => setEmoji(e.target.value)} placeholder="—" />
        </Field>
      </div>
      <div className="mt-3">
        <Field label={`Instructions (${instructions.length}/${INSTRUCTIONS_MAX})`}>
          <textarea
            className={cx(controlClass, "min-h-[132px] resize-y")}
            value={instructions}
            maxLength={INSTRUCTIONS_MAX}
            onChange={(e) => setInstructions(e.target.value)}
            placeholder="Describe the METHOD: what to examine first, which questions to ask every time, what evidence to demand, what would change your mind."
          />
        </Field>
      </div>
      <p className="mt-2 text-[11px] leading-relaxed text-muted">
        Write a method, not a conclusion. Instructions asking for a price target, a buy/sell call, or a
        number the evidence does not contain are refused by the model service regardless of what is
        written here.
      </p>
      <div className="mt-3 flex justify-end gap-2">
        <Button variant="ghost" size="sm" onClick={onCancel} disabled={busy}>
          Cancel
        </Button>
        <Button
          variant="primary"
          size="sm"
          disabled={!valid || busy}
          onClick={() => onSave({ id: draft.id, name: name.trim(), emoji: emoji.trim(), instructions: instructions.trim() })}
        >
          {busy ? "Saving…" : draft.id ? "Save changes" : "Create lens"}
        </Button>
      </div>
    </div>
  );
}
