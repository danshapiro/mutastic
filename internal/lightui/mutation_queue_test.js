"use strict";

const assert = require("node:assert/strict");
const createLightMutationQueue = require("./mutation_queue.js");

function mutation(value) {
  return {
    key: "COM4:brightness",
    endpoint: "/api/light",
    body: {target: "COM4", action: "brightness", value},
    replace: true,
  };
}

async function waitFor(predicate) {
  for (let attempt = 0; attempt < 100; attempt += 1) {
    if (predicate()) return;
    await new Promise((resolve) => setImmediate(resolve));
  }
  throw new Error("timed out waiting for mutation");
}

async function returnToInFlightValueReplacesPendingValue() {
  const calls = [];
  let releaseFirst;
  const firstResponse = new Promise((resolve) => {
    releaseFirst = resolve;
  });
  const queue = createLightMutationQueue({
    post: (item) => {
      calls.push(item.body.value);
      return calls.length === 1 ? firstResponse : Promise.resolve();
    },
  });

  queue.enqueue(mutation(50));
  await waitFor(() => calls.length === 1);
  queue.enqueue(mutation(52));
  queue.enqueue(mutation(50));
  releaseFirst();
  await queue.whenIdle();

  assert.deepEqual(calls, [50], "a stale intermediate slider value must not be sent");
}

async function failedValueCanBeRetried() {
  const calls = [];
  const queue = createLightMutationQueue({
    post: (item) => {
      calls.push(item.body.value);
      return calls.length === 1
        ? Promise.reject(new Error("simulated failure"))
        : Promise.resolve();
    },
  });

  queue.enqueue(mutation(50));
  await queue.whenIdle();
  assert.deepEqual(calls, [50]);

  queue.enqueue(mutation(50));
  await queue.whenIdle();
  assert.deepEqual(calls, [50, 50], "the same value must be sendable again after failure");
}

async function sameValueCanBeSentAgainAfterSuccess() {
  const calls = [];
  const queue = createLightMutationQueue({
    post: (item) => {
      calls.push(item.body.value);
      return Promise.resolve();
    },
  });

  queue.enqueue(mutation(50));
  await queue.whenIdle();
  assert.equal(queue.enqueue(mutation(50)), true);
  await queue.whenIdle();
  assert.deepEqual(calls, [50, 50], "a later request must not be blocked by an earlier success");
}

Promise.all([
  returnToInFlightValueReplacesPendingValue(),
  failedValueCanBeRetried(),
  sameValueCanBeSentAgainAfterSuccess(),
]).catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
