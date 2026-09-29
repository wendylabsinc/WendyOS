// Creates its own WendyOS guest, deploys an app, checks monitoring, and removes it.
import assert from "node:assert/strict";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { setTimeout as delay } from "node:timers/promises";
import pty from "node-pty";
import { DesktopService } from "../electron/service.mjs";
import { Sessions } from "../electron/sessions.mjs";

const site = fileURLToPath(new URL("..", import.meta.url));
const data = await mkdtemp(join(tmpdir(), "wendy-desktop-vm-"));
const sessions = new Sessions({
  spawn: pty.spawn,
  env: process.env,
  cwd: data,
  emit() {},
});
const service = new DesktopService({
  resources: join(site, "desktop-resources"),
  data,
  env: process.env,
  sessions,
  chooseDirectory: async () => data,
});
let simulator;
async function completed(job) {
  const deadline = Date.now() + 15 * 60_000;
  let reported = Date.now();
  while (sessions.get(job.id).running) {
    const output = sessions.snapshot(job.id).output;
    if (Date.now() > deadline) throw new Error(output.slice(-6000));
    if (Date.now() - reported > 30_000) {
      console.log(output.slice(-1200));
      reported = Date.now();
    }
    await delay(500);
  }
  const result = sessions.snapshot(job.id);
  assert.equal(result.exitCode, 0, result.output.slice(-6000));
}
async function outputContains(job, pattern) {
  const deadline = Date.now() + 180_000;
  while (Date.now() < deadline) {
    const result = sessions.snapshot(job.id);
    if (pattern.test(result.output)) return result.output;
    if (!result.running) throw new Error(result.output.slice(-6000));
    await delay(500);
  }
  throw new Error(sessions.snapshot(job.id).output.slice(-6000));
}
try {
  await service.initialize();
  console.log("Creating a disposable Raspberry Pi app VM...");
  const result = await service.createSimulator({
    name: "pi-check",
    kind: "raspberry-pi",
    runtime: "docker",
  });
  simulator = result.simulator;
  await completed(result.session);
  assert.equal((await service.simStatus(simulator.id)).ready, true);
  console.log("Guest agent is ready. Deploying an ARM64 app...");
  const appId = "dev.wendy.desktop-vm-check";
  await writeFile(
    join(data, "Dockerfile"),
    'FROM busybox:1.37\nCMD ["sh", "-c", "while true; do echo WENDY_VM_APP_OK; sleep 1; done"]\n',
  );
  await writeFile(
    join(data, "wendy.json"),
    JSON.stringify({ appId, version: "0.1.0", platform: "linux" }),
  );
  const project = await service.pickProject();
  const run = service.projectAction({
    action: "run",
    project,
    runtime: "docker",
    target: simulator.target,
  });
  await outputContains(run, /WENDY_VM_APP_OK/);
  console.log("App output verified. Checking Dashboard data and logs...");
  const snapshot = await service.deviceSnapshot({ target: simulator.target });
  assert.equal(snapshot.target, simulator.target);
  assert.ok(Number.isFinite(snapshot.host.cpuPercent));
  assert.ok(snapshot.host.memTotalBytes > 0);
  assert.ok(snapshot.containers.some((container) => container.name.includes(appId)));
  const checkLogs = async (args, expected) => {
    const stream = await service.deviceLogsStart(args);
    try {
      for (let attempt = 0; attempt < 40; attempt++) {
        const current = service.deviceLogsSnapshot(stream.id);
        if (current.entries.some((entry) => expected.test(entry.body))) return;
        if (current.error) throw new Error(current.error);
        await delay(500);
      }
      throw new Error(`Expected log message was not received: ${expected}`);
    } finally { service.deviceLogsStop(stream.id); }
  };
  await checkLogs({ target: simulator.target, app: appId }, /WENDY_VM_APP_OK/);
  await checkLogs({ target: simulator.name, source: { kind: "simulator", id: simulator.id } }, /Linux|systemd|Wendy/i);
  sessions.cancel(run.id);
  console.log("PASS: VM readiness, app deployment, Dashboard data, app logs, and boot logs.");
} finally {
  service.close();
  sessions.close();
  if (simulator) {
    const exists = (await service.virtualMachines()).some(
      (vm) => vm.name === simulator.vmName,
    );
    if (exists)
      await service.run(
        service.cli,
        ["vm", "rm", simulator.vmName, "--force", "--yes"],
        { timeout: 60_000 },
      );
  }
  await rm(data, { recursive: true, force: true });
}
