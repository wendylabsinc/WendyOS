Runs your app on a Wendy-enabled device:

1. [Selects a device](../device-selection.md)
2. [Queries the platform and architecture](./device/version.md) of this device
3. Invokes a [build](./build.md) using the target triple, and injects a [debugger](../../../debugging/) if needed
4. Uploads the artifact(s) for [Linux](../../../wendy-agent/connectivity/container-registry.md) or [macOS](../../../wendy-agent/macos/)
5. [Starts the app](./device/apps/start.md), then (attached runs only) waits for readiness and prints the reachable URL
6. [Attaches the logs](./device/logs.md) if needed (when `--detach` is not provided)

## Reachable app URLs

After the app starts and its readiness probe passes, `wendy run` prints an `App reachable at <url>` line when it can infer a browser URL from the app configuration:

```text
App reachable at http://192.168.123.222:3000
App reachable at http://[2001:db8::1]:3000
```

The CLI derives this URL from either:

- `hooks.postStart.openURL`, when the URL contains `WENDY_HOSTNAME`
- `readiness.tcpSocket.port`

The printed URL uses a routable IP address reported by the device instead of the `.local` hostname, which makes it easier to open from browsers that do not resolve mDNS names reliably. If neither an `openURL` hook nor a TCP readiness port is configured, or if the device cannot report an IP address, `wendy run` skips this line.

> **Note:** If the readiness probe fails (timeout or connection error), `wendy run` skips the `App reachable at` line and the `postStart` hook and prints a warning instead. This prevents opening a browser tab pointed at a container that has already exited.

> **Note:** When `wendy.json` is absent, `wendy run` resolves the target device before prompting to create one. If the target is Headless Mac and the detected project type is unsupported, the project/target mismatch error is returned immediately without opening the config creation prompt.

## ESP32 — native ESP-IDF projects

Regular ESP-IDF projects are the recommended app model for ESP32 targets. Wendy recognizes a project by its standard top-level `CMakeLists.txt`/`project.cmake` include or an `sdkconfig` file. Add a `wendy.json` with `"platform": "wendy-lite"`, then run:

```bash
wendy run --device <name>
```

The connected device must run a firmware variant with native app support. Wendy reads its chip target, ensures ESP-IDF 5.5.4 is available through `eim`, runs `idf.py set-target` when needed, builds the project, uploads the native application firmware, reboots, reconnects, and streams its console output. ESP-IDF projects are detected automatically; `--build-type` does not need to be set.

See [ESP32 installation](/docs/installation/wendy-lite-esp32) for setup and a minimal project layout.

## Headless Mac — supported project types

Headless Mac (Darwin targets) currently runs native macOS apps only. When the selected agent reports `os: darwin`, `wendy run` rejects Linux/container deployment paths before any build, registry auth, or registry setup.

| Project type | Mac target support |
|---|---|
| Native SwiftPM (`Package.swift`, `platform: "darwin"`) | Supported |
| Native Xcode (`.xcodeproj`, `platform: "darwin"`) | Supported |
| Dockerfile / Containerfile / container image | Rejected |
| Python container path | Rejected |
| Docker Compose | Rejected |
| Multi-service `wendy.json` (`services` map) | Rejected |
| `platform: "linux/..."` or `platform: "wendyos"` | Rejected |

The error explains the project/target mismatch and tells you to set `platform: "darwin"` with a Mac-compatible native SwiftPM or Xcode project, or to target a Linux/WendyOS device. Linux container support on Mac is planned for a future release.

## Image build-args

When building a Dockerfile or Containerfile project, `wendy run` passes the target device's hardware parameters as `--build-arg` values so the build file can branch on platform, GPU vendor, or CUDA version. Declare any arg you want to use with `ARG`:

On Apple silicon Macs with Apple's `container` runtime, Wendy tries
Apple Container first when `--builder` is omitted. If Apple Container is
unavailable or the build fails, Wendy falls back to Docker. Use
`--builder docker` to force Docker, or `--builder apple-container` to require
Apple Container:

