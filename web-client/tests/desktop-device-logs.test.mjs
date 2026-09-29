import test from "node:test";
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { DeviceLogs } from "../electron/device-logs.mjs";

function fixture(t) {
  const launches = [];
  const logs = new DeviceLogs({
    cli: "/bundled/wendy", env: { PATH: "/usr/bin" },
    spawn(file, args, options) {
      const child = new EventEmitter();
      child.stdout = new PassThrough();
      child.stderr = new PassThrough();
      child.signals = [];
      child.kill = (signal) => { child.signals.push(signal); return true; };
      launches.push({ file, args, options, child });
      return child;
    },
  });
  t.after(() => {
    logs.close();
    launches.forEach(({ child }) => child.emit("close", 0));
  });
  return { logs, launches };
}

const record = (body, extra = {}) => JSON.stringify({
  timestamp: "2026-09-27T13:42:00.123456789Z", severity: "INFO", service: "camera", body, ...extra,
}) + "\n";

test("device streams pin the explicit target and use JSON pipes without a terminal", (t) => {
  const { logs, launches } = fixture(t);
  const { id } = logs.start({ target: "vm:pi-lab" });
  assert.deepEqual(launches[0].args, ["device", "logs", "--device", "vm:pi-lab", "--json", "--read-only", "--tail", "100", "--min-severity", "0"]);
  assert.equal(launches[0].file, "/bundled/wendy");
  assert.equal(launches[0].options.shell, false);
  assert.deepEqual(launches[0].options.stdio, ["ignore", "pipe", "pipe"]);
  assert.equal(logs.snapshot(id).target, "vm:pi-lab");
  logs.start({ target: "192.168.0.5", app: "--unsafe option; echo nope", level: "warn" });
  assert.deepEqual(launches[1].args.slice(-3), ["--app=--unsafe option; echo nope", "--level", "warn"]);
  for (const target of ["", "--help", "local", "docker", "apple-container", "sim", "host\n--help"])
    assert.throws(() => logs.start({ target }));
  assert.throws(() => logs.start({ target: "vm:pi", level: "loud" }));
  assert.throws(() => logs.start({ target: "vm:pi", app: {} }));
  assert.equal(launches.length, 2);
});

test("records survive chunked UTF-8, malformed lines, and final lines without a newline", (t) => {
  const { logs, launches } = fixture(t);
  const { id } = logs.start({ target: "vm:pi" });
  const child = launches[0].child;
  const encoded = Buffer.from(record("Camera 📷 ready", { attributes: { sensor: "left" } }));
  const emoji = encoded.indexOf(Buffer.from("📷"));
  child.stdout.write(encoded.subarray(0, emoji + 2));
  child.stdout.write(encoded.subarray(emoji + 2));
  child.stdout.write("not json\n[]\n{\"timestamp\":\"bad\"}\n");
  child.stdout.write(record("\u001b[31mWarm\u001b[0m", { severity: "WARN" }).trimEnd());
  child.emit("close", 0);
  const result = logs.snapshot(id);
  assert.equal(result.running, false);
  assert.equal(result.exitCode, 0);
  assert.equal(result.entries.length, 2);
  assert.equal(result.entries[0].body, "Camera 📷 ready");
  assert.equal(result.entries[0].attributes.sensor, "left");
  assert.equal(result.entries[1].body, "Warm");
  assert.equal(result.entries[1].severity, "warn");
  assert.equal(result.dropped, 3);
});

test("read-only VM monitoring keeps the VM alias in the actual connection command", (t) => {
  const { logs, launches } = fixture(t);
  const { id } = logs.start({ target: "vm:pi-lab", connectionTarget: "127.0.0.1:50051" });
  assert.deepEqual(launches[0].args.slice(0, 4), ["device", "logs", "--device", "vm:pi-lab"]);
  assert.ok(launches[0].args.includes("--read-only"));
  assert.equal(logs.snapshot(id).target, "vm:pi-lab");
});

test("buffers stay bounded by count, total bytes, and partial line size", (t) => {
  const { logs, launches } = fixture(t);
  const { id } = logs.start({ target: "vm:pi" });
  const child = launches[0].child;
  for (let i = 0; i < 520; i++) child.stdout.write(record(`row ${i}`));
  assert.equal(logs.snapshot(id).entries.length, 500);
  assert.equal(logs.snapshot(id).entries[0].body, "row 20");
  child.stdout.write("x".repeat(48 * 1024));
  child.stdout.write("x".repeat(48 * 1024));
  child.stdout.write("\n" + record("after oversized line"));
  assert.equal(logs.streams.get(id).pending.length, 0);
  assert.equal(logs.snapshot(id).entries.at(-1).body, "after oversized line");
  for (let i = 0; i < 100; i++) child.stdout.write(record("x".repeat(40 * 1024)));
  assert.ok(Buffer.byteLength(JSON.stringify(logs.snapshot(id).entries)) < 2 * 1024 * 1024 + 1000);
  assert.ok(logs.snapshot(id).entries.length < 100);
  assert.ok(logs.snapshot(id).dropped > 500);
});

