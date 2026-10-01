import { spawn } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { createConnection, createServer } from "node:net";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";

const site = fileURLToPath(new URL("..", import.meta.url));
const root = resolve(site, "..");
const children = new Set();
let stopping = false;
let work;

function start(command, args, cwd = site) {
  const child = spawn(command, args, { cwd, stdio: "inherit" });
  children.add(child);
  const exited = new Promise((resolve) => {
    child.once("error", (error) => {
      children.delete(child);
      resolve({ code: 1, error });
    });
    child.once("exit", (code, signal) => {
      children.delete(child);
      resolve({ code: code ?? 1, signal });
    });
  });
  return { child, exited };
}

async function stop(code) {
  if (stopping) return;
  stopping = true;
  process.exitCode = code;
  const exits = [...children].map((child) => new Promise((resolve) => {
    child.once("exit", resolve);
    child.kill("SIGTERM");
  }));
  const force = setTimeout(() => {
    for (const child of children) child.kill("SIGKILL");
  }, 5000);
  await Promise.all(exits);
  clearTimeout(force);
  if (work) await rm(work, { recursive: true, force: true });
}

process.once("SIGINT", () => void stop(130));
process.once("SIGTERM", () => void stop(143));

async function requireFreePort(port, host) {
  const server = createServer();
  await new Promise((resolve, reject) => {
    server.once("error", (error) => reject(new Error(
      `Cannot listen on ${host}:${port}. Stop the existing dev server or relay and rerun npm run dev.`,
      { cause: error },
    )));
    server.listen(port, host, resolve);
  });
  await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
}

async function relayReady() {
  // The port was checked before spawning our relay. Wait for its listener,
  // while the caller also watches for an early process exit.
  const deadline = Date.now() + 10000;
  while (!stopping && Date.now() < deadline) {
    const connected = await new Promise((resolve) => {
      const socket = createConnection({ host: "127.0.0.1", port: 8788 });
      const finish = (ready) => { socket.destroy(); resolve(ready); };
      socket.once("connect", () => finish(true));
      socket.once("error", () => finish(false));
      socket.setTimeout(250, () => finish(false));
    });
    if (connected) return;
    await delay(100);
  }
  throw new Error("The browser relay did not start on 127.0.0.1:8788.");
}

async function main() {
  await requireFreePort(5173, "localhost");
  await requireFreePort(8788, "127.0.0.1");
  if (stopping) return;
  work = await mkdtemp(join(tmpdir(), "wendy-web-dev-"));
  if (stopping) { await rm(work, { recursive: true, force: true }); return; }
  const binary = join(work, process.platform === "win32" ? "wendy-web-relay.exe" : "wendy-web-relay");
  console.log("Building the browser relay...");
  const build = await start("go", ["build", "-o", binary, "./go/cmd/wendy-web-relay"], root).exited;
  if (stopping) return;
  if (build.error) throw build.error;
  if (build.code !== 0) throw new Error(`Relay build failed with exit code ${build.code}.`);

  // Run the built executable directly, so shutdown cannot orphan go run's child.
  const relay = start(binary, ["-cloud"]);
  const relayExited = relay.exited.then((result) => {
    throw result.error ?? new Error(`Browser relay exited with code ${result.code}.`);
  });
  await Promise.race([relayReady(), relayExited]);
  if (stopping) return;

  const dev = start(process.execPath, [
    fileURLToPath(new URL("./cli.js", import.meta.resolve("vinext"))),
    "dev", "--port", "5173", ...process.argv.slice(2),
  ]);
  const result = await Promise.race([dev.exited, relayExited]);
  if (result.error) throw result.error;
  await stop(result.code);
}

main().catch(async (error) => {
  if (!stopping) {
    console.error(error.message);
    await stop(1);
  }
});
