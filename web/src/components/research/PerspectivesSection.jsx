import { useEffect, useState } from "react";
import { listThesesV2 } from "../../lib/thesisApi.js";
import { EmptyState, SkeletonText } from "../ui/index.js";
import { Micro, Tag } from "../terminal/bits.jsx";
import { SectionShell } from "./Placeholder.jsx";
import { LensRunner } from "../lens/index.js";
import { TASKS, listLenses } from "../../lib/lensApi.js";
import { Icon } from "../shell/icons.jsx";

// PerspectivesSection — apply a named analytical method to a research OBJECT.
//
// Step 03 framed this and launched a chat; Step 07 makes it real. The change that matters is the
// order of operations: you pick the object first, then the method. The old flow ("choose a
// personality, then talk about something") could not produce a traceable review because nothing
// bound the output to a target or to the evidence it was allowed to cite.
//
// What a run produces is a SAVED, CITED REVIEW attached to that object — findings marked as cited
// fact or explicit inference, the evidence ids behind each one, what was missing, and what is still
// open. It is never merged into the user's thesis (§3.7).
//
// Committee is no longer a top-level product. It is the "Review with perspectives" task on a
// selected thesis, rendered through the same review card as every other lens, with its sequential
// analyst → debate → judge pipeline and deterministic consensus untouched.
//
// NOTHING on this screen calls a model on mount. The lens catalogue and saved reviews are journal
// reads; every review is a click (§3.8).

export default function PerspectivesSection({ ticker }) {
  const [theses, setTheses] = useState(null);
  const [selectedId, setSelectedId] = useState("");
  const [lenses, setLenses] = useState([]);

  useEffect(() => {
    let alive = true;
    setTheses(null);
    listThesesV2(ticker)
      .then((list) => {
        if (!alive) return;
        setTheses(list);
        const active = list.find((t) => t.status === "active") || list[0];
        setSelectedId(active?.id || "");
      })
      .catch(() => alive && setTheses([]));
    return () => { alive = false; };
  }, [ticker]);

  useEffect(() => {
    let alive = true;
    listLenses()
      .then((r) => alive && setLenses(r.lenses))
      .catch(() => alive && setLenses([]));
    return () => { alive = false; };
  }, []);

  const selected = (theses || []).find((t) => t.id === selectedId) || null;
  const custom = lenses.filter((l) => !l.builtin);

  return (
    <div className="flex flex-col gap-3">
      <SectionShell
        title={`${ticker} · Perspectives`}
        tags={<Tag tone="llm">research lenses</Tag>}
        note="A lens is a method of examining evidence, not a personality. Each one is applied to a specific object with a specific set of evidence, and it returns a saved review: findings marked as cited fact or explicit inference, what is missing, and what stays open. A review is attached to your thesis — it never edits it, and it cannot produce a price target or a buy/sell call."
      >
        {theses === null ? (
          <div className="px-[22px] py-3.5">
            <SkeletonText lines={3} />
          </div>
        ) : theses.length === 0 ? (
          <div className="px-[22px] py-5">
            <EmptyState title="Write a thesis first">
              A lens reviews something you believe. Once you have written a claim for {ticker} in the
              Thesis section, you can challenge it, have it explained, or put it in front of several
              analysts at once.
            </EmptyState>
          </div>
        ) : (
          <>
            <div className="flex flex-wrap items-center gap-2 border-b border-line bg-panel2/30 px-[22px] py-2">
              <Micro>Reviewing</Micro>
              <label htmlFor="perspectives-thesis" className="sr-only">
                Thesis to review
              </label>
              <select
                id="perspectives-thesis"
                value={selectedId}
                onChange={(e) => setSelectedId(e.target.value)}
                className="min-w-0 max-w-full flex-1 rounded-md border border-line bg-panel2 px-2 py-1 text-[12px] text-fg focus:outline-none focus-visible:border-accent"
              >
                {theses.map((t) => (
                  <option key={t.id} value={t.id}>
                    [{t.status}] {(t.claim || t.text || "").slice(0, 80)}
                  </option>
                ))}
              </select>
            </div>

            {selected && (
              <div className="px-[22px] py-3.5">
                {/* The user's own claim, shown as THEIR text, above the model's output — the
                    separation the ownership rule requires (guide §8.4). */}
                <div className="mb-3 rounded-lg border border-line bg-panel2/40 px-3 py-2">
                  <div className="flex items-center gap-2">
                    <Tag tone="outline">you author this</Tag>
                    <span className="text-[10.5px] uppercase tracking-wide text-muted">Your claim</span>
                  </div>
                  <p className="mt-1.5 break-words text-[12.5px] leading-relaxed text-fg/90">
                    {selected.claim || selected.text}
                  </p>
                </div>

                <LensRunner
                  target={{ type: "thesis", id: selected.id }}
                  ticker={ticker}
                  thesisId={selected.id}
                  title="Apply a lens to this thesis"
                />
              </div>
            )}
          </>
        )}
      </SectionShell>

      {/* The lens catalogue, for reference. Applying one happens on the object above, which is why
          these are described rather than clickable — a lens with no target is not a research act. */}
      <SectionShell title="Available lenses" tags={<Tag tone="outline">reference</Tag>}>
        <div className="flex h-8 items-center border-b border-line bg-panel2/30 px-[22px]">
          <Micro>Methods</Micro>
        </div>
        <ul className="divide-y divide-line">
          {TASKS.filter((t) => t.id !== "apply_custom_framework").map((t) => (
            <li key={t.id} className="flex flex-col gap-1 px-[22px] py-3">
              <span className="text-[12.5px] font-semibold text-fg">{t.label}</span>
              <span className="text-[11.5px] leading-relaxed text-muted">{t.blurb}</span>
            </li>
          ))}
        </ul>

        <div className="flex h-8 items-center gap-2 border-y border-line bg-panel2/30 px-[22px]">
          <Micro>Your specialists</Micro>
          <span className="text-[10.5px] text-muted/60">applied with "Apply my specialist framework"</span>
        </div>
        {custom.length === 0 ? (
          <p className="px-[22px] py-3.5 text-[11.5px] leading-relaxed text-muted">
            No custom lenses yet. Write one to apply your own sector framework, checklist, or
            investment philosophy to {ticker}. Your instructions shape the method and the tone — they
            cannot loosen the grounding or safety rules every lens runs under.
          </p>
        ) : (
          <ul className="divide-y divide-line">
            {custom.map((l) => (
              <li key={l.id} className="flex items-center gap-2 px-[22px] py-3">
                <span aria-hidden="true">{l.emoji || <Icon name="lens" size={14} className="text-dim" />}</span>
                <span className="text-[12.5px] font-semibold text-fg">{l.name}</span>
              </li>
            ))}
          </ul>
        )}
      </SectionShell>
    </div>
  );
}
