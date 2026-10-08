import assert from "node:assert/strict";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { build } from "esbuild";
import { AppBridge } from "@modelcontextprotocol/ext-apps/app-bridge";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { ErrorCode } from "@modelcontextprotocol/sdk/types.js";

// Compile the production bridge without writing generated files or replacing
// its SDK calls. Absolute external imports also work from the in-memory module.
const compiled = await build({
  entryPoints: [fileURLToPath(new URL("../src/bridge.ts", import.meta.url))],
  bundle: true,
  write: false,
  format: "esm",
  platform: "node",
  plugins: [
    {
      name: "installed-sdk-imports",
      setup(builder) {
        builder.onResolve({ filter: /^@/ }, ({ path }) => ({
          path: import.meta.resolve(path),
          external: true,
        }));
      },
    },
  ],
});
const { app, call, APP_WEB_REQUEST_OPTIONS } = await import(
  "data:text/javascript;base64," +
    Buffer.from(compiled.outputFiles[0].text).toString("base64")
);

test(
  "app web preparation times out despite continuous host progress",
  { timeout: 3000 },
  async (t) => {
    assert.ok(
      APP_WEB_REQUEST_OPTIONS,
      "production app web request options must be exported",
    );
    // Only DOM sizing is disabled. Initialization, calls, progress, timeouts, and
    // cancellation all use the installed MCP Apps and MCP SDK implementations.
    app.options.autoResize = false;
    const host = new AppBridge(
      null,
      { name: "test-host", version: "1" },
      { serverTools: {} },
    );
    const [appTransport, hostTransport] = InMemoryTransport.createLinkedPair();
    let heartbeat;
    let progressCount = 0;
    let requestCount = 0;
    let hostCanceled = false;
    let completeHandler;
    const handlerFinished = new Promise((resolve) => {
      completeHandler = resolve;
    });
    const backgroundErrors = [];
    t.after(async () => {
      clearInterval(heartbeat);
      await app.close();
      await host.close();
    });
    host.oncalltool = async (params, extra) => {
      requestCount++;
      assert.equal(params.name, "open_robot_app");
      assert.deepEqual(params.arguments, {
        robot_id: "test-device",
        app_name: "test-app",
      });
      assert.notEqual(
        params._meta?.progressToken,
        undefined,
        "exercise actual SDK progress handling",
      );
      const emitProgress = async () => {
        progressCount++;
        await extra.sendNotification({
          method: "notifications/progress",
          params: {
            progressToken: params._meta.progressToken,
            progress: progressCount,
          },
        });
      };
      extra.signal.addEventListener(
        "abort",
        () => {
          hostCanceled = true;
          clearInterval(heartbeat);
          completeHandler();
        },
        { once: true },
      );
      // Establish that progress reaches the app before its deadline, then keep
      // it arriving throughout the wait. No successful tool response is sent.
      await emitProgress();
      await emitProgress();
      await emitProgress();
      heartbeat = setInterval(() => {
        void emitProgress().catch((error) => backgroundErrors.push(error));
      }, 10);
      await handlerFinished;
      return { content: [] };
    };
    await host.connect(hostTransport);
    await app.connect(appTransport);

    await assert.rejects(
      call(
        "open_robot_app",
        { robot_id: "test-device", app_name: "test-app" },
        {
          ...APP_WEB_REQUEST_OPTIONS,
          timeout: 80,
          // Keep the total safeguard longer so removing resetTimeoutOnProgress:
          // false fails this test instead of passing through that separate guard.
          maxTotalTimeout: 1000,
        },
      ),
      (error) => {
        assert.equal(error.code, ErrorCode.RequestTimeout);
        assert.match(error.message, /Request timed out/);
        assert.equal(error.data?.timeout, 80);
        return true;
      },
    );
    assert.ok(
      progressCount > 3,
      "host must continue reporting progress during preparation",
    );
    assert.equal(
      hostCanceled,
      true,
      "the fixed deadline must cancel the outstanding host request",
    );
    assert.equal(
      requestCount,
      1,
      "a timeout must not automatically allocate another app view",
    );
    assert.deepEqual(backgroundErrors, []);
  },
);
