// Stage a scratch image using the same Go toolchain as the CLI checkout. This
// avoids requiring a separately maintained Go+Node builder image.
import { execFileSync } from "node:child_process";
import { cpSync, mkdirSync, rmSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { resolve, join } from "node:path";
const site = fileURLToPath(new URL("..", import.meta.url));
const root = resolve(site, "..");
const output = join(site, ".device-package");
execFileSync("npm", ["run", "device:build"], { cwd: site, stdio: "inherit" });
mkdirSync(output, { recursive: true });
for (const arch of ["arm64", "amd64"]) {
  execFileSync("go", ["build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", join(output, `wendy-device-web-${arch}`), "./go/cmd/wendy-device-web"], {
    cwd: root, stdio: "inherit", env: { ...process.env, GOOS: "linux", GOARCH: arch, CGO_ENABLED: "0" },
  });
}
rmSync(join(output, "www"), { recursive: true, force: true });
cpSync(join(site, "dist-device"), join(output, "www"), { recursive: true });
cpSync(join(site, "device", "Dockerfile"), join(output, "Dockerfile"));
cpSync(join(site, "device", "wendy.json"), join(output, "wendy.json"));
console.log(`Device image context: ${output}`);
