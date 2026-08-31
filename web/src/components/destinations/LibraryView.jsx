import { SubNav } from "../shell/SubNav.jsx";
import LibraryBrowser from "../library/LibraryBrowser.jsx";
import LensManager from "../library/LensManager.jsx";
import { DestinationHeader } from "../shell/DestinationHeader.jsx";

// LibraryView — the Library destination: retrieve research you already saved, and reuse the methods
// you trust.
//
// Two surfaces, matching the two things people come here to do:
//   * a single searchable list over every durable research object (evidence, lens reviews, perspective
//     reviews, transcript analyses, daily reads);
//   * the lens catalogue, where a Custom Specialist is written once and re-applied.
//
// Library READS. It owns no store, writes no record of its own, and copies nothing into a second place
// to make it appear here — every row opens the record where it actually lives, and an object type with
// no store yet says so rather than showing an empty shelf that looks finished.
//
// SUBVIEW NOTE. The route table still names the first subview `transcripts` ("Saved transcripts"),
// which predates this step: retrieval now spans every object type, not just transcripts. lib/routes.js
// is reserved for the wave integrator, so this view does not rename it. Instead the subview SEEDS the
// type filter — arriving from "Saved transcripts" opens the list filtered to transcript analyses, which
// is honest about where you came from and still widens to everything in one click. The exact
// route-table change to make this read "Saved research" is recorded in the handoff.
const TYPES_FOR_SUBVIEW = {
  transcripts: ["transcript"],
  saved: [],
};

export default function LibraryView({
  subview,
  onSubview,
  level,
  ticker,
  onOpenCopilot,
  // Optional navigators. App.jsx already builds these for other destinations and simply does not
  // forward them here yet (see the handoff); until it does, the browser falls back to hash navigation
  // and tells the user the active company will not change.
  onOpenThesis,
  onSelectTicker,
  onApplyLens,
}) {
  const wired = !!(onOpenThesis || onSelectTicker);

  const openResearch = (sym, target) => {
    if (target === "thesis" && onOpenThesis) return onOpenThesis(sym || ticker);
    if (onSelectTicker) return onSelectTicker(sym || ticker);
    window.location.hash = `#research/${target || "investigations"}`;
    return undefined;
  };

  return (
    <div className="flex flex-col gap-3">
      <DestinationHeader view="library" className="mb-1" />

      <SubNav view="library" subview={subview} onChange={onSubview} level={level} />

      {subview === "lenses" ? (
        <LensManager onApplyLens={onApplyLens} onOpenCopilot={onOpenCopilot} />
      ) : (
        <LibraryBrowser
          initialTypes={TYPES_FOR_SUBVIEW[subview] || []}
          activeTicker={ticker}
          onOpenResearch={wired ? openResearch : undefined}
          onApplyLens={onApplyLens}
        />
      )}
    </div>
  );
}
