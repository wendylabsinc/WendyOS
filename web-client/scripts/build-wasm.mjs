// Run from web-client, inside the Wendy monorepo.
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync, mkdtempSync, rmSync } from "node:fs";
import { gzipSync } from "node:zlib";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
const site = resolve(fileURLToPath(new URL("..", import.meta.url)));
const root = resolve(site, "..");
const work = mkdtempSync(join(tmpdir(), "wendy-web-"));
try {
  execFileSync(
    "go",
    [
      "build",
      "-ldflags=-s -w",
      "-o",
      join(work, "wendy.wasm"),
      "./go/cmd/wendy-web",
    ],
    {
      cwd: root,
      env: { ...process.env, GOOS: "js", GOARCH: "wasm", CGO_ENABLED: "0" },
      stdio: "inherit",
    },
  );
  const goroot = execFileSync("go", ["env", "GOROOT"], {
    cwd: root,
    encoding: "utf8",
  }).trim();
  writeFileSync(
    join(site, "public/wasm_exec.js"),
    readFileSync(join(goroot, "lib/wasm/wasm_exec.js")),
  );
  const bytes = gzipSync(readFileSync(join(work, "wendy.wasm")), { level: 9 });
  writeFileSync(join(site, "public/wendy.wasm.gz"), bytes);
  console.log(
    `Built browser client: ${(bytes.length / 1024 / 1024).toFixed(1)} MiB compressed`,
  );
} finally {
  rmSync(work, { recursive: true, force: true });
}
