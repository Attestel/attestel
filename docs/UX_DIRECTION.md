# UX direction — the Attestel design system

> **Note for the open-source release.** The marketing landing page (`landing/`) this document
> treats as the design source of truth is not part of the public repository. Its tokens live on
> in `web/src/globals.css`, which is the source of truth here; read the parity invariant below
> as a record of where the system came from.

## The parity invariant

**`landing/index.html` is the design source of truth.**
`web/src/globals.css` `@theme` mirrors its `:root`. Changing one without the other is a bug.

A user who clicks **Request Access** on the landing page and lands in the terminal must not perceive
a product boundary: same colours, same typography, same buttons, same icon language, same logo
treatment, same premium surface feel. When this document and the landing page disagree, the landing
page wins and this document is wrong.

## Colour discipline

Indigo means **brand** — focus, active state, interactive emphasis, provenance chrome.
Green and red mean **data** — price direction, and positive/negative state.

Never use indigo for a price value or a direction reading. Never use green for a focus ring or an
active nav item. (This is not cosmetic: before the Attestel palette, `accent` *was* the up colour, and
"Uptrend" and "focused input" were literally the same colour.)

| Token | Value | Meaning |
|---|---|---|
| `bg` | `#060708` | page base |
| `canvas` | `#0A0B0D` | inset canvases — chart wells, hero panels |
| `panel` | `#0E0F13` | flat panel surface |
| `panel2` | `#14161B` | inner chips, inputs, glass base |
| `line` | `rgba(255,255,255,.08)` | hairline divider — **translucent**, not opaque |
| `line2` | `rgba(255,255,255,.16)` | control border |
| `fg` | `#F4F4F2` | primary text (warm off-white) |
| `muted` | `#9A9CA3` | secondary text |
| `dim` | `#64666D` | tertiary / mono micro-labels. ~4.3:1 — fine for large mono micro-labels, **not** for body copy |
| `accent` | `#5B6CFF` | BRAND — focus, active, interactive emphasis |
| `accent-deep` | `#3D4BD6` | gradient end / pressed |
| `up` | `#5BC79A` | price up · strengthened · SEC-filing provenance |
| `down` | `#E5484D` | price down · negative |
| `warn` | `#E0703C` | warning · challenged · computed-rules provenance |
| `info` | `#53A0FF` | market-data provenance |
| `llm` | `#A78BE0` | AI-synthesis provenance |

Provenance glyph tints, used by `IconChip` and coloured source text:
`filing #9FDBBF · company #C9CFFF · market #A5D7FF · computed #FFC089 · ai #D5B8FF · user #F4F4F2`.

Radius scale: `sm 8 · md 12 · lg 16 · xl 20 · 2xl 28 · full 999`. The only intentional exception is
the landing's 14px field radius, which lives in `ui/Field.jsx` and `auth/AuthLayout.jsx`.

Shadows: `shadow-card` (glass cards), `shadow-lift` (hover), `shadow-nav` (floating nav),
`shadow-glow` (primary-button hover only).
Easing: `ease-premium` `cubic-bezier(.16,.8,.24,1)` · `ease-reveal` `cubic-bezier(.2,.75,.2,1)`.
Durations: hover `.18s` · control `.22s` · surface `.35s` · reveal `.75s`.

## Typography

- Sans **Google Sans**, mono **IBM Plex Mono** — both self-hosted from `web/src/fonts/` and bundled
  by Vite. Nothing is fetched at runtime (CLAUDE.md invariant #1); the token stacks keep a system
  fallback.
- Headings: weight **550**, `line-height 1.02–1.04`, `letter-spacing -.03em`.
- Body: 16px base. `line-height` stays **1.5**, not the landing's prose 1.65 — the landing leaves
  `body { line-height }` at `normal` and the terminal's dense data rows must not grow. Prose blocks
  opt in with `.prose-body`.
- **The signature micro-label**: mono, `10.5px`, weight 500, `letter-spacing .16em`, uppercase,
  colour `dim`. It appears on every section of the landing page and is the most recognisable
  typographic tell of the brand. Use the `Label` primitive (or the `.label-mono` class); never
  hand-roll it again.

## Surfaces

Four recipes, all lifted from the landing and defined once in `globals.css`:

| Class | Landing origin | Use |
|---|---|---|
| `.surface-card` | `.pcard` | top-level cards — liquid glass + top highlight, radius 24 |
| `.surface-card-blue` | `.claim-card` | a card that needs indigo emphasis |
| `.surface-deep` | `.method-card` | modals, drawers, menus — radius 30 |
| `.surface-canvas` | `.lens-slide` | inset wells (charts, hero panels) — radius 28 |
| `.glass-nav` | `.glass-nav` | the floating navigation slab |

The top-highlight pseudo-element sits at `z-index: -1` so it can never veil card content.
`Panel` exposes these as `variant="flat" | "glass" | "deep" | "canvas"`; **flat is the default**, so
nested panels stay dense until a screen opts in.

## Primitives (`web/src/components/ui/`)

`Button` is the landing's `.pill` family. **Primary is a WHITE pill** (`#F4F4F2` on `#060708`,
hover pure white plus `shadow-glow`) — indigo is reserved for state and focus, never for a call to
action. Secondary is the glass pill. Sizes `xs / sm / md / lg`, where `lg` is the 52px hero pill.

`Label` (the signature micro-label) · `IconChip` (the landing's `.method-mark` housing with its six
provenance tints) · `PageHeader` / `DestinationHeader` (folio + weight-550 title) · `Badge` and
`StatusPill` (full-round, mono 9px / .12em) · `Field` (the landing `.field` glass slab, indigo
focus) · `Tabs` (pill segmented control, indigo active).

## The density rule

**Premium chrome, dense data.** Outer shells, nav, cards, buttons, forms, empty states and modals
take the landing's radii, glass and type. Tables, data rows and numeric readouts keep their compact
sizing. This mirrors the landing itself: 28px outer canvases wrapping 1px hairline inner structure.

Do not inflate table row heights. Do not bump numeric font sizes. Do not touch `text-[11px]` /
`text-[12px]` inside data rows.

## Icons

Inline SVG only — there is **no icon library** and there never will be, because the app must run
with zero network (CLAUDE.md invariant #1). Every mark lives in
`web/src/components/shell/icons.jsx`, 24×24, `fill="none"`, `stroke-width 1.8`, with
`vector-effect: non-scaling-stroke` so the optical weight is identical at every size.

The six provenance marks (filing, company, market, computed, ai, user) are transplanted **verbatim**
from the landing's methodology cards and keep its `data-stroke` / `data-fill` convention, which the
base rules in `globals.css` drive.

There are **no emoji** in `web/src`. The `▲ ▼ ●` numeric-delta glyphs are kept — the landing uses
them itself.

## Honesty guardrails

The SYNTHETIC banner, the PAPER chip, the live/seed/synthetic provenance badges and the
"insufficient validation" signal state are guardrails, not decoration. They may be made prettier;
they may never be made quieter. Warning surfaces keep full-opacity `warn` orange and their icon, and
the `synthetic` provenance badge carries a border so it reads as the loudest of the four.

## Accessibility

- Focus is a 2px indigo outline at 3px offset, on every interactive element. Tab order is never
  changed for visual reasons.
- `prefers-reduced-motion: reduce` kills every animation and the hover lift (`.pill-lift`).
- WCAG-AA on anything a user must read. `dim` on `bg` is ~4.3:1 — large mono micro-labels only.
