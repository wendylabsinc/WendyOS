import assert from "node:assert/strict";
import test from "node:test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";
const result = await build({
  entryPoints: [
    fileURLToPath(new URL("../src/request-queue.ts", import.meta.url)),
  ],
  bundle: true,
  write: false,
  format: "esm",
  platform: "node",
});
const { RequestQueue } = await import(
  "data:text/javascript;base64," +
    Buffer.from(result.outputFiles[0].text).toString("base64")
);
const deferred = () => {
  let resolve;
  const promise = new Promise((r) => (resolve = r));
  return { promise, resolve };
};
test("an idle background poll yields to a user action in the same turn", async () => {
  const queue = new RequestQueue(), order = [];
  const background = queue.enqueue(async () => order.push("background"), "background");
  const foreground = queue.enqueue(async () => order.push("foreground"));
  await Promise.all([background, foreground]);
  assert.deepEqual(order, ["foreground", "background"]);
});
test("foreground requests jump queued discovery without interrupting an active call", async () => {
  const queue = new RequestQueue(),
    gate = deferred(),
    order = [];
  const active = queue.enqueue(async () => {
    order.push("active");
    await gate.promise;
  });
  const background = queue.enqueue(async () => {
    order.push("background");
  }, "background");
  const foreground = queue.enqueue(async () => {
    order.push("foreground");
  });
  assert.deepEqual(order, ["active"]);
  gate.resolve();
  await Promise.all([active, background, foreground]);
  assert.deepEqual(order, ["active", "foreground", "background"]);
});
test("canceled queued work never reaches the transport", async () => {
  const queue = new RequestQueue(),
    gate = deferred(),
    abort = new AbortController();
  let calls = 0;
  const active = queue.enqueue(() => gate.promise);
  const canceled = queue.enqueue(
    async () => {
      calls++;
    },
    "background",
    abort.signal,
  );
  const rejected = assert.rejects(
    canceled,
    (error) => error.name === "AbortError",
  );
  abort.abort();
  await rejected;
  gate.resolve();
  await active;
  await queue.enqueue(async () => {
    calls++;
  });
  assert.equal(calls, 1);
});
test("failed requests release the queue and are not retried", async () => {
  const queue = new RequestQueue();
  let calls = 0;
  const failed = queue.enqueue(async () => {
    calls++;
    throw Error("failed");
  });
  const next = queue.enqueue(async () => 42);
  await assert.rejects(failed, /failed/);
  assert.equal(await next, 42);
  assert.equal(calls, 1);
});