```sh
wendy --device my-wendy.local run
```

Wendy automatically checks for the `container` CLI and offers to install it via Homebrew if missing, and starts the `system` and `builder` services if they are not running.

If Apple Container reports an empty build context for a project under `/tmp` or
`/private/tmp`, Wendy returns an error with the known workaround: move the
project to a non-`/tmp` directory and retry.

For local-only Dockerfile or Containerfile runs on the Mac itself, use `wendy run --device
apple-container` instead. Compose projects still require the Docker provider for
local runs, but compose service builds targeting a WendyOS device can use
`--builder apple-container`.

The interactive device picker hides local run targets (this machine,
Docker/OrbStack, Apple Container) by default so it lists separate WendyOS
devices first. Select one explicitly with `--device` (as above), or set
`WENDY_SHOW_LOCAL_DEVICES=1` to list them in the picker.

| Build-arg | Values | Notes |
|---|---|---|
| `WENDY_PLATFORM` | `nvidia-jetson` \| `generic` | Platform tier derived from the device type |
| `WENDY_DEBUG` | `true` \| `false` | Set when `--debug` is passed |
| `WENDY_DEVICE_TYPE` | e.g. `jetson-agx-orin` | Raw device type; absent when unknown |
| `WENDY_HAS_GPU` | `true` \| `false` | Absent on older agents |
| `WENDY_GPU_VENDOR` | e.g. `nvidia`, `qualcomm` | Absent when no GPU is reported |
| `WENDY_JETPACK_VERSION` | e.g. `6.0` | Jetson only |
| `WENDY_JETPACK_MAJOR` | e.g. `6`, `7` | Jetson only; JetPack major for per-generation base-image selection |
| `WENDY_CUDA_VERSION` | e.g. `12.6` | Jetson only |
| `WENDY_GPU_ARCH` | e.g. `sm_87` | GPU architecture identifier; absent when no GPU is reported |

`WENDY_PLATFORM` and `WENDY_DEBUG` are always set. The remaining args are only injected when the agent reports them, so Dockerfiles and Containerfiles can define their own `ARG` defaults for devices that predate the field.

## Multi-service projects (`wendy.json` with `services`)

When `wendy.json` contains a `services` map, `wendy run` automatically switches to the multi-service path:

1. All service images are built in parallel (up to 4 concurrent builds). In an interactive terminal a per-service spinner shows build progress; in non-interactive environments plain log lines are printed instead.
2. Containers are created individually in topological dependency order (services listed in `dependsOn` are created first).
3. All containers are started and their logs are streamed to stdout/stderr with a `[serviceName]` prefix per line.

Press **Ctrl-C** to stop all services. A 30-second graceful shutdown window is given before the CLI exits.

Use `--service <name>` to build and run only a specific service and its transitive `dependsOn` dependencies instead of the full set.

See [Multi-Service Apps with `wendy.json`](../../../apps/wendy-services.md) for a full walkthrough.

