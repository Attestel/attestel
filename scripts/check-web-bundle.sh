#!/usr/bin/env bash
# Wave 4 integration gate — no server-side-only field may reach the built bundle.
#
# `modelThesis` and `modelInvalidation` are audit records of what the MODEL said. §9.56 binding 8
# and §9.58 binding 3 exempt them from the served-number rule precisely because sanitising them
# would destroy their purpose — which makes them the two fields in the payload most dangerous to
# put on a screen: they are the only prose in the response that is guaranteed unchecked.
# `directionBranch` is machine-readable plumbing (§9.56 binding 4); rendering it would turn an
# internal enum into an API. `proseSuppressions` and `warnings` are diagnostics (§9.58 binding 4).
#
# This is a grep, not a visual check, because that is what catches the failure mode: a field
# rendered inside a collapsed panel or a debug block looks like nothing at all until someone
# expands it. A visual pass over a working page will not find it; this will.
#
# Run after `npm run build` — `npm run build` is the only automated web check this repo has.
set -euo pipefail

DIST="${1:-web/dist}"

if [ ! -d "$DIST" ]; then
  echo "check-web-bundle: $DIST does not exist — run 'npm run build' in web/ first" >&2
  exit 2
fi

FORBIDDEN=(modelThesis modelInvalidation directionBranch proseSuppressions)
rc=0

for name in "${FORBIDDEN[@]}"; do
  # -F: fixed string. -r: the bundle is many files. Minifiers rename locals but never rename a
  # property read off a server payload, so the literal survives if the field is touched at all.
  if hits=$(grep -rlF "$name" "$DIST" 2>/dev/null); then
    echo "FAIL: '$name' is present in the built bundle — it is server-side only, never rendered:" >&2
    echo "$hits" | sed 's/^/       /' >&2
    rc=1
  fi
done

if [ "$rc" -eq 0 ]; then
  echo "check-web-bundle: OK — none of ${FORBIDDEN[*]} reach $DIST"
fi
exit "$rc"
