#!/usr/bin/env bash
# Wave 4 integration gate — the client re-derives no importance band (§AD-8, doc §16.6).
#
# WHY THIS IS A SEPARATE GATE FROM check-web-bundle.sh. That one asks "did a server-only field reach
# the screen". This one asks the opposite question: "did a server-owned RULE get copied into the
# client". Both are copy-paste failures and neither finds the other's.
#
# The history it exists to prevent, in one paragraph. `gateway/feeds.go` declares
# `importanceHighMin = 0.70` and `importanceMediumMin = 0.40` and bands every item with them.
# Wave 4 Lane 4A copied both numbers into `web/src/lib/eventsApi.js`, correctly, and STATED the drift
# risk in the file: nothing failed if one side moved and the other did not. Lane 4B copied them too,
# independently, and got 0.45 / 0.75 — so a card would have printed "High impact" while its own
# server-authored reason string read "High market importance" at 0.72, and the header counting it
# would have disagreed with the card. Nothing anywhere would have failed.
#
# The fix was to SERVE the band (`importanceBand` on every feed and explore item) and delete the
# client's constants. This gate is what keeps them deleted. A future edit that reintroduces a
# threshold comparison in the browser fails here, loudly, instead of two waves later on a screenshot.
#
# What is deliberately NOT forbidden: `MIN_IMPORTANCE_MATERIAL` / `MIN_IMPORTANCE_HIGH`. Those are
# REQUEST values — `/api/following` filters on a `minImportance` float and has no band-name form —
# so a client offering "High impact only" must name the number the server compares against. Nothing
# renders from them, a drift there changes which items a filter asks for and can never make a card
# disagree with the header that counts it, and if the route ever accepts a band name they go too.
set -euo pipefail

SRC="${1:-web/src}"
GO_FEEDS="${2:-gateway/feeds.go}"
rc=0

if [ ! -d "$SRC" ] || [ ! -f "$GO_FEEDS" ]; then
  echo "check-band-parity: need $SRC/ and $GO_FEEDS" >&2
  exit 2
fi

# ── 1. The server still owns exactly one definition of the thresholds ──────────────────────────
high=$(grep -oE 'importanceHighMin[[:space:]]*=[[:space:]]*[0-9.]+' "$GO_FEEDS" | head -1 | grep -oE '[0-9.]+$' || true)
med=$(grep -oE 'importanceMediumMin[[:space:]]*=[[:space:]]*[0-9.]+' "$GO_FEEDS" | head -1 | grep -oE '[0-9.]+$' || true)
if [ -z "$high" ] || [ -z "$med" ]; then
  echo "FAIL: could not read importanceHighMin / importanceMediumMin from $GO_FEEDS." >&2
  echo "      If they were renamed, this gate must be updated with them — it is the thing that" >&2
  echo "      notices when the two sides of this rule stop being one side." >&2
  rc=1
fi

bandfns=$(grep -c '^func importanceBand(' "$GO_FEEDS" || true)
if [ "$bandfns" != "1" ]; then
  echo "FAIL: $GO_FEEDS declares $bandfns importanceBand functions, want exactly 1." >&2
  rc=1
fi

# ── 2. The client compares no importance against a number ──────────────────────────────────────
# A comparison operator, a numeric literal, and the word `importance` on one line. This is the
# shape of a re-derived band and there is no legitimate instance of it in the browser.
# Comment lines are excluded: this rule is worth EXPLAINING in prose next to the code it governs,
# and a gate that punished the explanation would quietly delete the reason.
if hits=$(grep -rnE '[iI]mportance[^,;)]*(>=|<=|>|<)[[:space:]]*0?\.[0-9]' "$SRC" 2>/dev/null \
          | grep -vE '^[^:]+:[0-9]+:[[:space:]]*(//|\*|/\*)'); then
  echo "FAIL: the client compares an importance against a numeric threshold — the band is SERVED:" >&2
  echo "$hits" | sed 's/^/       /' >&2
  echo "       Read \`event.importanceBand\` (via importanceBand(event)) instead of banding a float." >&2
  rc=1
fi

# ── 3. importanceBand() in the client holds no numeric literal at all ──────────────────────────
# It is a lookup, not a computation. A number appearing inside it means someone reintroduced the
# rule, whatever the surrounding comment says.
api="$SRC/lib/eventsApi.js"
if [ -f "$api" ]; then
  body=$(awk '/^export function importanceBand\(/{f=1} f{print} f&&/^}/{exit}' "$api")
  if [ -z "$body" ]; then
    echo "FAIL: no importanceBand() found in $api — this gate has lost its subject." >&2
    rc=1
  elif echo "$body" | grep -qE '[0-9]*\.[0-9]'; then
    echo "FAIL: importanceBand() in $api contains a numeric literal. It must READ the served band:" >&2
    echo "$body" | sed 's/^/       /' >&2
    rc=1
  fi

  # ── 4. The deleted constants stay deleted ────────────────────────────────────────────────────
  if hits=$(grep -rn 'export const IMPORTANCE_MATERIAL\|export const IMPORTANCE_HIGH' "$SRC" 2>/dev/null); then
    echo "FAIL: the hand-synced importance thresholds are back in the client:" >&2
    echo "$hits" | sed 's/^/       /' >&2
    rc=1
  fi
fi

if [ "$rc" -eq 0 ]; then
  echo "check-band-parity: OK — server owns importanceHighMin=$high importanceMediumMin=$med;"
  echo "                   the client re-derives nothing and holds no copy of either."
fi
exit "$rc"
