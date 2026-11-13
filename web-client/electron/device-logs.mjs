import { spawn as spawnProcess } from "node:child_process";
import { randomUUID } from "node:crypto";
import { StringDecoder } from "node:string_decoder";
import { stripVTControlCharacters } from "node:util";
import { deviceTarget, text } from "./policy.mjs";

const MAX_ROWS = 500;
const MAX_BYTES = 2 * 1024 * 1024;
const MAX_LINE = 64 * 1024;
const MAX_STREAMS = 8;
const LEVELS = new Set(["all", "trace", "debug", "info", "warn", "error", "fatal"]);
const clean = (value, limit = MAX_LINE) =>
  stripVTControlCharacters(String(value ?? ""))
    .replace(/[\x00-\x08\x0b-\x1f\x7f]/g, "")
    .slice(0, limit);

// Device logs use a bounded JSON pipe, independent of interactive sessions.
export class DeviceLogs {
  constructor({ cli, env, spawn = spawnProcess }) {
    Object.assign(this, { cli, env, spawn });
    this.streams = new Map();
  }

  start({ target, app = "", level = "all" }) {
    target = deviceTarget(target);
    if (typeof app !== "string") throw new Error("Invalid application name.");
    app = app.trim() ? text(app, "application name") : "";
    if (!LEVELS.has(level)) throw new Error("Invalid log severity.");
    const args = ["device", "logs", "--device", target, "--json", "--read-only", "--tail", "100"];
    if (app) args.push(`--app=${app}`);
    if (level === "all") args.push("--min-severity", "0");
    else args.push("--level", level);

    return this.createStream({ target, app, level, file: this.cli, args, format: "json" });
  }

  // Only the main-process service calls this after resolving a live, owned
  // container or simulator. Never expose arbitrary executable arguments to IPC.
  startCommand({ target, file = this.cli, args }) {
    target = text(target, "log target");
    if (typeof file !== "string" || !file || file.includes("\0") ||
        !Array.isArray(args) || args.length > 100 ||
        args.some((arg) => typeof arg !== "string" || arg.includes("\0") || arg.length > 16384))
      throw new Error("Invalid log command.");
    return this.createStream({ target, app: "", level: "all", file, args, format: "text" });
  }

  createStream({ target, app, level, file, args, format }) {
    if (this.streams.size >= MAX_STREAMS)
      throw new Error("Close another log view before opening more.");
    const child = this.spawn(file, args, {
      env: this.env,
      stdio: ["ignore", "pipe", "pipe"],
      windowsHide: true,
      shell: false,
    });
    const stream = {
      id: randomUUID(), target, app, level, child, format, running: true,
      entries: [], bytes: 0, sequence: 0, dropped: 0,
      pending: "", oversized: false, stderr: "", decoder: new StringDecoder("utf8"),
    };
    this.streams.set(stream.id, stream);
    child.stdout.on("data", (chunk) => this.consume(stream, stream.decoder.write(chunk)));
    stream.stderrDecoder = new StringDecoder("utf8");
    child.stderr.on("data", (chunk) => {
      stream.stderr = (stream.stderr + clean(String(chunk).slice(-8192), 8192)).slice(-8192);
      // Container runtimes send the container's stderr through their own stderr.
      // Keep it visible in plain log views instead of treating it as a CLI error.
      if (format === "text") this.consumeTextStderr(stream, stream.stderrDecoder.write(chunk));
    });
    child.on("error", (error) => {
      stream.running = false;
      stream.error = clean(error.message, 2000);
    });
    child.on("close", (code, signal) => {
      this.consume(stream, stream.decoder.end());
      if (stream.pending && !stream.oversized) this.record(stream, stream.pending);
      if (format === "text") {
        this.consumeTextStderr(stream, stream.stderrDecoder.end());
        if (stream.stderrPending && !stream.stderrOversized) this.record(stream, stream.stderrPending);
      }
      stream.pending = "";
      stream.running = false;
      clearTimeout(stream.killTimer);
      if (typeof code === "number") stream.exitCode = code;
      if (!stream.stopped && !stream.error && (code !== 0 || signal))
        stream.error = stream.stderr.trim() || `The log stream ended${signal ? ` (${signal})` : ` with status ${code}`}.`;
    });
    return { id: stream.id };
  }

