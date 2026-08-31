import assert from "node:assert/strict";
import test from "node:test";

import { formatHash, parseHash } from "../src/lib/routes.js";

test("calendar event deep links survive parse and canonical formatting", () => {
  const route = parseHash("#calendar?event=evt%2F2026-08-23");
  assert.equal(route.view, "calendar");
  assert.equal(route.params.event, "evt/2026-08-23");
  assert.equal(formatHash(route.view, route.subview, route.tab, route.params),
    "calendar?event=evt%2F2026-08-23");
});

test("research ticker deep links preserve the requested section and ticker", () => {
  const route = parseHash("#research/thesis?ticker=nvda");
  assert.equal(route.view, "research");
  assert.equal(route.subview, "thesis");
  assert.equal(route.params.ticker, "NVDA");
  assert.equal(formatHash(route.view, route.subview, route.tab, route.params),
    "research/thesis?ticker=NVDA");
});

test("invalid ticker parameters are discarded", () => {
  const route = parseHash("#research/overview?ticker=not%20a%20ticker");
  assert.equal(route.params, undefined);
});