test("streams remain isolated and stop and close terminate their own children", (t) => {
  const { logs, launches } = fixture(t);
  const first = logs.start({ target: "vm:first" });
  const second = logs.start({ target: "vm:second" });
  launches[0].child.stdout.write(record("first only"));
  launches[1].child.stdout.write(record("second only"));
  assert.equal(logs.snapshot(second.id).entries[0].body, "second only");
  logs.stop(first.id);
  assert.deepEqual(launches[0].child.signals, ["SIGTERM"]);
  assert.deepEqual(launches[1].child.signals, []);
  assert.throws(() => logs.snapshot(first.id), /no longer open/);
  logs.stop(first.id);
  logs.close();
  assert.deepEqual(launches[1].child.signals, ["SIGTERM"]);
  assert.equal(logs.streams.size, 0);
});

test("process failures become bounded readable errors and retained logs stay visible", (t) => {
  const { logs, launches } = fixture(t);
  const first = logs.start({ target: "vm:offline" });
  launches[0].child.stdout.write(record("before disconnect"));
  launches[0].child.stderr.write("x".repeat(20_000) + "\nConnection lost");
  launches[0].child.emit("close", 1);
  const result = logs.snapshot(first.id);
  assert.equal(result.running, false);
  assert.equal(result.entries.length, 1);
  assert.match(result.error, /Connection lost/);
  assert.ok(result.error.length <= 8192);
  const second = logs.start({ target: "vm:missing" });
  launches[1].child.emit("error", new Error("Executable not found"));
  assert.equal(logs.snapshot(second.id).error, "Executable not found");
  assert.equal(logs.snapshot(second.id).running, false);
});

test("the number of concurrently open streams is bounded", (t) => {
  const { logs, launches } = fixture(t);
  const first = logs.start({ target: "vm:pi" });
  for (let i = 1; i < 8; i++) logs.start({ target: `vm:pi-${i}` });
  assert.throws(() => logs.start({ target: "vm:ninth" }), /Close another log view/);
  assert.equal(launches.length, 8);
  logs.stop(first.id);
  assert.doesNotThrow(() => logs.start({ target: "vm:ninth" }));
});

test("a child that ignores graceful stop is terminated forcibly", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const { logs, launches } = fixture(t);
  const { id } = logs.start({ target: "vm:pi" });
  logs.stop(id);
  assert.deepEqual(launches[0].child.signals, ["SIGTERM"]);
  t.mock.timers.tick(1000);
  assert.deepEqual(launches[0].child.signals, ["SIGTERM", "SIGKILL"]);
});

test("plain container and boot streams keep stdout and stderr with independent partial lines", (t) => {
  const { logs, launches } = fixture(t);
  const { id } = logs.startCommand({ target: "Camera container", file: "docker", args: ["logs", "--follow", "--tail", "100", "camera-id"] });
  const child = launches[0].child;
  child.stdout.write("2026-09-27T10:20:30.001Z started ");
  child.stderr.write("warning ");
  child.stdout.write("camera\n");
  child.stderr.write("from stderr\n");
  child.stdout.write("last stdout");
  child.stderr.write("last stderr");
  child.emit("close", 0);
  assert.equal(launches[0].file, "docker");
  const rows = logs.snapshot(id).entries;
  assert.deepEqual(rows.map((row) => row.body), ["started camera", "warning from stderr", "last stdout", "last stderr"]);
  assert.equal(rows[0].timestamp, "2026-09-27T10:20:30.001Z");
  assert.equal(logs.snapshot(id).error, undefined);
  assert.throws(() => logs.startCommand({ target: "bad", file: "docker", args: ["\0"] }));
});

test("plain stderr is bounded even without a newline and parser recovers afterward", (t) => {
  const { logs, launches } = fixture(t);
  const { id } = logs.startCommand({ target: "Boot logs", args: ["vm", "logs", "pi", "--follow"] });
  const child = launches[0].child;
  child.stderr.write("x".repeat(80 * 1024));
  assert.equal(logs.streams.get(id).stderrPending.length, 0);
  assert.ok(logs.streams.get(id).stderr.length <= 8192);
  child.stderr.write("\nRecovered\n");
  assert.equal(logs.snapshot(id).entries[0].body, "Recovered");
  assert.equal(logs.snapshot(id).dropped, 1);
});
