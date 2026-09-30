// Reproducible static renders of the repository's G1 MJCF/STL and Go2 GLB.
// Run from any directory after `npm --prefix web-client/mcp-app ci`:
//   node web-client/mcp-app/scripts/render-robot-images.mjs
// CHROME_BIN can select a Chromium/Chrome executable on other platforms.
import { createServer } from "node:http";
import { spawn } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile, access, mkdir } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { build } from "esbuild";

const app = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const repo = resolve(app, "../..");
const g1 = join(repo, "Examples/G1FruitNinjaMujoco/models/unitree_g1");
const output = join(app, "src/assets/devices");
const chrome = process.env.CHROME_BIN || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
await access(chrome);
await mkdir(output, { recursive: true });
const temporary = await mkdtemp(join(tmpdir(), "wendy-device-renders-"));
const bundle = await build({ entryPoints: [join(app, "scripts/render-robot-images.client.js")], bundle: true, format: "esm", write: false });
let completed, failed;
const server = createServer(async (request, response) => {
  try {
    const path = new URL(request.url, "http://localhost").pathname;
    if (request.method === "POST" && (path === "/result" || path === "/error")) {
      let text = "";
      for await (const chunk of request) {
        text += chunk;
        if (text.length > 4_000_000) throw new Error("Render response too large");
      }
      if (path === "/error") throw new Error(text);
      const result = JSON.parse(text);
      if (!["g1", "go2"].includes(result.id) || !result.image.startsWith("data:image/webp;base64,")) throw new Error("Invalid render result");
      const bytes = Buffer.from(result.image.split(",")[1], "base64");
      await writeFile(join(output, result.id + ".webp"), bytes);
      response.end("saved");
      console.log(`${result.id}: 768×768 transparent WebP, ${bytes.length} bytes; source dimensions ${result.dimensions.map(n => n.toFixed(3)).join(" × ")}`);
      completed();
      return;
    }
    if (path === "/") {
      response.setHeader("Content-Type", "text/html");
      response.end('<!doctype html><html><body><script type="module" src="/render.js"></script></body></html>');
    } else if (path === "/render.js") {
      response.setHeader("Content-Type", "text/javascript");
      response.end(bundle.outputFiles[0].contents);
    } else if (path === "/models/g1.xml") {
      response.end(await readFile(join(g1, "g1_29dof.xml")));
    } else if (/^\/models\/g1\/meshes\/[a-zA-Z0-9_-]+\.STL$/.test(path)) {
      response.end(await readFile(join(g1, "meshes", path.split("/").at(-1))));
    } else if (path === "/models/go2.glb") {
      response.end(await readFile(join(repo, "go/internal/cli/mcp/desktop_assets/go2.glb")));
    } else {
      response.writeHead(404).end();
    }
  } catch (error) {
    response.writeHead(500).end("Render failed");
    failed?.(error);
  }
});
await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
try {
  for (const model of ["g1", "go2"]) {
    const result = new Promise((resolve, reject) => { completed = resolve; failed = reject; });
    const child = spawn(chrome, ["--headless=new", "--disable-background-networking", "--disable-component-update", "--no-first-run", "--no-default-browser-check", `--user-data-dir=${join(temporary, model)}`, `http://127.0.0.1:${server.address().port}/?model=${model}`], { stdio: "ignore" });
    const exited = new Promise(resolve => {
      child.once("exit", resolve);
      child.once("error", resolve);
    });
    const timer = setTimeout(() => failed(new Error(`Rendering ${model} timed out`)), 60_000);
    child.on("error", error => failed(error));
    try { await result; } finally {
      clearTimeout(timer);
      child.kill();
      await exited;
    }
  }
} finally {
  server.closeAllConnections();
  await new Promise(resolve => server.close(resolve));
  await rm(temporary, { recursive: true, force: true });
}
