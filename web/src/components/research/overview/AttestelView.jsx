import { Badge, Button, Label, Panel } from "../../ui/index.js";
import { cx } from "../../../lib/cx.js";
import {
  CONFIDENCE_WORDS,
  DIRECTION_WORDS,
  EVIDENCE_LAYERS,
  EVIDENCE_WORDS,
  directionGate,
  horizonLabel,
} from "../../../lib/analystApi.js";

// AttestelView — the model-authored read, and the first thing the ticker page says.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// THE UNVALIDATED STATE IS THE PRIMARY CASE. This is the design fact this file exists to honour.
//
// Contract §9.20 gates the `direction` WORD on a stored ablation verdict under `data/eval/ablation`.
// Measured at this tree: that directory does not exist, nothing reads it, and no served field
// carries a verdict — and none will until Wave 5A runs the ablation ladder. So "Not yet validated"
// is not an error, not a fallback and not a loading state. It is what this product says, in the
// ordinary case, today.
//
// The same is true of the server-authored prose. On an abstention, `thesis` and `invalidation` are
// SERVER lines (§9.56 bindings 7 and 9), and on a withdrawn rationale (§9.58) the direction stands
// while the prose is replaced. Those lines are CONTENT. They are styled as prose the product means,
// because it does: the system reached its evidence, reasoned over it, and declined to call a
// direction. That is a result, and a grey placeholder or a "no data yet" frame would be a false
// statement about what happened.
//
// So: no skeleton treatment, no muted-to-invisible, no apology, no empty-state illustration
// anywhere in this file. `evidence`, `confidenceBucket`, `riskFlags`, `invalidation` and
// `whatChanged` render UNCONDITIONALLY and carry the value of the screen.
// ─────────────────────────────────────────────────────────────────────────────────────────────
//
// Two further rules this file is built around:
//
//   * IT RENDERS `thesis` AND `invalidation`, NEVER THE AUDIT COPIES. It receives an object already
//     projected by `analystApi.renderableThesis`, so the audit-only keys are not present to reach.
//     `scripts/check-web-bundle.sh` greps the built bundle for their names; this file names none.
//   * NO NUMERIC CONFIDENCE (AD-8, doc §16.6). `confidenceBucket` is an ordinal rendered as a word.
//     There is no percentage, no decimal, no 0–100 gauge and no ring in this file. `support`,
//     `resistance` and `invalidation` are printed VERBATIM from the server (invariant #3) — nothing
//     here rounds, re-derives or recomputes a level.

// DIRECTION_TONE — `up`/`down` are DATA colours and carry a directional reading; `muted` carries an
// abstention at full weight (locked decision 3). `accent` appears nowhere in this map: it is the
// brand colour for focus and active state, and it may never render a direction.
const DIRECTION_TONE = {
  bullish: "text-up",
  bearish: "text-down",
  neutral: "text-muted",
  unclear: "text-muted",
};

const EVIDENCE_TONE = {
  bullish: "text-up",
  positive: "text-up",
  bearish: "text-down",
  negative: "text-down",
  neutral: "text-muted",
  unclear: "text-dim",
};

// DirectionSlot renders the §9.20 gate, and it is the only place in the lane that decides whether
// the direction word may appear.
//
// The unvalidated line sits at the SAME heading scale as a real direction would, because §9.20's
// point is that the word has not earned its place — not that the read is lesser. Quieter type here
// would say "this is a degraded version of the real thing", and the honest statement is "this
// system has not yet proved it can call direction, so it does not."
function DirectionSlot({ envelope, thesis }) {
  const gate = directionGate(envelope, thesis);

  if (!gate.validated) {
    return (
      <div className="min-w-0">
        <Label tone="muted">Direction</Label>
        <p className="mt-1 text-[19px] font-semibold leading-tight tracking-[-0.01em] text-fg">
          Not yet validated
        </p>
        <p className="mt-1.5 max-w-[46ch] text-[12px] leading-[1.6] text-muted">
          A direction is shown only once an ablation verdict exists for this horizon. No verdict has
          been recorded, so the direction word is withheld — the evidence below is not.
        </p>
      </div>
    );
  }

  const word = DIRECTION_WORDS[thesis?.direction] || DIRECTION_WORDS.unclear;
  return (
    <div className="min-w-0">
      <Label tone="muted">Direction</Label>
      <p
        className={cx(
          "mt-1 text-[19px] font-semibold leading-tight tracking-[-0.01em]",
          DIRECTION_TONE[thesis?.direction] || "text-muted"
        )}
      >
        {word}
      </p>
      {/* Invariant #2 permits exactly one directional output, and only with its track record
          beside it. When this branch ever renders, the verdict that opened the gate is named. */}
      <p className="mt-1.5 text-[12px] leading-[1.6] text-muted">
        Validated by the {gate.verdict.rung} ablation rung
        {gate.verdict.horizon ? ` over ${horizonLabel(gate.verdict.horizon)}` : ""}.
      </p>
    </div>
  );
}

