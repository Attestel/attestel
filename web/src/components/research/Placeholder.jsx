import { EmptyState } from "../ui/index.js";
import { Tag } from "../terminal/bits.jsx";

// Placeholder — the honest empty state for a Research section whose backing contract does not exist yet.
//
// The rule this enforces (guide §11.4: "Do not fake graph semantics in the UI"): a section that cannot be
// filled from a real stored record says WHAT is missing and WHICH step owns it. It never renders invented
// data, never joins unrelated records in the client to look complete, and never implies a link that isn't
// persisted.
export default function Placeholder({ title, step, children, action }) {
  return (
    <div className="px-[22px] py-5">
      <EmptyState title={title} action={action}>
        {children}
        {step && (
          <span className="mt-2.5 block">
            <Tag tone="outline">not built yet · {step}</Tag>
          </span>
        )}
      </EmptyState>
    </div>
  );
}

// SectionShell — the common bordered frame every Research section uses, so the workspace reads as one
// file with a consistent internal hierarchy rather than a grid of unrelated cards.
export function SectionShell({ title, tags, right, note, children, className }) {
  return (
    <section className="overflow-hidden surface-card">
      <div className="flex flex-wrap items-center gap-x-2.5 gap-y-2 border-b border-line px-[22px] py-3">
        <span className="text-[14px] font-semibold text-fg">{title}</span>
        {tags}
        {right && <span className="ml-auto flex shrink-0 items-center gap-2">{right}</span>}
      </div>
      {note && (
        <p className="border-b border-line bg-panel2/30 px-[22px] py-2.5 text-[11.5px] leading-relaxed text-muted">
          {note}
        </p>
      )}
      <div className={className}>{children}</div>
    </section>
  );
}
