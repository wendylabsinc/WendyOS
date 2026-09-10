Builds your WendyOS app. Leverages `wendy.json` for metadata. If `wendy.json` is missing, it prompts you to generate one.

The build command is mainly used to verify your app can build/compile.

## Flags

| Flag | Default | Description |
|---|---|---|
| `--build-type` | auto-detected | Build type to use when multiple project markers are present: `docker`, `swift`, or `python`. |
| `--dockerfile` | auto-detected | Build file to build from: a `Dockerfile`, `Containerfile`, a dot/hyphen variant of either (`Dockerfile.prod`), or a Stagefile (`prod.stagefile.yaml`). A bare filename in the project directory, not a path. Shows a selection menu when multiple build files exist — see [Selecting a build file](#selecting-a-build-file). |
| `--builder` | auto | Image builder to force for Dockerfile/Containerfile builds: `docker`, `apple-container`, or `buildkit`. |
| `--gpu-arch` | from the device | GPU architecture a Stagefile `cuda:` stage targets; taken from the device when one is selected. |
| `--debug` | `false` | Build compiled languages unoptimized instead of the release default — see below. |
| `--service <name>` | all services | Build only the named service and its dependencies (multi-service projects) — see [Multi-service manifests](#multi-service-manifests). |
| `--max-concurrency <n>` | `0` | Multi-service: max service images to build at once (0 = default limit of 4). |

### `--debug`

`--debug` switches compiled-language stages from the release default to an unoptimized build:

- **Stagefile projects** — Swift stages emit `swift build -c debug` and Rust stages drop `--release`. It overrides the profile a stage declares, whichever Stagefile is being built — the canonical `build.stagefile.yaml` or a variant named with `--dockerfile`. Go and Node (`npm`/`yarn`/`pnpm`) stages have no optimization level to switch and are unaffected.
- **Compose projects** — applies to services built from a Stagefile.
- **Swift packages without a build file** — the host cross-compile runs `swift build -c debug`.

Hand-written Dockerfiles are not rewritten, and `wendy build` passes no build-args, so `--debug` has no effect on them here. To gate a hand-written Dockerfile on it, branch on the `WENDY_DEBUG` build-arg that [`wendy run`](run.md) passes (see the build-arg table below).

The same flag on [`wendy run`](run.md) and `wendy fleet run` additionally enables debug logging, and on `fleet run` host networking. On `wendy build` it only selects the build configuration.

## Manifest detection

`wendy build` scans the project directory for a build manifest in the following priority order:

1. `wendy.json` with a non-empty `services` map — multi-service build. Takes precedence unconditionally, even over a root-level Compose file, Dockerfile, or other marker below; each service resolves its own build file independently inside its `context` directory, using the same per-directory Stagefile-before-Dockerfile rule as the Stagefile entry below. See [Multi-service manifests](#multi-service-manifests).
2. `docker-compose.yml` / `compose.yml` — multi-service Compose project
3. `build.stagefile.yaml` / `<name>.stagefile.yaml` — Stagefile, compiled to a digest-pinned Dockerfile at build time. Checked ahead of plain Dockerfiles: a Stagefile is the more explicit build signal, and a project that has one usually keeps a generated Dockerfile beside it.
4. `Dockerfile` / `Containerfile` (or dot/hyphen variants) — container image build
5. `Package.swift` — Swift Package Manager project
6. `*.xcodeproj` — Xcode project (macOS targets only)
7. `requirements.txt` / `setup.py` / `pyproject.toml` — Python project (Dockerfile auto-generated)

If multiple manifests are present you can override detection with `--build-type`; a `services` map is the exception — it always wins, and `--build-type`/`--dockerfile` are rejected outright rather than used to pick something else (see [Multi-service manifests](#multi-service-manifests)).

A Stagefile is a YAML build descriptor that compiles to a real Dockerfile with guarantees a hand-written one does not get by default: base images are digest-pinned through a committed lockfile, each install step is emitted with the correct flags and a scoped cache mount, the `.dockerignore` is derived from the declared copy paths, and there is no raw-shell escape hatch. The compiled Dockerfile and its paired `.dockerignore` are written next to the source as build output — see [Stagefile naming](#stagefile-naming) for the artifact names. Most projects under [`Examples/`](https://github.com/wendylabsinc/wendyos/tree/main/Examples) use this format and are worth reading as reference.

## Multi-service manifests

A `wendy.json` with a non-empty `services` map builds one **local** image per service instead of a single image — the same manifest [`wendy run`](run.md) detects and orchestrates, but building only. Nothing is pushed to a registry and nothing is created or started; no device is required.

Each image is tagged `<lowercase-appId>-<lowercase-service>:latest`. For example, an app with `appId: "sh.wendy.examples.isolatedservices"` and services `api` and `worker` produces:

```
sh.wendy.examples.isolatedservices-api:latest
sh.wendy.examples.isolatedservices-worker:latest
```

Each service resolves its own build file independently inside its own `context` directory — Stagefile preferred over Dockerfile/Containerfile, the same rule [Manifest detection](#manifest-detection) uses per directory. Because there is no single build file or type to pick across services, `--dockerfile` and `--build-type` are not supported here and return an error.

Build only one service and its dependencies with `--service`:

```sh
wendy build --service api
```

`--service <name>` resolves `<name>` plus every service reachable through its `dependsOn` graph; services outside that subset are not built. An unknown service name returns an error immediately.

Up to 4 service images build in parallel by default. Override with `--max-concurrency <n>`; `0` restores the default limit of 4.

`--builder`, `--gpu-arch`, and `--debug` behave exactly as in a single-image build, applied per service — with one exception: `--builder buildkit` is not supported for multi-service builds, only `docker` and `apple-container`. As with single-image builds, `wendy build` passes no build-args, so a hand-written Dockerfile's `ARG WENDY_PLATFORM` / `ARG WENDY_DEBUG` take their declared defaults here.

With no device selected, the target platform defaults to `linux/arm64`; selecting a device uses that device's own platform instead, same as single-image builds.

A `services` map takes precedence over any root-level build marker present in the same project — see [Manifest detection](#manifest-detection).

## Selecting a build file

A project may hold several container build files at once — `Dockerfile`, `Dockerfile.prod`, `Containerfile`, `build.stagefile.yaml`, `gpu.stagefile.yaml`. When more than one is present and the shell is interactive, `wendy build` shows a picker listing all of them, Stagefiles first. In CI (or any non-interactive shell) there is no picker, so name the file explicitly:

```sh
wendy build --dockerfile gpu.stagefile.yaml
wendy build --dockerfile Dockerfile.prod
```

`--dockerfile` is accepted by `wendy run`, `wendy watch`, and `wendy fleet run` too, and takes a Stagefile on all of them. `--build-type` is a coarser control: it picks the *project type* (`docker`, `swift`, `python`), not the file — Stagefiles build through the `docker` type.

### Stagefile naming

A Stagefile is named `<variant>.stagefile.yaml`, where `<variant>` matches `[A-Za-z0-9][A-Za-z0-9_-]*` — letters, digits, underscores and hyphens, **no dots**. `build.stagefile.yaml` is the canonical name: it leads the picker and is the one chosen wherever a default is needed.

The no-dots rule is what keeps `build.stagefile.lock.yaml` out of the family — a lockfile is a build artifact, not a rival build file. It also means a name like `my.app.stagefile.yaml` is *not* recognised as a Stagefile and will not appear in the picker; use `my-app.stagefile.yaml` instead.

Each Stagefile keeps its own artifacts next to it, so two variants never overwrite each other's output:

| Source | Compiled Dockerfile | Lockfile |
|---|---|---|
| `build.stagefile.yaml` (canonical) | `Dockerfile.generated` | `build.stagefile.lock.yaml` |
| `prod.stagefile.yaml` | `Dockerfile.generated.prod` | `prod.stagefile.lock.yaml` |
| `gpu.stagefile.yaml` | `Dockerfile.generated.gpu` | `gpu.stagefile.lock.yaml` |

Everything in the `Dockerfile.generated*` namespace is a build artifact: it is excluded from manifest detection and from the `wendy watch` rebuild triggers, and it is worth adding to `.gitignore`. Lockfiles are the exception — commit them, they pin the digests your build resolved.

## Compatibility

| Manifest | Required host | Notes |
|---|---|---|
| `wendy.json` `services` map | Docker Desktop or Apple `container` on Apple silicon macOS | Local build only — no push, no device required; each service resolves and builds independently with `--builder docker` or `--builder apple-container` (`buildkit` is not supported here). See [Multi-service manifests](#multi-service-manifests) |
| `<name>.stagefile.yaml` | Same as `Dockerfile` | Compiled to `Dockerfile.generated[.<variant>]` and then built through the Dockerfile path, so every builder below applies unchanged |
| `Dockerfile` / `Containerfile` | Docker Desktop, Apple `container` on Apple silicon macOS, or WendyOS | Local Docker builds use `docker buildx`; `--device apple-container` uses `container build`; WendyOS device builds can select `--builder docker` or `--builder apple-container` |
| `Package.swift` | macOS or Linux | Requires a host Swift toolchain |
| `*.xcodeproj` | macOS only | Built with `xcodebuild`; `Brewfile.wendy` is the auto-detected target-agent Brewfile for native Mac runs |

## Build paths

### Dockerfile and Containerfile projects

`wendy build` invokes an image builder targeting the device's CPU architecture. [`wendy run`](run.md) passes the following build-args so the Dockerfile or Containerfile can adapt to the target hardware — declare them with `ARG` to use them. `wendy build` compiles the same image but passes no build-args, so a Dockerfile that branches on them takes its `ARG` defaults here:

On Apple silicon Macs with [Apple `container`](https://github.com/apple/container)
installed, Wendy tries Apple Container first for Dockerfile and
Containerfile builds when `--builder` is omitted. If Apple Container is
unavailable or the build fails, Wendy falls back to Docker. Use
`--builder docker` to force Docker, or `--builder apple-container` to require
Apple Container:

```sh
wendy --device my-wendy.local build
```

Wendy automatically checks for the `container` CLI and offers to install it via Homebrew if missing, and starts the `system` and `builder` services if they are not running.

If Apple Container reports an empty build context for a project under `/tmp` or
`/private/tmp`, Wendy returns an error with the known workaround: move the
project to a non-`/tmp` directory and retry.

For local-only Dockerfile or Containerfile builds on the Mac itself, select the
local provider with `--device apple-container`. Compose projects still require
Docker for local provider runs.

| Build-arg | Values | Notes |
|---|---|---|
| `WENDY_PLATFORM` | `nvidia-jetson` \| `generic` | Platform tier derived from the device type |
| `WENDY_DEBUG` | `true` \| `false` | Set when `--debug` is passed. [`wendy project optimize`](project/optimize.md) flags it when it's declared (`ARG`/`ENV`) but no `RUN` step branches on it — gate your optimization level on it so debug builds aren't shipped to release. |
| `WENDY_DEVICE_TYPE` | e.g. `jetson-agx-orin` | Raw device type; absent when unknown |
| `WENDY_HAS_GPU` | `true` \| `false` (hardware presence) | Absent on older agents |
| `WENDY_HAS_CUDA` | `true` \| `false` (host CUDA support) | Falls back only to an explicit NVIDIA vendor on older agents |
| `WENDY_GPU_VENDOR` | e.g. `nvidia`, `qualcomm` | Absent when no GPU is reported |
| `WENDY_JETPACK_VERSION` | e.g. `6.0` | Jetson only |
| `WENDY_JETPACK_MAJOR` | e.g. `6`, `7` | Jetson only; JetPack major for per-generation base-image selection |
| `WENDY_CUDA_VERSION` | e.g. `12.6` | Jetson only |
| `WENDY_GPU_ARCH` | e.g. `sm_87` | GPU architecture identifier; absent when no GPU is reported |

Example — selecting a base image by platform:

```dockerfile
ARG WENDY_PLATFORM=generic
FROM ${WENDY_PLATFORM}-base-image
```

### Swift Package Manager projects

- **macOS target**: builds locally with `swift build` and syncs the binary to the device.
- **Container target (WendyOS / Docker)**: uses [swift-container-plugin](https://github.com/apple/swift-container-plugin) to cross-compile the app for the target device architecture. The plugin takes a Swift base container image and appends your compiled executable and bundle resources. The cross-compile runs in **release** configuration by default; pass `--debug` to build in debug configuration instead.

Swift projects that build through a Stagefile (`build.stagefile.yaml`) instead follow the Stagefile path, where `--debug` overrides the stage's declared profile.

### Xcode projects

Builds with `xcodebuild`. Xcode project support exists for native Mac packages that cannot be built correctly with SwiftPM alone, for example packages that need Xcode-only resource or shader build steps.

Wendy passes `-skipMacroValidation` and `-skipPackagePluginValidation` so `xcodebuild` can run from a headless CLI/agent session. Xcode's macro/plugin prompts are an interactive consent layer on top of SwiftPM's build-time code and package-plugin sandbox model; headless Wendy builds treat invoking the build as consent, similar to CLI build tools. Only use Xcode projects with trusted, pinned package dependencies.

For native Mac runs, if a `Brewfile.wendy` is present in the project root, Wendy applies it on the target Mac before starting the app. A plain project-root `Brewfile` is left for developer-machine setup unless explicitly referenced by `wendy.json`.

## Build progress output

In an interactive terminal, image builds render a live list of Dockerfile steps. BuildKit runs steps concurrently, so several can advance at once; each running step gets a dim sub-line showing whatever progress its output exposes — a compiler's `[525/1027]` counter, `pip`/`apt`/`wget` download percentages, or the byte counter and transfer rate BuildKit reports for base-image pulls and build-context transfer. Steps that produce no recognised progress fall back to their last line of output. Finished steps collapse to a single line.

When stdout is not a TTY (CI, piped output), a plain-text renderer is used instead. It prints a heartbeat line for each running step every 15 seconds so a long silent step is distinguishable from a hung build.

Progress detail for Docker builds comes from `docker buildx --progress=rawjson`, which requires buildx 0.13 or newer. The CLI probes the installed buildx version once per run and falls back to plain progress on older versions; set [`WENDY_BUILD_PROGRESS`](../global-flags.md#environment-variables) to override the choice.

## Post-build optimization hint

After a **slow incremental build** — one that reused cached layers but still took longer than ~50 seconds — `wendy build` runs a quick static [`wendy project optimize`](project/optimize.md) scan and, if it finds fixable build-config issues, prints them and offers to apply the safe fixes:

```
This incremental build took 1m4s. A quick scan found 2 build-config issue(s):
…
Apply 2 safe fix(es) now? (takes effect on your next build) [Y/n]
```

The fixes take effect on your **next** build; the build that just completed is unchanged. This nudge is purely interactive and is a **no-op in CI / non-interactive shells**. A separate one-line tip pointing at `wendy project optimize` is throttled to at most once per day per project.

## Platform support for Swift projects

`wendy build` for Swift packages requires a host Swift toolchain and is supported on **macOS and Linux hosts only**. On Windows, `wendy build` returns an error for Swift projects. The recommended alternative is to provide a `Dockerfile` or `Containerfile` and use `wendy run`, which routes through the image build path on all platforms.

## Remote build host

Delegating a build to another device is a [`wendy run`](run.md#remote-build-host) feature, not a `wendy build` one, and `wendy build --build-host` returns an error saying so.

The reason is that the two commands mean different things by "build". `wendy run --build-host` has a target device: the build host builds the image and pushes it straight into *that* device's registry over the mesh. `wendy build` has no target — it leaves an image behind on the machine that built it — so a remote build would deposit the image on the build host and nowhere useful, which is worse than not offering the flag.

Use `wendy run --build-host <device>` instead. To make a device willing to accept builds in the first place, see [`wendy device build-host`](device/build-host.md).

CUDA-selecting Dockerfiles must use `WENDY_HAS_CUDA`. `WENDY_HAS_GPU`
reports hardware presence, including Broadcom and other GPUs without CUDA.
Device info exposes `gpuCapabilities.computeBackends` (`cuda`, `rocm`, `metal`, `qnn`);
an empty list means no supported backend was detected, while an absent capability
object means an older agent. `containerStorage` identifies the filesystem used by
containerd; the existing disk scalar fields continue to describe the root filesystem.
