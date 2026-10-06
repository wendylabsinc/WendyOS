import assert from "node:assert/strict";
import test from "node:test";
import { build } from "esbuild";
import { gzipSync } from "node:zlib";
const compiled = await build({
  entryPoints: [
    new URL("../src/simulator-session.ts", import.meta.url).pathname,
  ],
  bundle: true,
  write: false,
  format: "esm",
  platform: "node",
});
const { sceneSession, readScene, closeSceneSession } = await import(
  "data:text/javascript;base64," +
    Buffer.from(compiled.outputFiles[0].text).toString("base64")
);
const session = {
  url: "http://127.0.0.1:12345",
  token: "12345678-1234-1234-1234-123456789abc",
  expires_at: new Date(Date.now() + 60_000).toISOString(),
};

test("compressed host geometry restores exactly and enforces expanded limits", async () => {
  const toolSession = { ...session, resource_uri: `wendy://simulator-scenes/${session.token}` };
  const scene = { version: 1, positions: [0.1234567, -0.7654321], private_scene: "geometry" };
  const bridge = {
    read: async () => ({ _meta: { scene_data_gzip: gzipSync(JSON.stringify(scene)).toString("base64") } }),
    close: async () => {},
  };
  assert.deepEqual(await readScene(toolSession, "/api/scene", new AbortController().signal, bridge), scene);
  bridge.read = async () => ({ _meta: { scene_data_gzip: "not gzip" } });
  await assert.rejects(readScene(toolSession, "/api/scene", new AbortController().signal, bridge));
  bridge.read = async () => ({ _meta: { scene_data_gzip: gzipSync(Buffer.alloc(33 << 20, 32)).toString("base64") } });
  await assert.rejects(readScene(toolSession, "/api/scene", new AbortController().signal, bridge), /32 MiB response limit/);
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(readScene(toolSession, "/api/scene", controller.signal, bridge), (error) => error.name === "AbortError");
});

test("sessions accept only private loopback endpoints with an expiry", () => {
  assert.deepEqual(sceneSession(session), session);
  for (const url of [
    "https://127.0.0.1:12345",
    "http://localhost:12345",
    "http://127.0.0.2:12345",
    "http://user@127.0.0.1:12345",
    "http://127.0.0.1:12345/other",
    "http://127.0.0.1:12345/?token=secret",
  ]) {
    assert.throws(() => sceneSession({ ...session, url }));
  }
  assert.throws(() => sceneSession({ ...session, expires_at: "never" }));
  assert.equal(sceneSession(undefined), undefined);
});

test("scene reads omit cookies and URL credentials, reject redirects and pass cancellation", async () => {
  const original = globalThis.fetch;
  let requests = 0;
  const controller = new AbortController();
  globalThis.fetch = async (url, options) => {
    requests++;
    assert.equal(url, session.url + "/api/scene/state");
    assert.equal(options.credentials, "omit");
    assert.equal(options.redirect, "error");
    assert.equal(options.headers.Authorization, "Bearer " + session.token);
    controller.abort();
    assert.equal(options.signal.aborted, true);
    return new Response('{"epoch":5}');
  };
  try {
    assert.deepEqual(
      await readScene(session, "/api/scene/state", controller.signal),
      { epoch: 5 },
    );
    await assert.rejects(
      readScene(session, "/api/command", new AbortController().signal),
      /Unsupported/,
    );
    await assert.rejects(
      readScene(
        { ...session, expires_at: new Date(0).toISOString() },
        "/api/scene",
        controller.signal,
      ),
      /expired/,
    );
    assert.equal(
      requests,
      1,
      "Rejected endpoints and expiry must not send requests",
    );
  } finally {
    globalThis.fetch = original;
  }
});

test("ended sessions give a reconnect instruction and cleanup only deletes the session", async () => {
  const original = globalThis.fetch;
  try {
    globalThis.fetch = async () => new Response("", { status: 403 });
    await assert.rejects(
      readScene(session, "/api/scene", new AbortController().signal),
      /session ended.*Reconnect/,
    );
    let closed = false;
    globalThis.fetch = async (url, options) => {
      assert.equal(url, session.url + "/session");
      assert.equal(options.method, "DELETE");
      assert.equal(options.credentials, "omit");
      closed = true;
      throw Error("The already-expired server has closed");
    };
    closeSceneSession(session);
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.equal(closed, true);
  } finally {
    globalThis.fetch = original;
  }
});

test("host tool reads and cleanup work without any browser network access", async () => {
  const original = globalThis.fetch;
  const proxied = {
    ...session,
    resource_uri: `wendy://simulator-scenes/${session.token}`,
  };
  let reads = 0;
  let closed = false;
  globalThis.fetch = () => {
    throw Error("Browser network access is blocked");
  };
  const bridge = {
    read: async (id, part, signal) => {
      reads++;
      assert.equal(signal.aborted, false);
      assert.equal(id, session.token);
      assert.equal(part, reads === 1 ? "geometry" : "state");
      return { _meta: { scene_data: { version: 1 } } };
    },
    close: async (id) => {
      assert.equal(id, session.token);
      closed = true;
    },
  };
  try {
    assert.deepEqual(sceneSession(proxied), proxied);
    assert.throws(() =>
      sceneSession({ ...proxied, resource_uri: "wendy://devices/private" }),
    );
    for (const path of ["/api/scene", "/api/scene/state"])
      assert.deepEqual(
        await readScene(proxied, path, new AbortController().signal, bridge),
        { version: 1 },
      );
    await assert.rejects(
      readScene(proxied, "/api/command", new AbortController().signal, bridge),
      /Unsupported/,
    );
    assert.equal(reads, 2);
    await assert.rejects(
      readScene(proxied, "/api/scene", new AbortController().signal),
      /host cannot read/,
    );
    closeSceneSession(proxied, bridge);
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.equal(closed, true);
  } finally {
    globalThis.fetch = original;
  }
});

test("host scene reads reject missing UI metadata and preserve cancellation", async () => {
  const proxied = {
    ...session,
    resource_uri: `wendy://simulator-scenes/${session.token}`,
  };
  const controller = new AbortController();
  await assert.rejects(
    readScene(proxied, "/api/scene", controller.signal, {
      read: async () => ({ structuredContent: { version: 1 } }),
      close: async () => {},
    }),
    /no scene data/,
  );
  await assert.rejects(
    readScene(proxied, "/api/scene", controller.signal, {
      read: async (_id, _part, signal) => {
        controller.abort(new Error("View hidden"));
        assert.equal(signal.aborted, true);
        return { contents: [] };
      },
      close: async () => {},
    }),
    /View hidden/,
  );
});
