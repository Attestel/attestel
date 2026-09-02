import assert from "node:assert/strict";
import test from "node:test";

import { isTerminal, pollAgencyRun, stageProgress } from "../src/lib/agencyApi.js";

// agencyPolling.test.mjs — the abort semantics of the research-run poll.
//
// THE BUG THIS COVERS. `pollAgencyRun` awaits a fetch and then calls `onTick`. A request already in
// flight when the caller aborts still resolves, so without a re-check after the await the callback
// fired anyway — writing a stale run into the state of a component that had unmounted, or that had
// moved on to a different run. Selecting a finished run from the history list while another was
// still being polled therefore repainted the one you had just navigated away from.
//
// These run under plain `node --test`: no browser, no React, no Vite. `globalThis.fetch` is
// replaced per test, and the module reads `import.meta.env?.VITE_GATEWAY_URL`, so the base URL is
// simply "" here and the stub sees the bare path.

// stubFetch installs a fetch that answers with `bodies` in order and records how many calls it
// received.
//
// `gate` turns it into a CONTROLLABLE fetch: the request announces that it has started and then
// blocks until the test releases it. That is what lets a test abort while a request is genuinely in
// flight, rather than during the poll's own wait — which is a different code path and the one the
// first version of this file accidentally tested.
//
// `honourAbort` decides what the released request then does. A real fetch rejects with an
// AbortError; a stub that RESOLVES SUCCESSFULLY instead models the harder case — a response that
// was already on the wire when the abort landed — and proves the post-await signal check, not the
// rejection, is what stops `onTick`.
function stubFetch(bodies, { gate = null, honourAbort = true, onCall = null } = {}) {
  let call = 0;
  const previous = globalThis.fetch;
  globalThis.fetch = async (url, opts = {}) => {
    const index = call++;
    onCall?.(index, url, opts);
    if (gate) {
      gate.started(index);
      await gate.released;
    }
    if (honourAbort && opts.signal?.aborted) {
      const err = new Error("aborted");
      err.name = "AbortError";
      throw err;
    }
    const body = bodies[Math.min(index, bodies.length - 1)];
    return { ok: true, status: 200, json: async () => body };
  };
  return {
    calls: () => call,
    restore: () => {
      globalThis.fetch = previous;
    },
  };
}

// makeGate builds the handshake stubFetch uses: `inFlight` resolves when a request has started, and
// `release()` lets that request return.
function makeGate() {
  let announce;
  let release;
  const inFlight = new Promise((resolve) => {
    announce = resolve;
  });
  const released = new Promise((resolve) => {
    release = resolve;
  });
  return { started: announce, inFlight, released, release };
}

const running = { id: "agr_1", status: "running", stage: "stock-scout", pollAfterMs: 5 };
const completed = { id: "agr_1", status: "completed", pollAfterMs: 5 };

test("polling follows a run to its terminal state", async () => {
  const fetchStub = stubFetch([running, completed]);
  try {
    const ticks = [];
    const final = await pollAgencyRun("agr_1", { onTick: (r) => ticks.push(r.status) });
    assert.equal(final.status, "completed");
    assert.deepEqual(ticks, ["running", "completed"]);
  } finally {
    fetchStub.restore();
  }
});

test("an abort during the poll's own wait stops before any request is made", () => {
  // The cheap case: the poll has not issued anything yet, so there is nothing to tear down. Kept
  // because it is a real path, but it is NOT the race — see the two tests below.
  const fetchStub = stubFetch([running]);
  try {
    const controller = new AbortController();
    const promise = pollAgencyRun("agr_1", { signal: controller.signal, onTick: () => {} });
    controller.abort();
    return promise.then((result) => {
      assert.equal(result, null);
      assert.equal(fetchStub.calls(), 0, "an aborted poll must not issue a request");
    });
  } finally {
    fetchStub.restore();
  }
});

test("onTick never fires when the abort lands while a request is in flight", async () => {
  // THE RACE ITSELF, with the ordering pinned rather than timed. The test waits until the stub
  // confirms a request has STARTED, aborts, and only then lets that request finish. An earlier
  // version aborted after 15ms — during the poll's 2000ms wait — so the request had not begun and
  // the post-await check was never exercised.
  const gate = makeGate();
  const fetchStub = stubFetch([running], { gate, honourAbort: true });
  try {
    const controller = new AbortController();
    let ticked = 0;
    const promise = pollAgencyRun("agr_1", {
      signal: controller.signal,
      onTick: () => {
        ticked++;
      },
    });

    await gate.inFlight; // the request is now outstanding
    assert.equal(fetchStub.calls(), 1, "the test must abort while a request is in flight");
    controller.abort();
    gate.release();

    assert.equal(await promise, null, "an aborted poll must resolve to null");
    assert.equal(ticked, 0, "a response that arrived after the abort was written back");
  } finally {
    fetchStub.restore();
  }
});

test("onTick never fires even when the aborted request resolves successfully", async () => {
  // THE HARDER CASE, and the one that proves WHICH mechanism is doing the work. Here the stub
  // ignores the abort entirely and returns a perfectly good response after it. Nothing rejects, so
  // the try/catch cannot help: only the explicit `if (signal?.aborted) return null` after the await
  // stops the stale run being written into a component that has moved on.
  const gate = makeGate();
  const fetchStub = stubFetch([running], { gate, honourAbort: false });
  try {
    const controller = new AbortController();
    let ticked = 0;
    const promise = pollAgencyRun("agr_1", {
      signal: controller.signal,
      onTick: () => {
        ticked++;
      },
    });

    await gate.inFlight;
    controller.abort();
    gate.release(); // resolves 200 OK despite the abort

    assert.equal(await promise, null, "the poll returned a run it had been told to abandon");
    assert.equal(ticked, 0, "the post-await signal check did not prevent onTick");
  } finally {
    fetchStub.restore();
  }
});

test("the abort signal is passed into fetch, not merely checked around it", async () => {
  // Checking the signal around the await leaves the request itself running. Passing it in is what
  // actually tears the connection down.
  let sawSignal = false;
  const fetchStub = stubFetch([completed], {
    onCall: (_i, _url, opts) => {
      sawSignal = opts.signal instanceof AbortSignal;
    },
  });
  try {
    const controller = new AbortController();
    await pollAgencyRun("agr_1", { signal: controller.signal });
    assert.ok(sawSignal, "pollAgencyRun did not forward its signal to fetch");
  } finally {
    fetchStub.restore();
  }
});

test("terminal states are exactly the four that end a run", () => {
  for (const status of ["completed", "failed", "cancelled", "expired"]) {
    assert.equal(isTerminal(status), true, `${status} must be terminal`);
  }
  for (const status of ["queued", "claimed", "running"]) {
    assert.equal(isTerminal(status), false, `${status} must not be terminal`);
  }
});

test("stage progress is read from the reported stage, never guessed", () => {
  assert.deepEqual(stageProgress({ stage: "stock-risk" }), {
    index: 3,
    total: 4,
    label: "stock-risk",
  });
  // A run that has reported no stage shows no progress at all — the UI says "queued" instead of
  // inventing motion.
  assert.deepEqual(stageProgress({}), { index: 0, total: 4, label: null });
  assert.deepEqual(stageProgress({ stage: "not-a-profile" }), { index: 0, total: 4, label: null });
});
