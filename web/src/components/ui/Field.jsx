import { cx } from "../../lib/cx.js";

// Shared control styling for inputs/selects/textareas across forms (alerts, journal, evidence).
//
// This is the landing's `.field` recipe (glass slab, 14px radius, indigo focus) applied to the
// control itself rather than to a wrapper: the app applies `controlClass` directly to bare
// <input>/<select>/<textarea> in ~24 places, so putting the border on a wrapper would either
// double-border or leave standalone controls invisible.
export const controlClass =
  "w-full rounded-[14px] border border-line2 bg-[rgba(20,22,27,.65)] px-3.5 py-2.5 text-[13.5px] text-fg backdrop-blur-[10px] transition-colors placeholder:text-dim hover:border-white/24 focus:outline-none focus-visible:border-accent/70 focus:border-accent/70";

// Field — labelled form control wrapper.
//
// The label is the landing's signature micro-label (`.label-mono`, 10.5px) rather than its
// 8.5px `.field label`: the landing only labels single words ("EMAIL", "NAME"), while this app's
// field labels are full sentences ("What would you do differently? (one lesson per line)"), which
// are not readable at 8.5px uppercase mono. Same family, the legible member of it.
export function Field({ label, children, className }) {
  return (
    <label className={cx("flex min-w-0 flex-col gap-1.5", className)}>
      {label && <span className="label-mono block leading-[1.4]">{label}</span>}
      {children}
    </label>
  );
}
