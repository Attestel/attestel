// Minimal stroked line-icons (inherit currentColor). Kept inline so the app needs no icon-font or
// network fetch (zero-key / offline invariant). 24×24 viewBox.
const P = {
  feedback: (
    <>
      <path d="M4 4h16v12H9l-5 4z" />
      <path d="M8 9h8M8 12h5" />
    </>
  ),
  terminal: (
    <>
      <path d="M3 5h18v14H3z" />
      <path d="M8 10v4M12 8v8M16 11v3" />
    </>
  ),
  watchlist: (
    <>
      <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z" />
      <circle cx="12" cy="12" r="2.6" />
    </>
  ),
  fundamentals: (
    <>
      <path d="M4 20V10M10 20V4M16 20v-7M22 20H2" />
    </>
  ),
  news: (
    <>
      <path d="M4 5h13v14H5a1 1 0 0 1-1-1z" />
      <path d="M17 8h3v9a2 2 0 0 1-2 2M7 9h7M7 13h7M7 17h4" />
    </>
  ),
  brief: (
    <>
      <path d="M5 3h9l5 5v13H5z" />
      <path d="M14 3v5h5" />
      <path d="M8 13l1.8 1.8L13 11" />
    </>
  ),
  alerts: (
    <>
      <path d="M18 8a6 6 0 1 0-12 0c0 7-3 9-3 9h18s-3-2-3-9" />
      <path d="M13.7 21a2 2 0 0 1-3.4 0" />
    </>
  ),
  journal: (
    <>
      <path d="M4 4.5A2.5 2.5 0 0 1 6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5z" />
      <path d="M8 2v20M12 7h5M12 11h5" />
    </>
  ),
  paper: (
    <>
      <path d="M9 3h6M10 3v6l-5 9a2 2 0 0 0 1.8 3h10.4A2 2 0 0 0 19 18l-5-9V3" />
      <path d="M7.5 15h9" />
    </>
  ),
  calendar: (
    <>
      <rect x="3" y="4.5" width="18" height="16" rx="2" />
      <path d="M3 9.5h18M8 2.5v4M16 2.5v4" />
    </>
  ),
  // Today — sunrise over a horizon: "what changed since I last looked".
  today: (
    <>
      <path d="M12 2.5v2.2M4.9 5.9l1.6 1.6M19.1 5.9l-1.6 1.6" />
      <path d="M7 14a5 5 0 0 1 10 0" />
      <path d="M2.5 14h19M6 18h12" />
    </>
  ),
  // Research — a magnifier over data: look closely at the evidence.
  // following — the companies the user has chosen to watch. A star, because "followed" is a
  // user-made mark on a company and not a system state. 24x24, fill="none", stroke 1.8, like the
  // rest; `explore` reuses the existing `compass` mark rather than adding a second one.
  following: (
    <>
      <path d="M12 4.5 14.4 9.4l5.4.8-3.9 3.8.9 5.4-4.8-2.5-4.8 2.5.9-5.4L4.2 10.2l5.4-.8z" />
    </>
  ),

  research: (
    <>
      <circle cx="10.5" cy="10.5" r="6.5" />
      <path d="M20.5 20.5l-5.2-5.2" />
      <path d="M8 13v-2.5M10.5 13V8M13 13v-3.5" />
    </>
  ),
  // Library — an archive of saved research.
  library: (
    <>
      <rect x="3" y="3.5" width="18" height="5.5" rx="1.5" />
      <path d="M4.7 9v9.5a2 2 0 0 0 2 2h10.6a2 2 0 0 0 2-2V9" />
      <path d="M10 13.5h4" />
    </>
  ),
  reload: (
    <>
      <path d="M21 12a9 9 0 1 1-2.6-6.4M21 4v5h-5" />
    </>
  ),
  copilot: (
    <>
      <path d="M4 5h16a1 1 0 0 1 1 1v9a1 1 0 0 1-1 1H9l-4 4v-4H4a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1z" />
      <path d="M12 8.2l.85 1.95 1.95.85-1.95.85L12 14.6l-.85-1.95-1.95-.85 1.95-.85z" />
    </>
  ),
  bell: (
    <>
      <path d="M18 8a6 6 0 1 0-12 0c0 7-3 9-3 9h18s-3-2-3-9" />
      <path d="M13.7 21a2 2 0 0 1-3.4 0" />
    </>
  ),
  // ── provenance marks ──────────────────────────────────────────────────────
  // Transplanted VERBATIM from the landing page's methodology cards
  // (`.method-card .method-mark svg`), including its data-stroke / data-fill
  // convention, so the same six marks render identically on both surfaces.
  filing: (
    <>
      <path
        data-stroke
        d="M8 3.75h5.8L18 7.95v12.3a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 6 20.25V5.25a1.5 1.5 0 0 1 1.5-1.5Z"
      />
      <path data-stroke d="M13.5 3.75v4.2H18" />
      <path data-stroke d="M9 11.25h6" />
      <path data-stroke d="M9 15h6" />
    </>
  ),
  company: (
    <>
      <path data-stroke d="M5 17.25V9.75l7-4 7 4v7.5" />
      <path data-stroke d="M7.75 17.25v-4.5h8.5v4.5" />
      <path data-stroke d="M12 5.75v7" />
      <circle data-fill cx="12" cy="18.5" r="1.5" />
    </>
  ),
  market: (
    <>
      <path data-stroke d="M4.5 18.75h15" />
      <path data-stroke d="M7 15.75l3-3 3 2.5 4-5" />
      <path data-stroke d="M15.5 10.25H17.5V12.25" />
      <circle data-fill cx="7" cy="15.75" r="1.2" />
      <circle data-fill cx="10" cy="12.75" r="1.2" />
      <circle data-fill cx="13" cy="15.25" r="1.2" />
      <circle data-fill cx="17" cy="10.25" r="1.2" />
    </>
  ),
  computed: (
    <>
      <rect data-stroke x="6" y="3.75" width="12" height="16.5" rx="2.5" />
      <path data-stroke d="M8.75 8.25h6.5" />
      <path data-stroke d="M9 12.25h.01" />
      <path data-stroke d="M12 12.25h.01" />
      <path data-stroke d="M15 12.25h.01" />
      <path data-stroke d="M9 15.75h.01" />
      <path data-stroke d="M12 15.75h.01" />
      <path data-stroke d="M15 15.75h.01" />
    </>
  ),
  ai: (
    <>
      <path data-stroke d="M12 4.75v2.25" />
      <path data-stroke d="M7.05 6.8l1.6 1.6" />
      <path data-stroke d="M16.95 6.8l-1.6 1.6" />
      <path data-stroke d="M12 19.25V17" />
      <path
        data-stroke
        d="M7 12a5 5 0 0 1 10 0c0 1.8-.96 2.95-2.1 3.95-.55.48-.9 1.03-.9 1.8h-4c0-.77-.35-1.32-.9-1.8C7.96 14.95 7 13.8 7 12Z"
      />
      <path data-stroke d="M9.5 20.25h5" />
    </>
  ),
  user: (
    <>
      <circle data-stroke cx="12" cy="8.25" r="3.25" />
      <path data-stroke d="M6.75 18.25c1.3-2.45 3.15-3.75 5.25-3.75s3.95 1.3 5.25 3.75" />
      <path data-stroke d="M17.75 11.5v3.8" />
      <path data-stroke d="M15.85 13.4h3.8" />
    </>
  ),

  // ── glyph replacements ────────────────────────────────────────────────────
  // Each of these retires an emoji or a typographic glyph that used to stand in
  // for an icon. Same 24×24 stroked language as everything above.
  //
  // lens — a perspective/persona mark (retires the brain emoji).
  lens: (
    <>
      <circle cx="12" cy="12" r="4" />
      <path d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M18.4 5.6l-2.1 2.1M7.7 16.3l-2.1 2.1" />
    </>
  ),
  // compass — the default copilot persona (retires the compass emoji).
  compass: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M15.6 8.4l-2 5.2-5.2 2 2-5.2z" />
    </>
  ),
  // graduation — levelling up out of beginner mode (retires the mortarboard emoji).
  graduation: (
    <>
      <path d="M2.5 8.6L12 4.2l9.5 4.4-9.5 4.4z" />
      <path d="M6.5 10.8v4.4c0 1.5 2.5 2.8 5.5 2.8s5.5-1.3 5.5-2.8v-4.4" />
      <path d="M20.5 9v5" />
    </>
  ),
  // flask — simulated / paper mode (retires the test-tube emoji).
  flask: (
    <>
      <path d="M9.5 3h5M10.5 3v6.2L5.8 17.6A2 2 0 0 0 7.5 20.6h9a2 2 0 0 0 1.7-3L13.5 9.2V3" />
      <path d="M8.2 14.5h7.6" />
    </>
  ),
  // flame — an unusual burst of attention (retires the fire emoji).
  flame: (
    <>
      <path d="M12 2.8s5.2 3.9 5.2 8.6a5.2 5.2 0 0 1-10.4 0C6.8 8.6 9 6.8 12 2.8z" />
      <path d="M12 20.6a2.7 2.7 0 0 0 2.7-2.7c0-1.7-2.7-3.6-2.7-3.6s-2.7 1.9-2.7 3.6A2.7 2.7 0 0 0 12 20.6z" />
    </>
  ),
  // star — the feedback rating mark (retires the black-star glyph); filled with fill-current.
  star: (
    <>
      <path d="M12 3.4l2.6 5.4 5.9.8-4.3 4.1 1.1 5.9L12 16.8l-5.3 2.8 1.1-5.9L3.5 9.6l5.9-.8z" />
    </>
  ),
  // pencil — an edit / simulated-entry mark (retires the pencil glyph).
  pencil: (
    <>
      <path d="M4 20h4.2L20 8.2a2.1 2.1 0 0 0 0-3L18.8 4a2.1 2.1 0 0 0-3 0L4 15.8z" />
      <path d="M15.2 4.8l4 4" />
    </>
  ),
  // caret — a small inline "next" marker (retires the right-pointing triangle).
  caret: (
    <>
      <path d="M9.5 5.5l7 6.5-7 6.5" />
    </>
  ),
  // reply — a nested / derived item (retires the corner-arrow glyph).
  reply: (
    <>
      <path d="M6 4v8.5a2.5 2.5 0 0 0 2.5 2.5H18" />
      <path d="M14.5 11.5l3.5 3.5-3.5 3.5" />
    </>
  ),
  // diamond — the neutral "informational" status mark (retires the diamond glyph).
  diamond: (
    <>
      <path d="M12 3.6l8.4 8.4-8.4 8.4L3.6 12z" />
    </>
  ),
  // circle — an unchecked checklist step (pairs with `check`).
  circle: (
    <>
      <circle cx="12" cy="12" r="8" />
    </>
  ),
  close: (
    <>
      <path d="M6 6l12 12M18 6L6 18" />
    </>
  ),
  check: (
    <>
      <path d="M4.5 12.5l5 5 10-11" />
    </>
  ),
  warning: (
    <>
      <path d="M12 3.6L1.9 20.4h20.2z" />
      <path d="M12 9.5v4.6M12 17.3h.01" />
    </>
  ),
  info: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 11v5.5M12 7.8h.01" />
    </>
  ),
  settings: (
    <>
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.6 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.6a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
    </>
  ),
};

// Icon — 24×24, stroked, inherits currentColor. `strokeWidth` defaults to the landing's 1.8 and
// `vector-effect: non-scaling-stroke` keeps the optical weight identical at every render size, so
// an icon lifted from the landing looks like the landing's.
//
// Marks that carry `data-stroke` / `data-fill` (the six provenance marks) are driven by the base
// rules in globals.css instead of these root attributes — same result, landing's own convention.
export function Icon({ name, size = 18, className, strokeWidth = 1.8 }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={strokeWidth}
      strokeLinecap="round"
      strokeLinejoin="round"
      vectorEffect="non-scaling-stroke"
      className={className}
      aria-hidden="true"
    >
      {P[name] || null}
    </svg>
  );
}
