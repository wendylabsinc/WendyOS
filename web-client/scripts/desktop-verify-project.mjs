// Explicit integration check for the same project actions exposed by Electron.
import assert from "node:assert/strict";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { setTimeout as delay } from "node:timers/promises";
import pty from "node-pty";
import { DesktopService } from "../electron/service.mjs";
import { Sessions } from "../electron/sessions.mjs";
import { runtimeCommand } from "../electron/policy.mjs";

const runtime = process.argv[2] || "docker";
const file = runtimeCommand(runtime);
const site = fileURLToPath(new URL("..", import.meta.url));
const project = await mkdtemp(join(tmpdir(), "wendy-desktop-project-"));
const appId = `dev.wendy.desktop-check-${Date.now()}`;
const sessions = new Sessions({
  spawn: pty.spawn,
  env: process.env,
  cwd: project,
  emit() {},
});
const service = new DesktopService({
  resources: join(site, "desktop-resources"),
  data: project,
  env: process.env,
  sessions,
  chooseDirectory: async () => project,
});
try {
  await writeFile(
    join(project, "Dockerfile"),
    'FROM busybox:1.37\nCMD ["echo", "WENDY_DESKTOP_APP_OK"]\n',
  );
  await writeFile(
    join(project, "wendy.json"),
    JSON.stringify({ appId, version: "0.1.0", platform: "linux" }),
  );
  const selected = await service.pickProject();
  for (const action of ["build", "run"]) {
    console.log(`${action} on ${runtime}...`);
    const job = service.projectAction({
      action,
      project: selected,
      target: runtime,
      runtime,
    });
    const deadline = Date.now() + 180_000;
    while (sessions.get(job.id).running) {
      if (Date.now() > deadline)
        throw new Error(sessions.snapshot(job.id).output.slice(-8000));
      await delay(250);
    }
    const result = sessions.snapshot(job.id);
    assert.equal(result.exitCode, 0, result.output.slice(-8000));
    if (action === "run")
      assert.ok(result.output.includes("WENDY_DESKTOP_APP_OK"), result.output);
    console.log(`${action}: passed`);
  }
} finally {
  sessions.close();
  try {
    await service.run(file, [runtime === "docker" ? "rm" : "delete", appId]);
  } catch {
    /* May not exist if the build failed. */
  }
  await rm(project, { recursive: true, force: true });
}
