import { useEffect, useState } from "react";
import { Tag } from "../terminal/bits.jsx";
import {
  listRules,
  scheduleReview,
  watchCondition,
  deleteRule,
  AuthRequiredError,
} from "../../lib/monitoringApi.js";

// ItemMonitoring — the monitoring rules that watch ONE thesis item, rendered beside that item
// (Step 08 UI placement: "monitoring rules beside the assumption/catalyst/condition they watch").
//
// The honesty rule this component exists to enforce (§4.1):
//
//   A free-text condition that no evaluator can test is NOT dressed up as a threshold. The only
//   offer made against an assumption or a risk is a REVIEW REMINDER. A measurable threshold is
//   offered only where the deterministic analysis can actually evaluate it — which today means a
//   price level on an invalidation condition or a catalyst.
//
// Creating a rule from an item is also the moment D-10's precondition is established: naming the
// item in the create body is the user asserting the causal link. The browser never sends the flag
// itself — the server derives it — so this component cannot manufacture that assertion.
// A REVIEW REMINDER is only representable against an assumption. `alerts` maps intents to item
// prefixes (allowedIntents, rules.go): `assumption_review` requires an `asm_` item and
// `thesis_review` takes no item at all — so there is no valid item-scoped reminder for a catalyst,
// an invalidation condition, or a risk. Offering the button anyway would produce a 400 the user
// cannot act on, so the offer is gated on the same rule the server enforces.
const canRemind = (item) => String(item?.id || "").startsWith("asm_");

export default function ItemMonitoring({ ticker, thesisId, item, kind, canEvaluate = false }) {
  const [rules, setRules] = useState([]);
  const [status, setStatus] = useState("loading");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const load = () => {
    if (!thesisId || !item?.id) return;
    listRules(thesisId)
      .then((all) => {
        setRules(all.filter((r) => r.thesisItemId === item.id));
        setStatus("ready");
      })
      .catch((err) => setStatus(err instanceof AuthRequiredError ? "guest" : "down"));
  };
  useEffect(load, [thesisId, item?.id]); // eslint-disable-line react-hooks/exhaustive-deps

  if (status === "guest") return null;

  const run = async (fn) => {
    setBusy(true);
    setError("");
    try {
      await fn();
      load();
    } catch (err) {
      setError(err?.message || "could not save this rule");
    } finally {
      setBusy(false);
    }
  };

  const onRemind = () =>
    run(() =>
      scheduleReview({ ticker, thesisId, thesisItemId: item.id, everyDays: 30 }),
    );

  // Offered ONLY when the caller says a deterministic evaluator exists for this condition.
  const onWatchLevel = (level) =>
    run(() =>
      watchCondition({
        ticker,
        thesisId,
        item: item.id,
        kind,
        type: "price_cross",
        params: { level, direction: kind === "invalidation" ? "below" : "above" },
      }),
    );

  return (
    <div className="mt-1.5 flex flex-wrap items-center gap-2">
      {rules.map((r) => (
        <span key={r.id} className="inline-flex items-center gap-1.5">
          <Tag tone={r.type === "thesis_review" ? "outline" : "info"}>
            {r.type === "thesis_review" ? "review reminder" : "watching"}
          </Tag>
          <button
            type="button"
            disabled={busy}
            onClick={() => run(() => deleteRule(r.id))}
            className="text-[10.5px] text-muted/70 transition-colors hover:text-fg disabled:opacity-50"
            aria-label="Stop monitoring this item"
          >
            stop
          </button>
        </span>
      ))}

      {rules.length === 0 && status === "ready" && (
        <>
          {canRemind(item) && (
            <button
              type="button"
              disabled={busy}
              onClick={onRemind}
              className="rounded-full border border-line2 px-2 py-0.5 text-[10.5px] text-fg/80 transition-colors hover:border-accent/60 disabled:opacity-50"
            >
              Remind me to review this
            </button>
          )}
          {canEvaluate && (
            <button
              type="button"
              disabled={busy}
              onClick={() => {
                const raw = window.prompt(`Alert when ${ticker} price crosses which level?`);
                const level = Number.parseFloat(raw);
                if (Number.isFinite(level)) onWatchLevel(level);
              }}
              className="rounded-full border border-line2 px-2 py-0.5 text-[10.5px] text-fg/80 transition-colors hover:border-accent/60 disabled:opacity-50"
            >
              Watch a price level
            </button>
          )}
        </>
      )}

      {/* Stated plainly rather than hidden: the product does not pretend to measure prose. */}
      {rules.length === 0 && status === "ready" && !canEvaluate && canRemind(item) && (
        <span className="text-[10px] text-muted/60">
          this condition is not machine-checkable — a reminder is the honest option
        </span>
      )}

      {status === "down" && (
        <span className="text-[10px] text-muted/60">monitoring unavailable</span>
      )}
      {error && <span className="text-[10px] text-down">{error}</span>}
    </div>
  );
}