  consumeTextStderr(stream, data) {
    // Each pipe can end halfway through a different line. Keep their partial
    // lines separate while retaining one bounded record buffer.
    const pending = stream.pending, oversized = stream.oversized;
    stream.pending = stream.stderrPending || "";
    stream.oversized = stream.stderrOversized || false;
    this.consume(stream, data);
    stream.stderrPending = stream.pending;
    stream.stderrOversized = stream.oversized;
    stream.pending = pending;
    stream.oversized = oversized;
  }

  consume(stream, data) {
    if (stream.stopped) return;
    // Split each incoming chunk without accumulating an unbounded partial line.
    let offset = 0;
    while (offset < data.length) {
      const end = data.indexOf("\n", offset);
      const part = data.slice(offset, end < 0 ? data.length : end);
      if (!stream.oversized) {
        if (Buffer.byteLength(stream.pending) + Buffer.byteLength(part) > MAX_LINE) {
          stream.pending = "";
          stream.oversized = true;
          stream.dropped++;
        } else stream.pending += part;
      }
      if (end < 0) break;
      if (!stream.oversized) this.record(stream, stream.pending);
      stream.pending = "";
      stream.oversized = false;
      offset = end + 1;
    }
  }

  record(stream, line) {
    if (!line.trim()) return;
    let raw;
    if (stream.format === "text") {
      const timestamp = line.match(/^(\d{4}-\d\d-\d\dT\S+)\s/);
      const valid = timestamp && Number.isFinite(Date.parse(timestamp[1]));
      raw = { timestamp: valid ? timestamp[1] : new Date().toISOString(), body: valid ? line.slice(timestamp[0].length) : line };
    } else {
      try { raw = JSON.parse(line); } catch { stream.dropped++; return; }
    }
    if (!raw || typeof raw !== "object" || Array.isArray(raw) ||
        typeof raw.timestamp !== "string" || !Number.isFinite(Date.parse(raw.timestamp))) {
      stream.dropped++;
      return;
    }
    const attributes = Object.create(null);
    if (raw.attributes && typeof raw.attributes === "object" && !Array.isArray(raw.attributes)) {
      for (const [key, value] of Object.entries(raw.attributes).slice(0, 100))
        attributes[clean(key, 256)] = clean(typeof value === "string" ? value : JSON.stringify(value), 4096);
    }
    const entry = {
      id: ++stream.sequence,
      timestamp: raw.timestamp.slice(0, 64),
      severity: clean(raw.severity, 32).toLowerCase(),
      service: clean(raw.service, 256),
      body: clean(typeof raw.body === "string" ? raw.body : JSON.stringify(raw.body ?? "")),
      attributes,
    };
    const bytes = Buffer.byteLength(JSON.stringify(entry));
    stream.entries.push({ entry, bytes });
    stream.bytes += bytes;
    while (stream.entries.length > MAX_ROWS || stream.bytes > MAX_BYTES) {
      stream.bytes -= stream.entries.shift().bytes;
      stream.dropped++;
    }
  }

  snapshot(id) {
    const stream = this.streams.get(id);
    if (!stream) throw new Error("This log view is no longer open.");
    return {
      id: stream.id, target: stream.target, app: stream.app, level: stream.level,
      running: stream.running, entries: stream.entries.map(({ entry }) => entry),
      dropped: stream.dropped, error: stream.error, exitCode: stream.exitCode,
    };
  }

  stop(id) {
    const stream = this.streams.get(id);
    if (!stream) return;
    this.streams.delete(id);
    stream.stopped = true;
    if (!stream.running) return;
    stream.child.kill("SIGTERM");
    stream.killTimer = setTimeout(() => {
      if (stream.running) stream.child.kill("SIGKILL");
    }, 1000);
    stream.killTimer.unref?.();
  }

  close() {
    for (const id of this.streams.keys()) this.stop(id);
  }
}
