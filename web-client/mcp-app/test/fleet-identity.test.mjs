import assert from "node:assert/strict";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { build } from "esbuild";

const compiled = await build({
  entryPoints: [
    fileURLToPath(new URL("../src/fleet-identity.ts", import.meta.url)),
  ],
  bundle: true,
  write: false,
  format: "esm",
  platform: "node",
});
const { displayIdentity, withDisplayIdentity } = await import(
  "data:text/javascript;base64," +
    Buffer.from(compiled.outputFiles[0].text).toString("base64")
);

test("fleet refresh preserves display identity but always uses fresh access and presence", () => {
  const cached = {
    model: "go2",
    device_type: "Unitree Go2",
    cloud_presence: "online",
    can_capture: true,
    can_control_apps: true,
    can_read_events: true,
    connected: true,
    readiness: "ready",
  };
  const fresh = {
    id: "robot",
    model: "generic",
    cloud_presence: "offline",
    can_capture: false,
    can_control_apps: false,
    can_read_events: false,
  };
  assert.deepEqual(withDisplayIdentity(fresh, cached), {
    ...fresh,
    model: "go2",
    device_type: "Unitree Go2",
  });
  assert.deepEqual(displayIdentity(cached), {
    model: "go2",
    device_type: "Unitree Go2",
  });
});

test("fresh identity wins and a changed device type invalidates the cached model", () => {
  const cached = { model: "go2", device_type: "Unitree Go2" };
  assert.deepEqual(
    withDisplayIdentity(
      { model: "thor", device_type: "Jetson AGX Thor" },
      cached,
    ),
    {
      model: "thor",
      device_type: "Jetson AGX Thor",
    },
  );
  assert.deepEqual(
    withDisplayIdentity(
      { model: "generic", device_type: "Different device" },
      cached,
    ),
    {
      model: "generic",
      device_type: "Different device",
    },
  );
});

test("missing or malformed inspection metadata cannot populate the identity cache", () => {
  for (const value of [
    null,
    undefined,
    {},
    { model: 42, device_type: false },
    { connected: true },
  ]) {
    assert.equal(displayIdentity(value), undefined);
  }
});