> **Note:** Every multi-service run rebuilds and re-pushes each service — the push-skip optimisation is currently inactive for multi-service deployments. See [Push-skip content verification](#push-skip-content-verification) for why.

> **Headless Mac:** Multi-service `wendy.json` projects are not supported when the selected target is Headless Mac. `wendy run` returns an error immediately. Target a Linux/WendyOS device for multi-service workloads.

## Compose projects

If the current directory contains a `docker-compose.yml` (or `compose.yml`) but no `wendy.json`, `wendy run` automatically runs it as a multi-service compose project. Each service is built, pushed, and started on the device in dependency order. See [Multi-Service Apps with Docker Compose](../../../apps/compose.md) for full details.

> **Headless Mac:** Compose projects are not supported when the selected target is Headless Mac. `wendy run` returns an error before performing any registry or Docker setup. To deploy a compose workload, target a Linux/WendyOS device. For Mac targets, use a native SwiftPM or Xcode project with `platform: "darwin"`.

## Swift Package Manager projects (macOS)

From a macOS (Darwin) SwiftPM project, target the Mac agent explicitly:

```bash
wendy run --device <hostname-or-ip>:50051
```

When running a Swift Package Manager project on a macOS target, `wendy run`:

1. Builds the project with `swift build -c release` (or `-c debug` when `--debug` is passed). (This is the native macOS build path; for the cross-compiled Linux container target's build configuration, see [Swift Package Manager projects](./build.md#swift-package-manager-projects) in `build.md`.)
2. Resolves the build products directory via `swift build --show-bin-path`.
3. Syncs the compiled binary to the device.
4. Automatically syncs any sibling `.bundle` and `.resources` directories found in the build products directory alongside the binary, so SwiftPM resource bundles are available at runtime.
5. Syncs `sandbox.sb` from the project root if present, and any additional files declared under `files` in `wendy.json`.
6. If a `Brewfile.wendy` or explicitly configured `brewfile` is present, syncs it to the device and the agent runs `brew bundle` before starting the app.

## Swift Package Manager projects — host requirements

Both the macOS-target and Linux-target Swift paths shell out to a host Swift toolchain. The following host OS requirements apply when no `Dockerfile` or `Containerfile` is present (or when `--build-type=swift` is set explicitly):

| Target platform | Supported host OS | Notes |
|-----------------|------------------|-------|
| macOS device | macOS only | Linux's Swift toolchain cannot cross-compile to macOS. |
| Linux device | macOS or Linux | swift-container-plugin does not yet ship for Windows. |

On a **Windows host**, `wendy run` returns an actionable error for Swift projects that would require the host toolchain. Providing a `Dockerfile` or `Containerfile` bypasses these restrictions — the build is routed through the image build path, which works on all platforms.

## Flags

| Flag | Description |
|------|-------------|
| `--deploy` | Build and create the container but do not start it. |
| `--detach` | Start the container and return without streaming logs, waiting for readiness, or opening the app URL. |
| `--restart-unless-stopped` | Restart the container unless manually stopped. |
| `--restart-on-failure` | Restart the container on failure. |
| `--no-restart` | Do not restart the container on exit. |
| `--debug` | Enable debug logging and inject debug tooling via `WENDY_DEBUG=true`. For SwiftPM projects (both native macOS and cross-compiled Linux container targets), builds with `-c debug` instead of `-c release`. |
| `--yes` / `-y` | Accept all device-selection prompts automatically. |
| `--builder <name>` | Image builder for Dockerfile/Containerfile builds: `docker` or `apple-container`. Cannot be combined with `--build-host`. |
| `--build-host <device>` | Build the image on another WendyOS device instead of this machine. See [Remote build host](#remote-build-host). |
| `--build-type <type>` | Override build type detection: `docker`, `swift`, or `python`. |
| `--prefix <dir>` | Run from a project directory other than the current working directory. |
| `--product <name>` | Swift Package Manager product to build and run (Swift projects only). |
| `--service <name>` | Build and run only the named service and its transitive dependencies (multi-service `wendy.json` projects only). Returns an error if the name does not match any key in the `services` map. |
| `--keep-going` | Deploy services that build successfully instead of aborting the whole group on the first build/push failure (multi-service projects only). |
| `--max-concurrency <n>` | Max service images to build+push at once in multi-service projects. 0 = default limit of 4. |
| `--user-args <args>` | Extra arguments to pass to the container at runtime. |
| `--env <KEY=VALUE>` | Set an environment variable in the container. Repeatable. Overrides a `wendy.json` `env` entry of the same key. See [Environment variables](#environment-variables). |
| `--chunking <mode>` | Controls the content-based chunking (CBC) chunk-diff deploy path: `auto` (default), `force`, or `off`. See [Deploy path: `--chunking`](#deploy-path---chunking). |
| `--watch` | Watch the project directory and redeploy on every change, streaming the app's logs between deploys. Runs non-interactive. See [Watch mode](#watch-mode). |
| `--debounce <ms>` | Watch mode only: quiet period in milliseconds after the last change before redeploying (default `400`). |
| `--verbose` | Watch mode only: always show build output. By default build output is hidden unless a build fails. |

## Remote build host

`--build-host` delegates the image build to another WendyOS device:

```bash
wendy run --build-host spark-office
```

The build host pushes the finished image directly into the target device's
registry over the mesh, using a direct LAN connection when possible and the
cloud broker otherwise. It addresses the provisioned target by asset ID without
resolving a hostname, so delivery does not depend on the build host resolving
`device-<id>.cloud.wendy.dev`. The image never travels through your machine.

Use a remote host for builds that need its GPU or CPU architecture, such as an
arm64 build that would otherwise use QEMU emulation on an x86 development machine.

Your machine does not need a container builder. With `--build-host`, the CLI
does not start Docker, Apple Container, or a local BuildKit daemon. The
`--builder` flag selects a local builder and cannot be combined with `--build-host`.

To set a default so you do not pass the flag every time, set `defaultBuildHost`
in the CLI config. The flag always wins over the default. This is a
per-developer setting rather than a project one, because the right build host
depends on which network you are on.

For a complete example, follow [Build Once, Deploy to Several Devices](/docs/guides/fleet-deployment).

### Requirements

- A Linux build host with the builder role enabled, BuildKit available, and
  support for the target platform.
- A user certificate in the build host's organisation.
- A provisioned target device reachable from the build host over the mesh.

See [`wendy device build-host`](device/build-host.md) for setup, access rules,
cache-space requirements, and how shared builds use the host.

If a requirement is missing, `wendy run` fails and names the host. It does not
fall back to building locally.

### Errors

A failed remote build reports which half failed, because the fixes differ:

- *build on `<host>` failed*: check your Dockerfile or Stagefile.
- *image built on `<host>` but could not be delivered*: check mesh reachability
  between the two devices and registry credentials on the build host.

## Watch mode

Pass `--watch` to rebuild and redeploy automatically whenever source files in the
project directory change:

```sh
wendy run --watch
wendy run --watch --debounce 800 --verbose
```

Watch mode runs **attached** and **non-interactive** (equivalent to `--yes`), so
the watch loop never blocks on a prompt. A rapid sequence of saves is coalesced
by the debounce window (default 400 ms) so a single redeploy runs after edits
settle. Build output is hidden unless a build fails; pass `--verbose` to always
show it, or `--debounce <ms>` to tune the quiet period.

Logs remain visible for the whole watch session and continue across redeploys.
For multi-service apps, this includes output from unchanged services that remain
running. A new session starts with new output rather than replaying recent lines
from before watch began. With an older agent, a small number of recent lines may
appear once at startup. Each cycle reports itself after the changed containers
have started, readiness has completed, and any first-run actions have launched:

```text
↻ change detected — redeploying...
✓ redeployed in 1.98s
listening on :3000
```

If you save again during a redeploy, Wendy cancels that redeploy and moves on to
the latest change once cancellation finishes. Deploys do not overlap.

**`openURL` and `cli` postStart actions run once per watch session for each
container, after its first successful readiness check.** Later saves do not
reopen the browser or rerun the local command. If readiness is canceled or
fails, a later successful redeploy can still run the action. Restart watch to
run it again. In a multi-service project, each service and the top-level action
run once independently. `postStart.agent` runs on the device after every
corresponding container start.

Ctrl-C stops watching and leaves the app running on the device; use
`wendy device apps stop` to stop it. Add `--detach` to keep watching and
redeploying without streaming logs or running `openURL` and `cli` actions.

Attached watch requires a Wendy agent target. For provider targets, use
`--watch --detach`.

For multi-service `wendy.json` and Compose projects deployed to WendyOS, watch
redeploys only services whose build inputs or runtime configuration changed.
Unchanged services that are still running are not rebuilt, recreated, or
restarted. Changed services are redeployed in dependency order. Missing or
stopped services are deployed again even when their files have not changed.
When the primary of a `shared-network` or `shared-ipc` group changes, the group
is restarted together because its other containers share that primary's Linux
namespaces.

Watch mode does not forward stdin to a container. Use a plain `wendy run` for
an app that reads stdin.

> **Note:** `wendy watch` is a hidden alias for `wendy run --watch`. Prefer
> `wendy run --watch`; both forms accept `--detach`.

## Deploy path: `--chunking`

`wendy run` normally attempts a fast content-based chunking (CBC) chunk-diff deploy and falls back to a full registry push when it fails (`auto`, the default). Use `--chunking` to override this:

| Value | Behaviour |
|-------|-----------|
| `auto` (default) | Try chunk-diff; fall back to a registry push on failure. |
| `force` | Use chunk-diff only. If chunk-diff fails the error is returned and no registry-push fallback is attempted. Cancellation still exits cleanly. |
| `off` | Skip chunk-diff entirely; go straight to the registry push. |

> **Note:** When `--deploy` is also passed, `--chunking force` and `--chunking off` are no-ops — `--deploy` always uses the registry path because it must create the container without starting it.

> **Note:** The `postStart` hook fires on both the chunk-diff and registry-push
> paths. The deploy path does not affect hook execution.

Any value other than `auto`, `force`, or `off` is rejected with an error before the build starts.

## Environment variables

Environment variables reach the container from two places:

```sh
wendy run --env LOG_LEVEL=debug --env OTEL_LOGS_EXPORTER=console
```

and the `env` map in `wendy.json`, which is where they belong when they are part of the app rather than one run of it:

```json
{
  "appId": "my-app",
  "env": {
    "LOG_LEVEL": "info",
    "API_TOKEN": "${MY_API_TOKEN}"
  }
}
```

`${VAR}` (or `$VAR`) is expanded from the deploying machine's environment, so secrets stay out of the file. An entry whose value expands to empty is dropped, leaving whatever the image itself sets.

For a multi-service app, the top-level `env` is the default for every service and a service's own `env` overrides it key by key. `--env` overrides both.

Keys must be POSIX-portable environment variable names (letters, digits and `_`, not starting with a digit); the agent additionally reserves the `WENDY_`, `LD_` and `DYLD_` prefixes.

## Push-skip content verification

When a detached run (`--detach`) finds that nothing has changed since the last successful deploy to this device, `wendy run` can skip the build and push entirely and just ensure the existing container is running. So this never leaves the device on stale or partial content, the skip is content-verified — it happens only when **all** of the following hold:

1. The build inputs (context, Dockerfile/Containerfile, platform, and build-args) hash the same as the last deploy.
2. A local deploy record for this app on this device exists and lists the image layer diff IDs that were deployed.
3. The device confirms it still holds every one of those recorded layers.

If any check fails — an older agent that cannot answer the layer query, a layer garbage-collected on the device, a partial push, or a locally rebuilt base image that never changed the input hash — `wendy run` falls back to a full build and push, recording fresh layer IDs on success.

Deploy records written before this version carry no layer IDs, so they cannot be verified and never skip. In practice:

- The first deploy after upgrading always does a full build and push.
- A legacy record (or any record without verifiable layer IDs) is treated as unverifiable rather than skipped, so you see a full rebuild with unchanged inputs instead of a silent skip onto possibly-stale content.

> **Note:** Push-skip is currently inactive for multi-service deployments. Registry-push content cannot be verified via layer diff IDs, so every multi-service run rebuilds and re-pushes each service; a registry-digest pre-check to restore the optimisation is planned.

## postStart hooks

In an attached run, `wendy run` runs `openURL` and `cli` postStart actions after
the app reports readiness. This applies to both registry-push and chunk-diff
deploys. If readiness fails, Wendy skips these actions and prints a warning.

`--detach` returns after the selected containers start and does not run
readiness checks, `openURL`, or `cli`; `postStart.agent` still runs on the
device. See [Readiness and lifecycle hooks](../../../apps/wendy-services.md#readiness-and-lifecycle-hooks)
for multi-service details.
`--deploy` creates the app without starting it, so no postStart action runs.

Under attached `--watch` the host-side actions run after the first successful
readiness check only. `--watch --detach` skips them; see [Watch mode](#watch-mode).

> **Note:** When the CLI connects to the device at an IPv6 address (for example, one discovered via mDNS), the hook targets the device's best self-reported IP address instead — the same address shown in the `App reachable at` line — for both `openURL` and `cli`. This avoids pointing at a rotating RFC 4941 temporary privacy address that may not be reachable later. If the device cannot be queried, the dialed address is used (and bracketed for URL safety in `openURL`).

### `openURL`

`openURL` opens a URL in the developer's default browser without a shell. It works uniformly on macOS, Linux, and Windows and is the recommended way to open a URL at startup:

```json
{
  "hooks": {
    "postStart": {
      "openURL": "http://${WENDY_HOSTNAME}:3001"
    }
  }
}
```

When `${WENDY_HOSTNAME}` is substituted and the device address is an IPv6 literal, `wendy run` automatically brackets it (e.g. `2001:db8::1` → `[2001:db8::1]`) so the resulting URL is parseable by browsers. Zone IDs are percent-escaped per RFC 6874. IPv4-mapped IPv6 addresses (`::ffff:x.x.x.x`) are unmapped to plain IPv4. The `cli` hook receives the raw (unbracketed) address.

If the browser cannot be opened, a warning is printed and `wendy run` continues normally. `openURL` is fire-and-forget and does not affect the process tracked by `wendy run`.

### `cli`

`cli` runs a free-form shell command on the developer's machine. It is dispatched through the platform shell (`sh -c` on Unix, `cmd.exe /S /C` on Windows). `wendy run` tracks this child process for waiting and cancellation; the returned handle is used to clean up when `wendy run` exits.

`openURL` and `cli` can be set together — `openURL` fires first, then `cli` is spawned.

> **Note:** `open`, `xdg-open`, and `start` inside `cli` are platform-specific. Use `openURL` to open a URL portably. WendyOS warns at config load time when `hooks.postStart.cli` begins with one of these commands.

### Hook process lifetime

On **Windows**, the entire process tree spawned by a `cli` hook — including grandchildren started via `start /B` — is terminated when `wendy run` exits or is interrupted. If the primary mechanism is unavailable, `wendy run` falls back to `taskkill /T /F`, which terminates the direct child and its descendants as long as the parent process is still alive.

On **Unix**, the default shell process-group cleanup is sufficient; no additional termination logic is applied.

### Attached-mode hook lifetime

In a normal attached run, the `cli` hook process is tied to the run. When the
container exits or you press **Ctrl-C**, Wendy cancels the hook and waits for it
to exit before returning. In watch mode, the hook is tied to the watch session
and is canceled when you stop watching.

Detached mode (`--detach`), deploy-only mode (`--deploy`), and
`--watch --detach` do not fire the host-side hook at all, so there is no child
process to reap. Attached watch hooks are owned and reaped by the watch session.

## Container image signature

`wendy run` optionally includes a detached **ML-DSA65** signature with every `RunContainer` call. The agent verifies the signature over the SHA256 digest of the OCI image config before assembling or starting the container.

Set `WENDY_IMAGE_SIGNATURE_PATH` to the path of the detached signature file; when the variable is unset or points to an empty file, no signature is sent. Verification is currently dormant on the agent side (the per-org publisher key is not yet wired in), so omitting the signature does not block container creation today. Once the publisher key is provisioned, sending an unsigned or tampered image causes the agent to refuse the run.
