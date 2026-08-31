// EvidenceSlot — the evidence boundary for a thesis that does not exist yet.
//
// Step 05 shipped this as an honest "not built yet" placeholder while Step 06 built evidence in a
// parallel lane. Wave 2 integration replaced it with the real ThesisEvidencePanel everywhere a SAVED
// thesis is shown; this component now covers the one remaining case, which is not a missing feature:
// a thesis being CREATED has no id, so there is nothing for an evidence link to point at yet.
//
// It still renders no evidence and fetches nothing. Thesis fields stay the user's own prose — URLs,
// headlines, and excerpts belong in evidence records, which own their source and capture time
// (PARALLEL_CONTRACTS.md §2).
export default function EvidenceSlot() {
  return (
    <div className="rounded-lg border border-dashed border-line px-3 py-3">
      <span className="text-[11px] font-semibold uppercase tracking-wide text-muted">Evidence</span>
      <p className="mt-1.5 text-[11.5px] leading-relaxed text-muted">
        Save this thesis first, then you can attach evidence to it — filings, transcripts, news, and
        computed facts, each keeping its own source and capture time. Keep your own wording in the
        fields above rather than pasting links into them.
      </p>
    </div>
  );
}