// EvidenceRow — the five-layer decomposition, as a DENSE row set. "Premium chrome, dense data":
// the card around it is a premium surface, this is a data table and is not inflated.
function EvidenceRow({ evidence }) {
  return (
    <div className="grid grid-cols-2 gap-x-4 gap-y-1.5 sm:grid-cols-3 lg:grid-cols-5">
      {EVIDENCE_LAYERS.map(({ key, label }) => {
        const stance = evidence?.[key] || "unclear";
        return (
          <div key={key} className="flex min-w-0 items-baseline justify-between gap-2 lg:flex-col lg:items-start lg:gap-0.5">
            <Label tone="dim" className="shrink-0">{label}</Label>
            <span className={cx("truncate text-[12.5px] font-medium", EVIDENCE_TONE[stance] || "text-muted")}>
              {EVIDENCE_WORDS[stance] || stance}
            </span>
          </div>
        );
      })}
    </div>
  );
}

// Levels prints server-computed support and resistance VERBATIM (invariant #3). The values are
// joined with a separator and nothing else happens to them: no rounding, no currency symbol, no
// re-derivation. The model quotes these; it never invents them, and neither does this file.
function Levels({ thesis }) {
  const support = Array.isArray(thesis?.support) ? thesis.support : [];
  const resistance = Array.isArray(thesis?.resistance) ? thesis.resistance : [];
  if (support.length === 0 && resistance.length === 0) return null;
  return (
    <div className="grid grid-cols-2 gap-2">
      {resistance.length > 0 && (
        <div className="rounded-md border border-line px-2.5 py-2">
          <Label tone="down">Resistance</Label>
          <div className="num mt-1 text-[13px] font-semibold text-fg">{resistance.join(" · ")}</div>
        </div>
      )}
      {support.length > 0 && (
        <div className="rounded-md border border-line px-2.5 py-2">
          <Label tone="up">Support</Label>
          <div className="num mt-1 text-[13px] font-semibold text-fg">{support.join(" · ")}</div>
        </div>
      )}
    </div>
  );
}

// Disclaimer — contract §9.18, rendered VERBATIM from the server and never composed here.
//
// `docs/DISCLOSURE_CLASSIFICATION.md` rule 3 requires it adjacent to the output it qualifies, so it
// sits inside this card rather than in a page footer. It is not conditional, not collapsible and
// not shortened.
function Disclaimer({ text }) {
  if (!text) return null;
  return (
    <p className="border-t border-line pt-3 text-[11.5px] leading-[1.6] text-muted">{text}</p>
  );
}

// NotAssessed — the state before any run, and a FULL PEER of the populated card.
//
// There is no read route that can discover an existing answer without either starting a run or
// holding its id, so this is a normal, common state rather than an edge case — and it is why the
// button below is the ONLY thing in this lane that can reach the model (invariant #4). No effect,
// no mount, no ticker change, no poll starts a run.
//
// THE RUNNING STATE IS A STATE, NOT A GAP. A run is a background job on the server (`runId`), so
// the card can say it is still going, say it survives leaving the page, and — when it ends badly —
// SAY SO. The failure this replaces was silent: the proxy closed the connection at two minutes and
// the card fell back to this screen with no error at all, which told the user the product had done
// nothing when it had in fact done everything except say so.
function NotAssessed({ ticker, onRun, pending, runId, error }) {
  return (
    <div className="flex flex-col gap-3">
      <div>
        <Label tone="muted">Attestel view</Label>
        <p className="mt-1 text-[19px] font-semibold leading-tight tracking-[-0.01em] text-fg">
          Not yet assessed
        </p>
        <p className="mt-1.5 max-w-[54ch] text-[12.5px] leading-[1.65] text-fg/70">
          No analyst run has been made for {ticker} at this cutoff. A run reads the price, news,
          fundamentals and macro layers in sequence through a local model, and takes a while — so it
          starts when you ask for it, never on its own.
        </p>
      </div>
      <div className="flex flex-wrap items-center gap-2.5">
        <Button variant="primary" size="sm" onClick={onRun} disabled={pending}>
          {pending ? "Running analysis…" : error ? "Run analysis again" : "Run analysis"}
        </Button>
        {pending && (
          <span className="text-[11.5px] text-muted">
            Sequential model calls — this can take a few minutes. The run continues on the server if
            you move around the app{runId ? ` (run ${runId})` : ""}.
          </span>
        )}
      </div>
      {error && (
        <p className="text-[12px] leading-relaxed text-warn">{error}</p>
      )}
    </div>
  );
}

