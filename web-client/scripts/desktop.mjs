import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { cp, mkdir, readFile, readdir, writeFile, rm } from "node:fs/promises";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { rebuild } from "@electron/rebuild";

const site = fileURLToPath(new URL("..", import.meta.url));
const root = resolve(site, "..");
const resources = join(site, "desktop-resources");
const mode = process.argv[2] || "dev";
const children = new Set();
let stopping = false;

function run(file, args, options = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(file, args, {
      cwd: site,
      stdio: "inherit",
      ...options,
    });
    children.add(child);
    child.once("error", (error) => {
      children.delete(child);
      reject(error);
    });
    child.once("exit", (code, signal) => {
      children.delete(child);
      if (stopping || code === 0) resolve();
      else reject(new Error(`${file} exited with ${signal || code}`));
    });
  });
}

function stop() {
  stopping = true;
  for (const child of children) child.kill("SIGTERM");
}
process.once("SIGINT", stop);
process.once("SIGTERM", stop);

async function copySource(kind) {
  const source = join(root, "go/simulator", kind);
  const target = join(resources, kind);
  // The same source families embedded by the CLI. Exclude downloaded assets,
  // local Python environments and integration artifacts from the desktop bundle.
  const files = [
    "Dockerfile",
    "entrypoint.sh",
    "cyclonedds.xml",
    "requirements.txt",
    "assets.lock.json",
    "unitree.lock.json",
    "UPSTREAM.md",
    "compatibility.json",
    `${kind}_sim`,
    "tools",
    "licenses",
    "ros_ws/src",
  ];
  if (kind === "go2")
    files.push("requirements-visuals.txt", "visuals.lock.json");
  await rm(target, { recursive: true, force: true });
  await mkdir(target, { recursive: true });
  for (const path of files)
    await cp(join(source, path), join(target, path), {
      recursive: true,
      filter: (path) => !/(__pycache__|\.pyc$|\.DS_Store$)/.test(path),
    });
  const hash = createHash("sha256");
  async function visit(path, relative = "") {
    const entries = await readdir(path, { withFileTypes: true });
    for (const entry of entries.sort((a, b) => a.name.localeCompare(b.name))) {
      const rel = `${relative}/${entry.name}`;
      if (entry.isDirectory()) await visit(join(path, entry.name), rel);
      else {
        hash.update(rel);
        hash.update(await readFile(join(path, entry.name)));
      }
    }
  }
  await visit(target);
  await writeFile(
    join(resources, `${kind}-digest.txt`),
    hash.digest("hex") + "\n",
  );
}

async function prepare() {
  await mkdir(resources, { recursive: true });
  console.log("Building the desktop's Wendy CLI...");
  await run(
    "go",
    [
      "build",
      "-o",
      join(resources, process.platform === "win32" ? "wendy.exe" : "wendy"),
      "./go/cmd/wendy",
    ],
    { cwd: root },
  );
  if (stopping) return;
  await copySource("go2");
  await copySource("g1");
  const electronVersion = JSON.parse(
    await readFile(join(site, "node_modules/electron/package.json"), "utf8"),
  ).version;
  console.log(`Preparing the terminal for Electron ${electronVersion}...`);
  await rebuild({
    buildPath: site,
    electronVersion,
    onlyModules: ["node-pty"],
  });
}

async function main() {
  if (!["dev", "build", "package"].includes(mode))
    throw new Error("Use desktop.mjs dev, build, or package.");
  await prepare();
  if (stopping) return;
  const vite = join(site, "node_modules/vite/bin/vite.js");
  if (mode === "dev") {
    // Vite's API reports readiness before Electron opens, without racing a port.
    const { createServer } = await import("vite");
    const server = await createServer({
      configFile: join(site, "vite.desktop.config.ts"),
    });
    await server.listen();
    server.printUrls();
    const { default: electron } = await import("electron");
    try {
      await run(electron, [site], {
        env: {
          ...process.env,
          WENDY_DESKTOP_DEV_URL: "http://127.0.0.1:5174/",
        },
      });
    } finally {
      await server.close();
    }
  } else {
    await run(process.execPath, [
      vite,
      "build",
      "--config",
      "vite.desktop.config.ts",
    ]);
    if (stopping || mode === "build") return;
    // Package only desktop files and its one native production dependency.
    const stage = join(site, ".desktop-package");
    await rm(stage, { recursive: true, force: true });
    await mkdir(stage, { recursive: true });
    for (const path of [
      "electron",
      "dist-desktop",
      "desktop-resources",
      "node_modules/node-pty",
    ])
      await cp(join(site, path), join(stage, path), { recursive: true });
    await writeFile(
      join(stage, "package.json"),
      JSON.stringify({
        name: "wendy-desktop",
        productName: "Wendy Desktop",
        version: "0.1.0",
        type: "module",
        main: "electron/main.mjs",
      }),
    );
    const { packager } = await import("@electron/packager");
    const electronVersion = JSON.parse(
      await readFile(join(site, "node_modules/electron/package.json"), "utf8"),
    ).version;
    const paths = await packager({
      dir: stage,
      name: "Wendy Desktop",
      out: join(site, "release"),
      electronVersion,
      overwrite: true,
      prune: false,
      asar: false,
      appBundleId: "dev.wendy.desktop",
    });
    console.log(paths.join("\n"));
  }
}

main().catch((error) => {
  console.error(error);
  stop();
  process.exitCode = 1;
});
