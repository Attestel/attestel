import { cx } from "../../lib/cx.js";
import {
  DISCLOSURE_WITHHELD_NOTICE,
  directionLabel,
  directionTone,
  horizonLabel,
  strengthWord,
} from "../../lib/eventsApi.js";

// ImpactChips — the per-ticker direction chips (`NVDA → Negative`).
//
// THE ONLY PLACE `up`/`down` APPEAR ON A CARD. Severity is `warn` and lives on the impact band;
// direction is `up`/`down` and lives here. A high-impact POSITIVE event that renders red is a bug,
// which is why the two vocabularies are in two files.
//
// Its own file for the same reason as EventMeta: Lane 4B and the Today digest render the identical
// chip and must not reimplement the mapping.

const TONE_CLASS = {
  up: "border-up/45 text-up",
  down: "border-down/45 text-down",
  // neutral, unclear and absent all land here. An undetermined direction is NOT a zero effect —
  // it is nobody having determined one — so it reads muted rather than as a confident flat call.
  muted: "border-line2 text-muted",
};

const ARROW = { up: "▲", down: "▼", muted: "●" };

// ImpactChip is one company's read-through. `strengthWord` returns a WORD or null; there is no
// numeric path into this component (AD-8). `potentialStrength` is a model-authored score with no
// calibration behind it, and "0.8" on a chip would claim a precision that does not exist.
export function ImpactChip({ entry, className }) {
  if (!entry?.ticker) return null;
  const tone = directionTone(entry.potentialDirection);
  const word = directionLabel(entry.potentialDirection);
  if (!word) return null;
  const strength = strengthWord(entry.potentialStrength);
  const horizon = horizonLabel(entry.expectedHorizon);
  return (
    <span
      className={cx(
        "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-[4px] font-mono text-[9px] font-medium uppercase leading-none tracking-[.12em]",
        TONE_CLASS[tone] || TONE_CLASS.muted,
        className
      )}
      title={[entry.transmission, horizon && `Expected horizon: ${horizon}`]
        .filter(Boolean)
        .join(" — ")}
    >
      <span aria-hidden="true" className="text-[8px] leading-none">
        {ARROW[tone] || ARROW.muted}
      </span>
      <span>
        {entry.ticker} → {strength ? `${strength} ${word.toLowerCase()}` : word}
      </span>
    </span>
  );
}

// ImpactChips renders every company carrying a direction, followed companies first.
//
// §9.18 — THE DISCLAIMER IS ADJACENT, NOT ELSEWHERE. These chips are a directional read-through on
// a company, and `docs/DISCLOSURE_CLASSIFICATION.md` rule 3 requires the qualifying string beside
// the output it qualifies. `compact` shortens nothing about the disclosure; it only drops the
// horizon footnote.
export function ImpactChips({ tickers = [], className, disclosure = "", showDisclaimer = true }) {
  const withDirection = (tickers || []).filter(
    (t) => t?.potentialDirection && directionLabel(t.potentialDirection)
  );
  if (!withDirection.length) return null;

  // §9.18 + Wave 4 clause: A MODEL-INFLUENCED FIELD MAY NOT BE RENDERED ON A SURFACE THE SERVER DID
  // NOT SEND ITS DISCLOSURE TO. These chips are exactly such a field. `disclosure` is the server's
  // own string, threaded from the response that produced these rows; this component no longer holds
  // a copy to fall back on, because a copy is how three of them happened.
  //
  // With no disclosure the chips are WITHHELD and the withholding is STATED. Rendering them bare
  // breaks §9.18; composing the sentence here breaks it differently, by making the browser the
  // author of a disclosure that exists so exactly one place owns the words. A visible withdrawal
  // also makes the gap countable — if it is common rather than rare, the defect is in what the
  // server sends, and a silent suppression would be hiding it.
  const disclosed = typeof disclosure === "string" && disclosure.trim() !== "";
  if (!disclosed) {
    return (
      <p className={cx("m-0 max-w-[72ch] text-[12px] leading-[1.55] text-muted", className)}>
        {DISCLOSURE_WITHHELD_NOTICE}
      </p>
    );
  }

  const ordered = [...withDirection].sort((a, b) => Number(b.followed) - Number(a.followed));
  return (
    <div className={cx("flex flex-col gap-1.5", className)}>
      <div className="flex flex-wrap items-center gap-1.5">
        {ordered.map((t) => (
          <ImpactChip key={t.ticker} entry={t} />
        ))}
      </div>
      {showDisclaimer && <AnalystDisclaimer disclosure={disclosure} />}
    </div>
  );
}

// AnalystDisclaimer — §9.18's string, rendered ADJACENT to the output it qualifies.
//
// `muted`, not `dim`, and body type, not a mono micro-label. This is a REQUIRED disclosure
// (`docs/DISCLOSURE_CLASSIFICATION.md`) and it is prose the user must actually be able to read;
// `dim` on `bg` is ~4.3:1, which is for large mono micro-labels only. Invariant #4 permits making a
// guardrail prettier and never quieter, and setting a disclosure in the dimmest colour on the
// palette is making it quieter.
export function AnalystDisclaimer({ disclosure, className }) {
  // The SERVER's string, verbatim. No local default: this component cannot render a disclosure that
  // was not sent, and it must not invent one. Callers that have no string withhold the output
  // instead — see ImpactChips above.
  if (typeof disclosure !== "string" || disclosure.trim() === "") return null;
  return (
    <p className={cx("m-0 max-w-[72ch] text-[12px] leading-[1.55] text-muted", className)}>
      {disclosure}
    </p>
  );
}

// Transmission is the one sentence explaining why this event reaches this company (§3.8).
//
// It is enrichment-derived, so with `EVENT_ENRICH_ENABLED` false — the shipped default — it is
// usually ABSENT. Absent renders nothing; it never renders a placeholder pretending a reason exists.
export function Transmission({ entry, className }) {
  if (!entry?.transmission) return null;
  return (
    <p className={cx("m-0 text-[13px] leading-[1.6] text-muted", className)}>
      <span className="text-dim">{entry.ticker}: </span>
      {entry.transmission}
    </p>
  );
}
