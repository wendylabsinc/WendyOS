Runs your app on a Wendy-enabled device:

1. [Selects a device](../device-selection.md)
2. [Queries the platform and architecture](./device/version.md) of this device
3. Invokes a [build](./build.md) using the target triple, and injects a [debugger](../../../debugging/) if needed
4. Uploads the artifact(s) for Linux (via the container registry) or macOS
5. [Starts the app](./device/apps/start.md). Attached runs, and any run with `--wait-ready`, then wait for readiness and print the reachable URL; other detached agent deployments report configured HTTP endpoints as described in [Detached output](#detached-output).
6. [Attaches the logs](./device/logs.md) if needed (when `--detach` is not provided)


[Command syntax, examples, and generated flag reference](/docs/reference/cli/run)

## Flags

| Flag | Description |
|------|-------------|
| `--deploy` | Build and create the container but do not start it. |
| `--detach` | Start the container and return without streaming logs or opening the app URL. Waits for readiness only with `--wait-ready`. Agent deployments report configured HTTP endpoints; see [Detached output](#detached-output). |
| `--wait-ready` | Succeed only once the app is ready: its readiness probe passes or, without a probe this machine can reach, it stays running for 10 seconds (or a shorter `--readiness-timeout`). Fails if the app crashes or the probe times out. See [Waiting for readiness](#waiting-for-readiness---wait-ready). |
| `--readiness-timeout <duration>` | Override the readiness deadline, from `1s` to `1h` in whole seconds. With `--detach` it requires `--wait-ready`. |
| `--restart-unless-stopped` | Restart the container unless manually stopped. |
| `--restart-on-failure` | Restart the container on failure. |
| `--no-restart` | Do not restart the container on exit. |
| `--debug` | Enable debug logging and inject debug tooling via `WENDY_DEBUG=true`. For SwiftPM projects (both native macOS and cross-compiled Linux container targets), builds with `-c debug` instead of `-c release`. |
| `--yes` / `-y` | Accept all device-selection prompts automatically. |
| `--builder <name>` | Image builder for Dockerfile/Containerfile builds: `docker`, `apple-container`, or `buildkit`. Cannot be combined with `--build-host`. |
| `--stagefile-backend <name>` | Stagefile compiler backend: `dockerfile` (default) or experimental direct `llb`. Direct LLB requires Docker/BuildKit and cannot be combined with Apple Container or `--build-host`. |
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

## Exit status

An attached `wendy run` streams the app's logs until the app exits or you stop the run:

| How the run ends | Exit status | The app afterwards |
|------------------|-------------|--------------------|
| The app exits with code 0 | 0 | As its restart policy leaves it |
| The app crashes: non-zero exit, OOM kill, or crash loop | Non-zero. The error names the exit code and termination reason and points to `wendy device logs --app <app>`. | As its restart policy leaves it |
| The container fails to start | Non-zero, with the start error the device reported | Not started |
| Ctrl-C (SIGINT) | 0 | Stopped |
| SIGTERM (a CI timeout, `kill`, a process supervisor) | Non-zero: `wendy run was terminated; app <app> was stopped` | Stopped |

The table describes single-container runs on WendyOS devices. Multi-service, Compose and native Mac runs, and runs on local provider targets such as `--device docker` or `--device apple-container`, exit 0 on Ctrl-C and try to stop their apps, but can leave them running: their stop races the cancellation of the run and is skipped when the cancellation wins. On SIGTERM they exit non-zero and may leave their apps running.

When nothing changed since the last deploy and the app is already running, `wendy run` only follows its logs. An interrupted run then leaves the app running, and the SIGTERM error says so. When the app is stopped instead, `wendy run` starts the existing container, unless the device reports it from another app `version` than `wendy.json` sets (`latest` when it sets none): that container is another deployment's, so `wendy run` deploys the project instead.

An interrupt that comes after the app's output has ended, while `wendy run` checks how the app exited (see below) or waits for the verdict of `--wait-ready`, stops nothing: the app has already exited, and the device may be running another deployment's app by then. Ctrl-C still exits 0, and SIGTERM exits non-zero.

The crash check reads the exit that the device agent records when the app stops, and re-checks the app for up to 3 seconds before it reports a crash (see below; it prints `Checking how <app> exited...` on stderr as the re-check starts): the device keeps an app's last recorded exit even after its restart policy restarted it, so a record alone does not prove the app just crashed. A clean exit (exit code 0) is reported at once, also when the device lists the app as crash-looping: once its restart policy has restarted an app, the device keeps that restart count until the app is next deployed or started, and lists a clean exit that the policy will restart that way. If the restart policy has already restarted the app by the time of the check, and the restart it counted still shows after that re-check, the exit status is gone: `wendy run` prints a notice and exits 0. Agents that predate exit reporting record no exit: every exit that stays for that re-check reads as a clean stop, and one that the restart policy recovered from reads as described in the known limitations below.

When another deployment replaces the app (for example `wendy run --detach` from another terminal), the run usually exits 0 and prints `Application <app> was replaced by another deployment.` (a run that only follows the app prints a neutral notice instead; see below). To replace an app, the device kills it, removes its container, prepares the new image while no app is listed, and creates the new container, which it lists as stopped (or crash-looping) with no exit recorded until it starts it. The whole replace can take a fifth of a second. The device counts the kill as a restart, so it can briefly list the new app running with a restart counted before the start resets the count. It records the kill like a SIGKILL crash (exit code 137), but that record may come late, on the new container, or never, and the killed app can be listed with the exit it recorded before, such as a crash its restart policy recovered from:

- Before it reports a crash or a restart by the restart policy — and when it finds no app, or an app stopped or crash-looping with no exit recorded — the run re-checks the app for up to 3 seconds. It reports the replacement as soon as the device lists a new container (after listing no app, or with no exit recorded after an app that was running or had an exit recorded), a restart count lower than the last one listed, the app running with no restart by its restart policy counted (once the device has shown that it reports exits or restarts; see the known limitations below), or an app from another version. It reports the crash (or the restart) only if the device still shows the app restarted by its restart policy, or the app stopped (or cannot be read), after all 3 seconds. The device can list an app as stopped before it records the exit, so an exit recorded during the re-check is the one reported.
- When the 3 seconds end with no app listed, a run that started the app, whose output has ended, reports the replacement: the new image is probably still being prepared. An app still crash-looping with no exit recorded counts as a replacement, again once the device has shown that it reports exits or restarts. An app still stopped with no exit recorded does not, since that is also how the device lists an app stopped with `wendy device apps stop`: the run prints `Application <app> stopped.`.
- A record from another app `version` (as set in `wendy.json`) counts as a replacement at once.
- If a run that started the app finds, when its output ends, the new app already running from the same version, it prints `Application <app> stopped.` instead.
- A run that only follows an app it did not start (see above) checks the app every second; while a check finds the app running, the run keeps following it. When a check finds the app stopped, the run re-checks it as above, but reports a crash (a non-zero exit) only if, after the 3 seconds, the app is still stopped with its exit recorded and nothing indicated a replacement. Otherwise (the app running again at any restart count, no app listed, or any sign of a replacement) it prints `Application <app> stopped; it may have been replaced by another deployment or restarted by its restart policy.` and exits 0, leaving the app as it is. A clean exit (exit code 0) prints `Application <app> stopped.` as before.
- With `--wait-ready` and no `--detach`, the readiness check also ends as soon as the run's output does, and a replacement it finds is reported the same way: the run exits 0, leaves the new app running, and runs no postStart actions for it. It also counts as a replacement, once the output of a run that started the app has ended, the app running with no restart counted, on a device that has shown that it reports exits or restarts. A run that only follows the app ends its check the way it ends following above: a crash only when the app stays stopped with its exit recorded, otherwise the notice that it may have been replaced or restarted, and exit 0. An exit or a restart during the check, a clean exit included, is re-checked the same way before the check fails, and a failed check never stops an app the device does not list, or lists as stopped with no exit recorded.

> **Known limitations:** a run that only follows an app it did not start cannot tell a crash that the restart policy recovered from apart from a replacement by another deployment: the device keeps the exit status of earlier exits, and the restart count the run started from may already include restarts. Such a run therefore reports that end neutrally and exits 0.
>
> Devices whose agent reports neither how apps exit nor restart counts (WendyOS agents that predate exit reporting, and the Mac agent) list an app that crashed and was restarted by its restart policy the way they list a replacement: stopped with no exit recorded, then running with no restart counted, or crash-looping with no restart counted. Until the run has seen a record with an exit recorded or a restart counted, it cannot tell the two apart: a run that started the app prints `Application <app> stopped; the device did not report how it exited, so it may have crashed and been restarted by its restart policy, or another deployment may have replaced it.` and exits 0, and with `--wait-ready` the check fails as `not_ready` and does not stop the app. A current device reads the same way when the run sees no exit or restart recorded during a fast replacement.

When the image build fails, `wendy run` prints the build failure details, including the builder's own error and the path of the full build log, and the error line below them names the step that failed and the cause when the log shows them (the last informative line of the step's output, skipping generic closing lines such as make's `make: *** [...] Error 1` or pip's closing notes, or the error the builder reported), for example `build failed at [build 4/4] RUN go build -o /out/app .: ./main.go:6:14: undefined: foo`.

## Detached output

For an ordinary single-device deployment to a Wendy agent, `--json --detach`
emits one result on stdout after start is acknowledged (or the unchanged app is
already running). Build progress and pre-start application output go to stderr:

```sh
wendy --json --device vm:dev run --yes --detach
```

```json
{
  "status": "started",
  "app": "com.example.web",
  "device": "vm:dev",
  "readiness": "not_checked",
  "url": "http://127.0.0.1:18880",
  "endpoints": [
    { "app": "com.example.web", "url": "http://127.0.0.1:18880" }
  ]
}
```

`readiness: "not_checked"` means no health probe or host `postStart` action ran.
Verify the HTTP response separately. In text mode, URLs appear as
`App URL (<app>): <url>` notices. With `--wait-ready`, the run checks the app
and prints its outcome instead of this result; see
[Waiting for readiness](#waiting-for-readiness---wait-ready).

URLs come from [`http` entitlements](../../../apps/wendy.json.md#http) and
HTTP(S) `hooks.postStart.openURL` values that contain `WENDY_HOSTNAME`.
`endpoints` contains distinct reported URLs, each with its app or service
identifier; a service may have several URLs. `url` is the first entry
and is omitted when `endpoints` is empty, including when routing cannot be
determined. A TCP readiness probe alone does not declare an HTTP endpoint.

For user-networked VMs, URLs use the connected VM's live forwarding on host
loopback. See [VM HTTP verification](../../../installation/wendyos-virtual-machine.mdx#reaching-an-app-you-deployed).
Multi-service and Compose runs emit one group result after the selected services
start. A partial deployment returns non-zero without a whole-group success
result. Fleet runs, `--deploy`, watch mode, and local container providers do not
produce this result.

## Waiting for readiness: `--wait-ready`

`--wait-ready` makes `wendy run` report whether the app actually came up:

```sh
wendy run --detach --wait-ready
wendy --json run --detach --wait-ready --readiness-timeout 90s
```

After the device confirms the container started, `wendy run` does one of two checks:

- It probes the app's readiness port from your machine (`readiness.tcpSocket.port`, or the `http` entitlement's port) until the port accepts a connection. The deadline is `--readiness-timeout`, else `readiness.timeoutSeconds`, else 30 seconds.
- When the app declares no probe, the device is reached through Wendy Cloud without an active Wendy Mesh VPN and your machine cannot connect to the device's agent on its LAN address (`wendy run` checks once, for up to 1.5 seconds), or `wendy run` runs on the device itself over the agent socket (`WENDY_AGENT_SOCKET`), it checks that the app stays running for 10 seconds, or for `--readiness-timeout` when that is shorter.

The run fails with a non-zero exit when, during the check, the app exits (even with exit code 0), is restarted by its restart policy, or is no longer reported by the device; when the probe deadline passes; or when, without a probe, the app's state cannot be read at the end of the window.

With `--detach` in JSON mode (`--json`; on by default when stdin or stdout is not a terminal), stdout carries exactly one JSON object, failures included, unless the run is interrupted or its command line does not parse (see below). It takes the place of the `started` result that `--detach` alone prints (see [Detached output](#detached-output)): `started` means only that the device acknowledged the start, while this object reports the check. Its `status` is one of five values:

```json
{"status":"ready","app":"my-app","device":"192.168.1.207","readiness":"passed","url":"http://192.168.1.207:8080"}
{"status":"running","app":"my-app","device":"192.168.1.207","readiness":"not_checked"}
{"status":"crashed","app":"my-app","device":"192.168.1.207","readiness":"not_checked","exit_code":3,"termination_reason":"crashed","message":"app my-app stopped unexpectedly (exit code 3, termination reason \"crashed\"); see its logs with `wendy device logs --app my-app`"}
{"status":"not_ready","app":"my-app","device":"192.168.1.207","readiness":"failed","message":"app my-app did not pass its readiness probe within 30s"}
{"status":"failed","app":"my-app","device":"192.168.1.207","readiness":"not_checked","message":"build failed at [build 4/4] RUN go build -o /out/app .: ./main.go:6:14: undefined: foo"}
```

| `status` | When | `readiness` | Fields besides `status`, `readiness`, `app` and `device` | Exit status |
|----------|------|-------------|------------------|-------------|
| `ready` | The readiness probe passed: the app's port accepted a connection from this machine. | `passed` | `url`, when the device reports an address for the app | 0 |
| `running` | No probe could run from this machine (see the second check above), and the app stayed running without a restart for 10 seconds, or for a shorter `--readiness-timeout`. | `not_checked` | None | 0 |
| `crashed` | During the check the app exited (even with exit code 0), stopped, crash-looped, was restarted by its restart policy, or was no longer reported by the device. | `failed` when a probe was running, else `not_checked` | `message`; `exit_code` and `termination_reason` when the device recorded the exit | Non-zero |
| `not_ready` | The probe did not pass before its deadline, or, without a probe, the app's state could not be read at the end of the window. | `failed` with a probe, `not_checked` without | `message` | Non-zero |
| `failed` | The run failed before the check started (see below). | `not_checked` | `message`: the run's error | Non-zero |

`app` is the app ID from `wendy.json`. `device` is the address `wendy run` connected to: for a direct connection that is often an IP address rather than the `.local` name, and through Wendy Cloud it is the device's cloud name. On the device itself (`WENDY_AGENT_SOCKET`), `device` is omitted and readiness is never probed, so the outcome is `running`, `crashed`, `not_ready` or `failed`. A `failed` object also omits `app` and `device` until the run knows them, and it never carries `exit_code`, `termination_reason` or `url`.

`failed` covers every error `wendy run` itself reports before the check: a flag value or combination it rejects (for example `--readiness-timeout 1500ms`, `--env FOO`, or `--wait-ready` with `--watch`, `--hil` or `--deploy`), a project or target `--wait-ready` does not support (listed at the end of this section), an invalid `wendy.json`, device selection, the build, the push, and the container start. The run also prints the error on stderr. Errors that stop the command before `wendy run` starts, such as an unknown flag, a malformed value like `--readiness-timeout abc`, or a CLI configuration that cannot be loaded, print only the error on stderr and no object. A run without `--wait-ready` prints no object when it fails, even when it is rejected (for example `--detach --readiness-timeout 30s`); a successful one prints the `started` result described in [Detached output](#detached-output). Ctrl-C (exit 0) or SIGTERM (non-zero exit) prints no object at any point; during the wait, both leave the app running.

A detached check does not look for another deployment. If one replaces the app during the check, the outcome is whatever the polls see: `crashed` when a poll finds the old app stopped (the device records it like a SIGKILL crash, exit code 137) or the app missing or not yet started, or `ready` or `running` when the polls see only the new app running.

Without `--detach`, `--wait-ready` runs the same check while streaming logs. A failed check fails the run and stops the app, except when nothing changed since the last deploy and `wendy run` only follows the app that was already running: that run did not start the app, so it fails without stopping it, and it ends neutrally instead of failing when the app runs again or may have been replaced (see [Exit status](#exit-status)). If another deployment replaces the app during the check, the run reports the replacement and leaves the new app running; when the device does not report how the app exited, so the run cannot tell a replacement from a crash, the check fails as `not_ready` and does not stop the app (see [Exit status](#exit-status)). Host-side postStart actions run only after the check passes.

`--wait-ready` currently supports single-container image projects (Dockerfile, Containerfile, Stagefile, or Python) on WendyOS devices. It is rejected for multi-service, Compose and Xcode projects, native Mac apps, Swift packages built without a Dockerfile, local provider targets such as `--device docker`, `--build-host`, `--watch`, `--hil`, and `--deploy`.

## Reachable app URLs

In attached mode after the app starts, and in a detached run with `--wait-ready` once its check passes, `wendy run` prints an `App reachable at <url>` line when it can infer a browser URL from the app configuration:

```text
App reachable at http://192.168.123.222:3000
App reachable at http://[2001:db8::1]:3000
```

The CLI derives this URL from either:

- `hooks.postStart.openURL`, when the URL contains `WENDY_HOSTNAME`
- `readiness.tcpSocket.port`

The printed URL uses a routable IP address reported by the device instead of the `.local` hostname, which makes it easier to open from browsers that do not resolve mDNS names reliably. If neither an `openURL` hook nor a TCP readiness port is configured, or if the device cannot report an IP address, `wendy run` skips this line.

When the device is reached through Wendy Cloud and the Wendy Mesh VPN is active on your machine, the readiness check, the printed URL, and the `openURL` and `cli` postStart actions use the device's mesh hostname. Without an active mesh route, `wendy run` checks once, for up to 1.5 seconds, whether your machine can connect to the device's agent on its LAN address. If it can (you are on the device's network), those actions work as on a LAN connection. If it cannot, `wendy run` does not print, probe, or open that address: the host-side readiness check and the `openURL` and `cli` postStart actions are skipped with a notice.

When an attached run's readiness probe times out but the app is still running, `wendy run` keeps checking every 5 seconds for up to ten probe timeouts in total (5 minutes with the default 30-second timeout), then warns. `--readiness-timeout` replaces the probe timeout and ends the wait at that deadline.

> **Note:** An attached run prints the `App reachable at` line before its readiness probe finishes (with `--wait-ready`, only once the check passes). If the probe then fails (timeout or connection error), `wendy run` skips the `postStart` hook, including opening the browser, and prints a warning instead. This prevents opening a browser tab pointed at a container that has already exited.

> **Note:** When `wendy.json` is absent, `wendy run` resolves the target device before prompting to create one. If the target is Headless Mac and the detected project type is unsupported, the project/target mismatch error is returned immediately without opening the config creation prompt.

## ESP32 — native ESP-IDF projects

Regular ESP-IDF projects are the recommended app model for ESP32 targets. Wendy recognizes a project by its standard top-level `CMakeLists.txt`/`project.cmake` include or an `sdkconfig` file. Add a `wendy.json` with `"platform": "wendy-lite"`, then run:

```bash
wendy run
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

For builds deployed to a WendyOS device, `--builder buildkit` uses buildctl and
exports an OCI image for Wendy's existing deployment path. If you explicitly
start the optional Local Build Service from the Wendy menu-bar app, the CLI
discovers its private socket automatically. `WENDY_BUILDKIT_HOST`,
`BUILDKIT_HOST`, and buildctl's normal local-daemon default remain supported.
This service only solves and caches builds; Apple `container` continues to run
local Mac applications.

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
| `WENDY_HAS_GPU` | `true` \| `false` (hardware presence) | Absent on older agents |
| `WENDY_HAS_CUDA` | `true` \| `false` (host CUDA support) | Falls back only to an explicit NVIDIA vendor on older agents |
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

## Remote build host

`--build-host` delegates the image build to another WendyOS device:

```bash
wendy run --build-host spark-office
```

The build runs on that device, and it delivers the finished image to the target
device over the mesh — LAN-direct when possible, via the cloud broker otherwise.
It addresses the provisioned target by asset ID without resolving a hostname, so
delivery does not depend on the build host resolving `device-<id>.cloud.wendy.dev`.
The image never travels through your machine.

Delivery works the way `wendy run` deploys from your laptop: the build host asks
the device which layers and chunks it already holds and sends only the missing
bytes into its content store, so a rebuild that changed one layer transfers a
few chunks rather than the image. A link that drops mid-transfer is resumed —
chunks the device already staged are never re-sent — and a deploy you cancel
and re-run picks up where it stopped. A device whose agent predates chunked
delivery receives a registry push instead, and the build log says so.
`--chunking` governs this leg too; see [Deploy path: `--chunking`](#deploy-path---chunking).

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

> **Note:** With `--build-host`, the same modes govern the build host's delivery
> to the device. `auto` delivers by chunks and falls back to a registry push only
> for a device whose agent predates chunked delivery, saying so in the build
> log. `force` turns that fallback into a delivery failure, and is refused up
> front against a build host too old to honour it. `off` takes the registry
> route for every device, as build hosts delivered before chunked delivery
> existed.

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
readiness checks (unless `--wait-ready` is set), `openURL`, or `cli`;
`postStart.agent` still runs on the device. See [Readiness and lifecycle hooks](../../../apps/wendy-services.md#readiness-and-lifecycle-hooks)
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

`openURL` opens a browser only when `wendy run` runs in an interactive terminal without `--json`. In CI, from a coding agent, or with piped output, it prints the URL instead.

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

CUDA-selecting Dockerfiles must use `WENDY_HAS_CUDA`. `WENDY_HAS_GPU`
reports hardware presence, including Broadcom and other GPUs without CUDA.
Device info exposes `gpuCapabilities`, one entry per detected GPU with its
`vendor`, `path`, and `computeBackends` (`cuda`, `rocm`, `metal`). A GPU whose
backend list is empty has no supported backend; no entries at all on a device
that reports a GPU means an older agent. An NPU runtime such as `qnn` is
reported separately, in `npuBackends`. `containerStorage` identifies the filesystem used by
containerd; the existing disk scalar fields continue to describe the root filesystem.

Attached runs keep observing slow startup after the initial readiness budget.
If the relevant service is still running when its probe timeout passes, the CLI
reports “still starting” and checks every five seconds while streaming logs,
for up to ten probe timeouts in total, then warns. `--readiness-timeout` is the
whole deadline instead: no extended observation follows it. Browser opening and
host `postStart` commands run once readiness succeeds. Stopping the service,
canceling the session, or replacing a watch deployment cancels its probes. With
`--wait-ready`, its own check replaces this observation. Detached and
create-only runs skip host `postStart` commands; a detached run checks
readiness only with `--wait-ready`.


## Native commands on Mac

A single native Darwin app can name a target executable directly:

```json
{
  "appId": "sh.example.chat",
  "platform": "darwin",
  "files": [{"path": "launcher.py"}, {"path": "data"}],
  "run": {"command": "/usr/bin/python3", "args": ["../launcher.py"], "cwd": "data"},
  "env": {"MAX_MODEL": "HuggingFaceTB/SmolLM2-135M-Instruct"}
}
```

`run.command` is an absolute executable on the target or an executable relative
 to the synced app directory. Relative scripts must have executable permission.
`run.cwd` defaults to the app directory and must stay inside it, including after
symlink resolution. Arguments are passed directly; shell expressions are literal
unless you explicitly choose a shell executable. The CLI selects this mode before
project detection and rejects conflicting build flags and non-Darwin targets.

The target must advertise `native-process`; update Wendy Agent for Mac if the
CLI requests it. Declared files, `wendy.json`, an optional `sandbox.sb`, and the
configured Brewfile (or auto-detected `Brewfile.wendy`) use native file sync.
Homebrew installation completes before the agent validates the executable.
Request environment values apply to native command, SwiftPM, and Xcode launches;
agent identity and telemetry settings take precedence. `WENDY_APP_ID` identifies
the app. Keep runtime data outside the synced source directory, for example under
`~/Library/Application Support/<appId>/runtime/`, to retain it across file sync.

Attached watch compares the command, working directory, arguments, effective
environment, files, configuration, and restart policy. Unchanged running commands
remain running. Host hooks and browser opening wait for readiness.


## Diagnosing an interrupted log stream

Quiet log subscriptions receive an empty transport heartbeat every 15 seconds.
The CLI and MCP omit these messages from logs, history transitions, and batch
counts. Go agent gRPC keepalive acknowledgements allow 20 seconds. A stream that
ends with a connection error still reports that error.

For a recurrence, record the CLI, agent, and OS versions, timestamps, device
address, direct WiFi or cloud route, and the exact gRPC error. Capture agent and
cloud tunnel logs for the same interval; compare an idle subscription with one
that emits a new log after ten minutes. Check WiFi roaming, link loss, NAT/proxy
idle limits, and HTTP/2 GOAWAY/keepalive diagnostics before assigning a cause.
The original direct-WiFi failure has no confirmed transport root cause.

## Cloud catalog registration

Before deploying to an enrolled device, `wendy run` registers each distinct
`appId` in Cloud Apps using an operator session matching the device's Cloud
endpoint and organization. Existing entries retain their metadata and grants.
Multi-service and Compose deployments register shared app IDs only once.
Registration does not enable notification permission; an owner or admin must
grant it in Cloud app settings.

Registration errors stop deployment. Use `--skip-cloud-registration` for an
offline deployment, then deploy again without it to register later. Unenrolled
devices skip registration automatically.
