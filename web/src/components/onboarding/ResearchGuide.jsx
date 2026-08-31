import { useOnboarding } from "./OnboardingContext.jsx";
import { STEPS, STEP_IDS, stepDef } from "../../lib/onboardingApi.js";
import { cx } from "../../lib/cx.js";
import { Icon } from "../shell/icons.jsx";

// ResearchGuide — the contextual guidance for a first company research review.
//
// It is deliberately NOT a feature tour. There is no modal, no carousel, no overlay, and nothing is
// blocked while it is on screen: it is a strip at the top of the Research workspace that names the
// one thing to do next and sends you to the section where you do it. Every destination, every
// section, and every action stays reachable the whole time, and "Not now" turns it off for good.
//
// The three activation conditions are rendered from the SERVER'S answer (hasThesis /
// hasEvidenceLink / hasChallenge), never from what this component believes it has walked the user
// through. That is the difference between a progress bar and a fact: if the thesis did not save, the
// guide says so instead of congratulating anyone.

function Check({ done }) {
  return (
    <span
      aria-hidden="true"
      className={cx(
        "flex h-[18px] w-[18px] flex-none items-center justify-center rounded-full border",
        done ? "border-accent/60 bg-accent/15 text-accent" : "border-line2 text-muted/60"
      )}
    >
      <Icon name={done ? "check" : "circle"} size={done ? 10 : 7} />
    </span>
  );
}

// Condition renders one activation condition with its durable state spelled out in words — never
// colour alone.
function Condition({ done, label, pending }) {
  return (
    <li className="flex items-start gap-2.5">
      <Check done={done} />
      <span className={cx("text-[12.5px] leading-[18px]", done ? "text-fg" : "text-muted")}>
        {label}
        <span className="sr-only">{done ? " — done" : " — not yet"}</span>
        {!done && pending && <span className="text-muted/70"> — {pending}</span>}
      </span>
    </li>
  );
}

