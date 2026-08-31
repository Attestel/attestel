import { PageHeader } from "../ui/index.js";
import { DESTINATIONS, destinationById } from "../../lib/routes.js";

// DestinationHeader — the landing's `.sec-head` pattern applied to a destination: a folio
// micro-label ("03 · Monitor") above the destination name, with an optional right-aligned action.
//
// Both strings come from lib/routes.js — the destination's position in the NINE (§9.34) and the
// research-loop stage it serves — so the header cannot drift from the route table, and nothing new
// has to be invented or maintained here.
//
// `subtitle` is a passthrough, added at Wave 4 integration: Explore wants one and had been
// hand-writing its whole header to get it, which is how it ended up printing the folio as "Orient"
// while every neighbour printed "01 · Orient", "02 · Monitor", "04 · Orient · Monitor". A header
// that computes its own number is a header that will eventually compute a different one.
export function DestinationHeader({ view, subtitle, actions, className }) {
  const dest = destinationById(view);
  if (!dest) return null;
  const index = DESTINATIONS.findIndex((d) => d.id === view) + 1;
  const folio = [String(index).padStart(2, "0"), dest.stage].filter(Boolean).join(" · ");
  return (
    <PageHeader
      folio={folio}
      title={dest.label}
      subtitle={subtitle}
      actions={actions}
      className={className}
    />
  );
}