// AttestelView is the card. `thesis` arrives ALREADY PROJECTED by `renderableThesis`.
export default function AttestelView({
  ticker,
  envelope,
  thesis,
  onRun,
  pending,
  runId = "",
  error,
  onAsk,
}) {
  const assessed = Boolean(thesis);

  // The disclosure travels with the output it qualifies. Before the first run there is no envelope
  // and therefore no server-supplied string; §9.18 defines it as a SERVER constant, so this lane
  // renders it verbatim when the server has sent it and does not compose a local imitation when it
  // has not. In that state no direction and no read-through is on screen for it to qualify. The
  // handoff records this as an open item for the integrator.
  const disclaimer = envelope?.disclaimer || thesis?.disclaimer || "";

  const audit = thesis?.audit || null;
  const degraded = envelope?.degraded === true || envelope?.available === false;

  return (
    <Panel
      variant="glass"
      title="Attestel view"
      badges={
        <>
          {/* AI-authored content carries the `llm` provenance treatment (invariant #4 of this
              lane's honesty set). It is a claim about WHO wrote this, and it stays visible. */}
          <Badge variant="llm">model-authored</Badge>
          {assessed && thesis?.horizon && (
            <Badge variant="outline">{horizonLabel(thesis.horizon)}</Badge>
          )}
          {pending && <Badge variant="outline">running</Badge>}
          {degraded && <Badge variant="warn">analyst unavailable</Badge>}
        </>
      }
      actions={
        assessed && onAsk ? (
          <Button variant="secondary" size="xs" onClick={onAsk}>
            Ask about this read
          </Button>
        ) : null
      }
    >
      {!assessed ? (
        <div className="flex flex-col gap-4">
          <NotAssessed ticker={ticker} onRun={onRun} pending={pending} runId={runId} error={error} />
          <Disclaimer text={disclaimer} />
        </div>
      ) : (
        <div className="flex flex-col gap-4">
          {/* Direction (gated) + confidence (ordinal word). */}
          <div className="flex flex-wrap items-start justify-between gap-x-8 gap-y-3">
            <DirectionSlot envelope={envelope} thesis={thesis} />
            <div className="min-w-0">
              <Label tone="muted">Confidence</Label>
              {/* An ORDINAL rendered as a word. Never a probability, never a percentage, never a
                  gauge — AD-8 and doc §16.6. */}
              <p className="mt-1 text-[19px] font-semibold leading-tight tracking-[-0.01em] text-fg">
                {CONFIDENCE_WORDS[thesis.confidenceBucket] || CONFIDENCE_WORDS.low}
              </p>
              {thesis.regime && (
                <p className="mt-1.5 text-[12px] leading-[1.6] text-muted">
                  Regime: {thesis.regime}
                </p>
              )}
            </div>
          </div>

          {/* The five-layer decomposition — words, not numbers, in doc §16.6's own order. */}
          <div className="border-t border-line pt-3.5">
            <Label tone="muted" className="mb-2">Evidence</Label>
            <EvidenceRow evidence={thesis.evidence} />
          </div>

          {/* THESIS. On an abstention or a withheld rationale this is a SERVER-AUTHORED line
              (§9.56 bindings 7 and 9, §9.58 binding 2). It is set at reading size, in `fg`, as
              prose — because it is prose the product means. Never the audit copy. */}
          {thesis.thesis && (
            <div className="border-t border-line pt-3.5">
              <Label tone="muted" className="mb-1.5">Thesis</Label>
              <p className="max-w-[76ch] text-[13.5px] leading-[1.7] text-fg/90">{thesis.thesis}</p>
            </div>
          )}

          {/* INVALIDATION. Renders unconditionally (§9.20) and, on the same branches, is likewise a
              server line saying that no falsification condition is stated. Verbatim: a level here
              is server-computed, and this file never re-derives one. */}
          {thesis.invalidation && (
            <div className="border-t border-line pt-3.5">
              <Label tone="muted" className="mb-1.5">What would invalidate this</Label>
              <p className="max-w-[76ch] text-[13.5px] leading-[1.7] text-fg/90">
                {thesis.invalidation}
              </p>
            </div>
          )}

          <Levels thesis={thesis} />

          {/* RISK FLAGS — ungated (§9.20), and a guardrail surface, so `warn` not decoration. */}
          {Array.isArray(thesis.riskFlags) && thesis.riskFlags.length > 0 && (
            <div className="border-t border-line pt-3.5">
              <Label tone="warn" className="mb-1.5">Risk flags</Label>
              <ul className="flex flex-wrap gap-1.5">
                {thesis.riskFlags.map((r, i) => (
                  <li key={i}>
                    <Badge variant="warn">{r}</Badge>
                  </li>
                ))}
              </ul>
            </div>
          )}

          {/* AUDIT — visible, not buried (this lane's honesty invariant 4). What model, what
              reasoning mode, what prompt version, what cutoff. */}
          {audit && (
            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-line pt-3 text-[11px] text-dim">
              {audit.modelUsed && <span className="text-llm">{audit.modelUsed}</span>}
              {audit.reasoningMode && <span>mode: {audit.reasoningMode}</span>}
              {audit.promptVersion && <span>prompt: {audit.promptVersion}</span>}
              {thesis.asOf && <span>as of {thesis.asOf}</span>}
            </div>
          )}

          <Disclaimer text={disclaimer} />
        </div>
      )}
    </Panel>
  );
}
