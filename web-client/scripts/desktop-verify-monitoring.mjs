// Run with Electron after desktop:build. This opens an isolated hidden renderer
// with deterministic IPC fixtures. It does not connect to devices or simulators.
import assert from "node:assert/strict";
import { app, BrowserWindow, ipcMain } from "electron";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { setTimeout as delay } from "node:timers/promises";

async function main() {
const root = fileURLToPath(new URL("..", import.meta.url));
const data = await mkdtemp(join(tmpdir(), "wendy-monitoring-fixture-"));
app.setPath("userData", data);
app.setName("Wendy monitoring fixture");
const calls = { snapshots: [], logs: [], stops: [], sessions: 0 };
const streams = new Map();
const vms = [
  { name: "fixture-a", state: "running", address: "127.0.0.1:50051" },
  { name: "fixture-b", state: "running", address: "127.0.0.1:50052" },
  { name: "fixture-unavailable", state: "running", address: "127.0.0.1:50053" },
];
const handlers = {
  status: () => ({ version: "Monitoring fixture", runtimes: [{ id: "docker", ready: true, detail: "fixture" }], platform: "darwin", image: "fixture" }),
  "sim:list": () => [],
  "vm:list": () => vms,
  "session:list": () => [],
  "device:snapshot": ({ target }) => {
    calls.snapshots.push(target);
    if (target === "vm:fixture-unavailable") throw new Error("✗ simulator unavailable: TLS handshake rejected by device. Check device clock: ssh root@vm date. WENDY_TLS_DEBUG=1 wendy device top");
    return { target, sampledAt: new Date().toISOString(), host: { cpuPercent: target.endsWith("a") ? 12.5 : 41.7, cpuCount: 4, memUsedBytes: 1024 ** 3, memTotalBytes: 4 * 1024 ** 3, containerStorage: { usedBytes: 5 * 1024 ** 3, totalBytes: 16 * 1024 ** 3, mountpoint: "/data" }, maximumTemperature: { name: "CPU", tempC: 48.2 } }, containers: [{ name: "camera", state: "RUNNING", cpuPercent: 2.3, memBytes: 256 * 1024 ** 2 }] };
  },
  "device:logs:start": (args) => {
    calls.logs.push(args);
    if (args.target === "vm:fixture-unavailable") throw new Error("TLS handshake rejected by device. Run WENDY_TLS_DEBUG=1 wendy device logs");
    const id = String(calls.logs.length);
    streams.set(id, { id, ...args, app: args.app || "", level: args.level || "all", running: true, dropped: 0, entries: [
      { id: 1, timestamp: new Date().toISOString(), severity: "info", service: "camera", body: "Camera is ready", attributes: { "frame.rate": "30" } },
      { id: 2, timestamp: new Date().toISOString(), severity: "warn", service: "camera", body: "Frame dropped", attributes: {} },
    ] });
    return { id };
  },
  "device:logs:snapshot": (id) => streams.get(id),
  "device:logs:stop": (id) => { calls.stops.push(id); streams.delete(id); },
  containers: () => [{ id: "fixture-container", name: "camera", image: "camera:dev", state: "running", ports: "" }],
  "project:action": () => { calls.sessions++; throw new Error("No sessions should run during monitoring."); },
};
for (const [method, handler] of Object.entries(handlers)) ipcMain.handle(`wendy:${method}`, (_event, args) => handler(args));

let window;
try {
  await app.whenReady();
  window = new BrowserWindow({ show: false, width: 1480, height: 960, webPreferences: { preload: join(root, "electron/preload.cjs"), contextIsolation: true, sandbox: true, nodeIntegration: false, backgroundThrottling: false } });
  const errors = [];
  window.webContents.on("console-message", (event) => { if (event.level === "error") errors.push(event.message); });
  const evaluate = (script) => window.webContents.executeJavaScript(script);
  const wait = async (expression) => {
    for (let attempt = 0; attempt < 100; attempt++) {
      if (await evaluate(expression)) return;
      await delay(50);
    }
    throw new Error(`Renderer condition failed: ${expression}`);
  };
  const clickText = async (text) => {
    const source = `Array.from(document.querySelectorAll('button')).find(button => button.textContent.trim() === ${JSON.stringify(text)})`;
    await wait(`!!(${source})`);
    assert.equal(await evaluate(`(() => { const button = ${source}; if (!button || button.disabled) return false; button.click(); return true; })()`), true, `Clickable ${text}`);
  };
  const clickSelector = (selector) => evaluate(`document.querySelector(${JSON.stringify(selector)}).click()`);
  const fill = (selector, value) => evaluate(`(() => { const input = document.querySelector(${JSON.stringify(selector)}); Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, ${JSON.stringify(value)}); input.dispatchEvent(new Event('input', { bubbles: true })); })()`);
  await window.loadFile(join(root, "dist-desktop/index.html"));
  await clickText("Devices & monitoring");
  await wait(`!!document.querySelector('[aria-label="Dashboard for fixture-a"]')`);
  assert.equal(await evaluate(`Array.from(document.querySelectorAll('button')).some(button => button.textContent.trim() === 'Select')`), false);
  await clickSelector('[aria-label="Dashboard for fixture-a"]');
  await wait(`document.body.textContent.includes('12.5%')`);
  assert.equal(await evaluate(`document.querySelector('#target').value`), "docker", "Monitoring preserves the build target");
  assert.equal(await evaluate(`document.querySelectorAll('.desktop-sessions').length`), 0);
  assert.equal(await evaluate(`document.querySelectorAll('.desktop-dashboard').length`), 1);
  await clickText("Dashboard");
  assert.equal(await evaluate(`document.querySelectorAll('.desktop-dashboard').length`), 1);
  await clickText("← Back to devices");
  await clickSelector('[aria-label="Dashboard for fixture-b"]');
  await wait(`document.body.textContent.includes('41.7%')`);
  await clickText("Workspace");
  await clickText("Devices & monitoring");
  await wait(`!!document.querySelector('[aria-label="Dashboard for vm:fixture-b"]') && document.body.textContent.includes('41.7%')`);
  await delay(200);
  await writeFile(join(tmpdir(), "wendy-monitoring-dashboard.png"), (await window.webContents.capturePage()).toPNG());
  await clickText("Pause");
  const pausedCalls = calls.snapshots.length;
  await delay(2200);
  assert.equal(calls.snapshots.length, pausedCalls);
  await clickText("Resume");
  await wait(`document.body.textContent.includes('Live')`);
  await clickSelector('#logs-tab');
  await wait(`document.body.textContent.includes('Camera is ready')`);
  assert.equal(calls.logs.at(-1).target, "vm:fixture-b");
  assert.equal(await evaluate(`document.querySelector('#target').value`), "docker", "Switching devices and Logs preserves the build target");
  await fill('[aria-label="Search logs"]', "dropped");
  await wait(`document.querySelectorAll('.desktop-log-entry').length === 1`);
  await clickSelector('.desktop-log-entry summary');
  assert.equal(await evaluate(`document.querySelector('.desktop-log-entry').open`), true);
  await delay(200);
  await writeFile(join(tmpdir(), "wendy-monitoring-logs.png"), (await window.webContents.capturePage()).toPNG());
  await clickText("Pause");
  await wait(`document.body.textContent.includes('Paused')`);
  assert.ok(calls.stops.length > 0);
  await clickText("Resume");
  await wait(`document.querySelectorAll('.desktop-log-entry').length === 1`);
  await clickText("Containers");
  await wait(`!!document.querySelector('.desktop-table-wrap tbody tr')`);
  await clickText("Logs");
  await wait(`document.body.textContent.includes('Camera is ready')`);
  assert.deepEqual(calls.logs.at(-1).source, { kind: "container", runtime: "docker", id: "fixture-container" });
  assert.equal(await evaluate(`document.querySelectorAll('.desktop-table-wrap').length`), 0, "Container logs must open directly instead of below the inventory");
  await clickText("← Back to containers");
  await wait(`!!document.querySelector('.desktop-table-wrap tbody tr')`);
  await clickText("Devices & monitoring");
  await clickText("← Back to devices");
  await clickSelector('[aria-label="Dashboard for fixture-unavailable"]');
  await wait(`document.querySelector('.desktop-dashboard [role="alert"]')?.textContent.includes('A secure connection')`);
  assert.equal(await evaluate(`document.querySelectorAll('.desktop-dashboard .desktop-metrics, .desktop-dashboard table').length`), 0, "Unavailable snapshots must not imply resource values or zero applications");
  assert.doesNotMatch(await evaluate(`document.querySelector('[role="alert"]').textContent`), /remote method|ssh|WENDY_|wendy:/);
  await clickSelector('#logs-tab');
  await wait(`document.querySelector('.desktop-device-logs [role="alert"]')?.textContent.includes('A secure connection')`);
  assert.doesNotMatch(await evaluate(`document.querySelector('[role="alert"]').textContent`), /remote method|WENDY_|wendy:/);
  assert.equal(await evaluate(`document.querySelector('#target').value`), "docker");
  assert.equal(calls.sessions, 0);
  assert.deepEqual(errors, []);
  console.log("PASS: native Dashboard, target switching, repeated opens, navigation, pause/resume, structured Logs, search, details, and container logs.");
  console.log(`Screenshots: ${join(tmpdir(), "wendy-monitoring-dashboard.png")} and ${join(tmpdir(), "wendy-monitoring-logs.png")}`);
} catch (error) {
  console.error(error);
  process.exitCode = 1;
} finally {
  window?.destroy();
  await rm(data, { recursive: true, force: true });
  app.exit(process.exitCode || 0);
}

}
void main().catch((error) => { console.error(error); app.exit(1); });
