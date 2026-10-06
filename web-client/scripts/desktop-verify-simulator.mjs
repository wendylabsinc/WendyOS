// Explicit integration check. Builds a disposable Go2, verifies causal movement,
// then removes only the container created by this invocation.
import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { setTimeout as delay } from "node:timers/promises";
import pty from "node-pty";
import { DesktopService } from "../electron/service.mjs";
import { Sessions } from "../electron/sessions.mjs";
import { runtimeCommand } from "../electron/policy.mjs";

const runtime = process.argv[2] || "docker";
const kind = process.argv[3] || "go2";
if (!["go2", "g1"].includes(kind)) throw new Error("Choose go2 or g1.");
const file = runtimeCommand(runtime);
const site = fileURLToPath(new URL("..", import.meta.url));
const data = await mkdtemp(join(tmpdir(), "wendy-desktop-check-"));
const sessions = new Sessions({
  spawn: pty.spawn,
  env: process.env,
  cwd: site,
  emit: (event) => {
    if (event.type === "exit")
      console.log(`${event.title}: exit ${event.exitCode}`);
  },
});
const service = new DesktopService({
  resources: join(site, "desktop-resources"),
  data,
  env: process.env,
  sessions,
});
let simulator;
try {
  await service.initialize();
  console.log(`Building and starting disposable ${kind} on ${runtime}...`);
  const created = await service.createSimulator({
    name: `desktop-check-${Date.now()}`,
    runtime,
    kind,
  });
  simulator = created.simulator;
  const deadline = Date.now() + 30 * 60_000;
  let report = Date.now();
  while (sessions.get(created.session.id).running) {
    if (Date.now() > deadline)
      throw new Error("Simulator build exceeded 30 minutes.");
    if (Date.now() - report > 30_000) {
      console.log(
        sessions
          .snapshot(created.session.id)
          .output.replace(/\x1b\[[0-9;?]*[a-zA-Z]/g, "")
          .slice(-1200),
      );
      report = Date.now();
    }
    await delay(500);
  }
  const completed = sessions.snapshot(created.session.id);
  assert.equal(completed.exitCode, 0, completed.output.slice(-8000));
  const state = await service.simStatus(simulator.id);
  assert.equal(state.ready, true, state.error);
  async function request(path, body) {
    try {
      const response = await fetch(new URL(path, simulator.url), {
        signal: AbortSignal.timeout(5000),
        ...(body
          ? {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify(body),
            }
          : {}),
      });
      if (!response.ok)
        throw new Error(`${path}: ${response.status} ${await response.text()}`);
      return await response.json();
    } catch (error) {
      throw new Error(`${path}: ${error.message}`, { cause: error });
    }
  }
  const before = await request("api/status");
  const { token } = await request("api/arm", {});
  try {
    for (let i = 0; i < 15; i++) {
      await request("api/command", { token, velocity: [0.2, 0, 0] });
      await delay(150);
    }
  } finally {
    await request("api/stop", { token });
    await request("api/release", { token });
  }
  const after = await request("api/status");
  const distance = Math.hypot(
    after.position[0] - before.position[0],
    after.position[1] - before.position[1],
  );
  assert.ok(distance > 0.03, `${kind} did not move: ${distance} m`);
  console.log(
    `${kind} moved ${distance.toFixed(3)} m. Checking the viewer scene...`,
  );
  const scene = await request("api/scene");
  assert.ok(scene.id, "The browser scene is missing its identity.");
  const viewer = await fetch(simulator.url);
  assert.equal(viewer.status, 200);
  console.log(
    JSON.stringify({
      runtime,
      healthy: after.healthy,
      ready: after.ready,
      movementMeters: distance,
      scene: scene.id,
      viewer: viewer.status,
    }),
  );
} catch (error) {
  if (simulator) {
    try {
      console.error(
        (await service.run(file, ["logs", simulator.container])).slice(-4000),
      );
    } catch {
      /* Container may not have been created. */
    }
  }
  throw error;
} finally {
  sessions.close();
  if (simulator) {
    try {
      await service.inspectSim(simulator);
      await service.run(file, ["stop", simulator.container]);
      await service.run(file, [
        runtime === "docker" ? "rm" : "delete",
        simulator.container,
      ]);
    } catch (error) {
      console.error(`Cleanup: ${error.message}`);
    }
  }
  await rm(data, { recursive: true, force: true });
}
