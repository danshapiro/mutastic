(function (root, factory) {
  if (typeof module === "object" && module.exports) {
    module.exports = factory;
  } else {
    root.LightMutationQueue = factory;
  }
})(typeof globalThis === "object" ? globalThis : this, function createLightMutationQueue(options) {
  options = options || {};
  if (typeof options.post !== "function") {
    throw new TypeError("mutation queue requires a post function");
  }

  const onSuccess = options.onSuccess || function () {};
  const onFailure = options.onFailure || function () {};
  const onSettled = options.onSettled || function () {};
  const pending = [];
  const idleWaiters = [];
  let running = null;

  function fingerprintFor(mutation) {
    return mutation.fingerprint || `${mutation.endpoint}:${JSON.stringify(mutation.body)}`;
  }

  function removePendingReplacement(key) {
    for (let index = pending.length - 1; index >= 0; index -= 1) {
      if (pending[index].replace && pending[index].key === key) {
        pending.splice(index, 1);
      }
    }
  }

  function resolveIdle() {
    if (running !== null || pending.length !== 0) return;
    while (idleWaiters.length > 0) {
      idleWaiters.shift()();
    }
  }

  function pump() {
    if (running !== null || pending.length === 0) {
      resolveIdle();
      return;
    }

    const mutation = pending.shift();
    running = mutation;
    Promise.resolve()
      .then(() => options.post(mutation))
      .then(
        (result) => {
          try {
            onSuccess(result, mutation);
          } catch (error) {
            onFailure(error, mutation);
          }
        },
        (error) => {
          onFailure(error, mutation);
        },
      )
      .finally(() => {
        running = null;
        onSettled(mutation);
        pump();
      });
  }

  function enqueue(mutation) {
    if (!mutation || typeof mutation.key !== "string" || mutation.key === "") {
      throw new TypeError("mutation requires a key");
    }

    const item = Object.assign({}, mutation, {
      fingerprint: fingerprintFor(mutation),
    });

    if (item.replace) {
      // If the user returns to the exact value currently in flight, discard
      // the stale pending replacement. The in-flight request already
      // represents the latest intent; unlike a confirmed-value cache, this
      // does not suppress a later request after the request has completed.
      if (running !== null && running.replace && running.key === item.key && running.fingerprint === item.fingerprint) {
        removePendingReplacement(item.key);
        return false;
      }
      for (let index = 0; index < pending.length; index += 1) {
        if (pending[index].replace && pending[index].key === item.key) {
          pending[index] = item;
          pump();
          return true;
        }
      }
    }

    pending.push(item);
    pump();
    return true;
  }

  function whenIdle() {
    if (running === null && pending.length === 0) {
      return Promise.resolve();
    }
    return new Promise((resolve) => idleWaiters.push(resolve));
  }

  return {
    enqueue,
    whenIdle,
    isRunning: () => running !== null,
    pendingCount: () => pending.length,
  };
});