export default function ResearchGuide({ ticker, subview, onSubview }) {
  const ob = useOnboarding();
  if (!ob || !ob.visible) return null;

  const { state, progress, guest, report, dismiss, promptSignIn } = ob;

  // Before anything has started, the strip carries the positioning and the two primary actions
  // (§6.5). It leads with what the product is for, not with what it refuses to do and not with a
  // feature list — and both actions start real work rather than a tour.
  if (state.status === "not_started") {
    return (
      <section
        aria-label="Start a company research review"
        className="overflow-hidden rounded-xl border border-accent/30 bg-accent/[0.06] px-[22px] py-5"
      >
        <div className="label-mono text-accent">
          ● The operating system for investment research
        </div>
        <h2 className="mt-2 max-w-[54ch] text-[16px] font-semibold leading-[1.4] text-fg">
          Build your thesis. Challenge your assumptions. Track what changes. Learn from every decision.
        </h2>
        <p className="mt-2 max-w-[58ch] text-[12.5px] leading-relaxed text-muted">
          Seven short steps on one company — a claim, what would break it, one piece of source-backed
          evidence, and a lens that argues against you. You can leave and pick it up any time.
        </p>
        <div className="mt-4 flex flex-wrap items-center gap-2.5">
          <button
            type="button"
            onClick={() => {
              report({ step: STEPS[0].id, ticker });
              onSubview?.(STEPS[0].section);
            }}
            className="rounded-full border border-accent/50 bg-accent/12 px-4 py-2 text-[12.5px] font-semibold text-accent transition-colors hover:bg-accent/20"
          >
            Start a company research review
          </button>
          <button
            type="button"
            onClick={() => {
              report({ step: "write_claim", ticker });
              onSubview?.("thesis");
            }}
            className="rounded-full border border-line2 px-4 py-2 text-[12.5px] font-medium text-fg transition-colors hover:border-accent/50"
          >
            Create your first thesis
          </button>
          <button
            type="button"
            onClick={dismiss}
            className="text-[11.5px] text-muted underline decoration-dotted underline-offset-2 transition-colors hover:text-fg"
          >
            Not now
          </button>
        </div>
      </section>
    );
  }

  const step = stepDef(state.step);
  const index = STEP_IDS.indexOf(step.id);
  const onSection = subview === step.section;
  const needsAccount = guest && step.needsAccount;

  const advance = () => {
    const next = STEPS[Math.min(index + 1, STEPS.length - 1)];
    report({ step: next.id, ticker });
    if (next.section && next.section !== subview) onSubview?.(next.section);
  };

  const goToSection = () => {
    report({ step: step.id, ticker });
    onSubview?.(step.section);
  };

  return (
    <section
      aria-label="Guided research review"
      className="overflow-hidden rounded-xl border border-accent/30 bg-accent/[0.06]"
    >
      <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1.5 border-b border-accent/20 px-[22px] py-2.5">
        <span className="label-mono text-accent">
          ● Your first research review
        </span>
        <span className="label-mono text-muted">
          step {index + 1} of {STEPS.length} · {step.stage}
        </span>
        <button
          type="button"
          onClick={dismiss}
          className="ml-auto text-[11.5px] text-muted underline decoration-dotted underline-offset-2 transition-colors hover:text-fg"
        >
          Not now
        </button>
      </div>

      <div className="flex flex-col gap-4 px-[22px] py-4 sm:flex-row sm:items-start sm:justify-between">
        <div className="min-w-0">
          <h2 className="text-[15px] font-semibold text-fg">
            {step.title}
            {ticker ? <span className="text-muted"> · {ticker}</span> : null}
          </h2>
          <p className="mt-1 max-w-[52ch] text-[12.5px] leading-relaxed text-muted">{step.help}</p>

          {needsAccount ? (
            <p className="mt-3 text-[12.5px] leading-relaxed text-fg/85">
              This step saves a record.{" "}
              <button
                type="button"
                onClick={promptSignIn}
                className="text-accent underline decoration-dotted underline-offset-2 hover:brightness-125"
              >
                Create an account
              </button>{" "}
              to keep your thesis, its evidence, and what changes it.
            </p>
          ) : (
            <div className="mt-3 flex flex-wrap items-center gap-2.5">
              {onSection ? (
                <button
                  type="button"
                  onClick={advance}
                  className="rounded-full border border-accent/50 bg-accent/12 px-4 py-2 text-[12.5px] font-semibold text-accent transition-colors hover:bg-accent/20"
                >
                  Done — next step
                </button>
              ) : (
                <button
                  type="button"
                  onClick={goToSection}
                  className="rounded-full border border-accent/50 bg-accent/12 px-4 py-2 text-[12.5px] font-semibold text-accent transition-colors hover:bg-accent/20"
                >
                  Go to {step.section}
                </button>
              )}
              {index > 0 && (
                <button
                  type="button"
                  onClick={() => {
                    const prev = STEPS[index - 1];
                    report({ step: prev.id, ticker });
                    if (prev.section !== subview) onSubview?.(prev.section);
                  }}
                  className="text-[12px] text-muted underline decoration-dotted underline-offset-2 transition-colors hover:text-fg"
                >
                  Back
                </button>
              )}
            </div>
          )}
        </div>

        {/* The activation conditions, straight from the server's read of durable records. */}
        <ul className="flex w-full shrink-0 flex-col gap-2 rounded-lg border border-line2 bg-panel/60 px-4 py-3 sm:w-[290px]">
          <li className="label-mono text-muted">
            Saved so far
          </li>
          <Condition done={progress?.hasThesis} label="A thesis of your own" pending="write one claim" />
          <Condition
            done={progress?.hasEvidenceLink}
            label="Source-backed evidence attached to it"
            pending="a model's opinion does not count"
          />
          <Condition done={progress?.hasChallenge} label="One challenge review completed" pending="run Devil's Advocate" />
        </ul>
      </div>
    </section>
  );
}
