import { cx } from "../../lib/cx.js";

// IconChip — the landing's `.method-mark` icon housing: a soft rounded square holding a stroked
// 24×24 icon. `kind` selects one of the six provenance variants, whose exact radial-gradient and
// border values are lifted from the landing's `.kind-*` rules.
//
// The colour carried here is the provenance GLYPH TINT, not the data palette: a filing chip is
// mint because SEC filings are mint in the provenance legend, not because something went up.
const KINDS = {
  filing: {
    color: "var(--color-prov-filing)",
    tint: "rgba(91,199,154,.18)",
    border: "rgba(91,199,154,.24)",
  },
  company: {
    color: "var(--color-prov-company)",
    tint: "rgba(91,108,255,.18)",
    border: "rgba(91,108,255,.24)",
  },
  market: {
    color: "var(--color-prov-market)",
    tint: "rgba(83,160,255,.18)",
    border: "rgba(83,160,255,.24)",
  },
  computed: {
    color: "var(--color-prov-computed)",
    tint: "rgba(224,112,60,.18)",
    border: "rgba(224,112,60,.24)",
  },
  ai: {
    color: "var(--color-prov-ai)",
    tint: "rgba(167,139,224,.18)",
    border: "rgba(167,139,224,.24)",
  },
  user: {
    color: "var(--color-prov-user)",
    tint: "rgba(244,244,242,.14)",
    border: "rgba(255,255,255,.22)",
  },
  // Neutral housing — the landing's bare `.method-mark`, for icons with no provenance meaning.
  neutral: {
    color: "var(--color-fg)",
    tint: "rgba(255,255,255,.055)",
    border: "rgba(255,255,255,.12)",
  },
};

const SIZES = {
  sm: { box: 32, radius: 10, icon: 16 },
  md: { box: 40, radius: 12, icon: 19 },
  lg: { box: 48, radius: 15, icon: 22 },
};

export function IconChip({ kind = "neutral", size = "md", className, children, title }) {
  const k = KINDS[kind] || KINDS.neutral;
  const s = SIZES[size] || SIZES.md;
  return (
    <span
      title={title}
      aria-hidden="true"
      className={cx("grid flex-none place-items-center overflow-hidden", className)}
      style={{
        width: s.box,
        height: s.box,
        borderRadius: s.radius,
        color: k.color,
        border: `1px solid ${k.border}`,
        background:
          kind === "neutral"
            ? k.tint
            : `radial-gradient(85% 85% at 30% 24%, ${k.tint}, rgba(255,255,255,.045) 60%, rgba(255,255,255,.02))`,
        boxShadow:
          "inset 0 1px 0 rgba(255,255,255,.08), 0 10px 28px -18px rgba(0,0,0,.7)",
      }}
    >
      <span
        className="grid place-items-center [&>svg]:h-full [&>svg]:w-full"
        style={{ width: s.icon, height: s.icon }}
      >
        {children}
      </span>
    </span>
  );
}
