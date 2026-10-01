import assert from "node:assert/strict";
import test from "node:test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";
const result = await build({
  entryPoints: [
    fileURLToPath(new URL("../src/device-visual.ts", import.meta.url)),
  ],
  bundle: true,
  write: false,
  format: "esm",
  platform: "node",
});
const { displayModel } = await import(
  "data:text/javascript;base64," +
    Buffer.from(result.outputFiles[0].text).toString("base64")
);
test("known fleet names select illustrations without overriding explicit models", () => {
  assert.equal(displayModel("generic", "Unitree G1 Nx"), "g1");
  assert.equal(displayModel("generic", "Spark 48fd (Spark 3)"), "dgx");
  assert.equal(displayModel("generic", "DGX Spark"), "dgx");
  assert.equal(displayModel("go2", "Unitree G1 Nx"), "go2");
  assert.equal(displayModel("generic", "sparkle"), "generic");
  assert.equal(displayModel("generic", "joannis-agx-orin"), "generic");
});
