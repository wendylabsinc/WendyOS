import assert from "node:assert/strict";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { build } from "esbuild";
import { renderToStaticMarkup } from "react-dom/server";

const compiled = await build({
  entryPoints: [
    fileURLToPath(new URL("../src/inspection-status.tsx", import.meta.url)),
  ],
  bundle: true,
  write: false,
  format: "esm",
  platform: "node",
});
const { InspectionStatus } = await import(
  "data:text/javascript;base64," +
    Buffer.from(compiled.outputFiles[0].text).toString("base64")
);
const render = (props) =>
  renderToStaticMarkup(
    InspectionStatus({
      ready: true,
      error: "",
      onRetry() {},
      ...props,
    }),
  );

test("pending inspection does not claim failure, connection, or an empty app inventory", () => {
  const html = render({ phase: "loading" });
  assert.match(html, /Checking agent/);
  assert.match(html, /use other tabs or select another device/);
  assert.match(html, /aria-busy="true"/);
  assert.doesNotMatch(
    html,
    /Agent responded|Connection not verified|Awaiting inspection|<dd>0<\/dd>|badge online/,
  );
});

test("inspection failure exposes its error and an enabled retry without reporting online", () => {
  const html = render({
    phase: "error",
    error: "Device connection timed out.",
  });
  assert.match(html, /Inspection failed/);
  assert.match(html, /role="alert">Device connection timed out\./);
  assert.match(html, /Retry inspection/);
  assert.doesNotMatch(html, /disabled|badge online|Agent responded/);
});

test("only a completed agent response confirms connection, even when older data exists", () => {
  const inspection = { connected: true, agent_version: "test", apps: [] };
  for (const phase of ["waiting", "loading", "error"]) {
    const html = render({ phase, inspection });
    assert.doesNotMatch(html, /badge online|Agent responded/);
  }
  assert.match(render({ phase: "complete", inspection }), /Agent responded/);
  assert.match(render({ phase: "complete", inspection }), /<dd>0<\/dd>/);
  assert.doesNotMatch(
    render({ phase: "complete", inspection: { connected: false } }),
    /badge online|Agent responded/,
  );
});
