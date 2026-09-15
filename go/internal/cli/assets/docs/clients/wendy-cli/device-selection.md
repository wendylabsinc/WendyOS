Many CLI operations run on the remote device. When a connection is to be
established, the CLI goes down a list in order to connect to this device.

## 1. `--device`

If the `--device` flag is specified, a connection is made against that target IP
address or hostname. Include a port when the target should not use the default
agent port:

```sh
wendy --device 192.168.1.42 device apps list
wendy run --device my-mac.local:50051
```

Failing to connect to an explicit device results in a failure.

> **TODO (test)**: If the target device is outdated, and `--json` is not specified, a warning will be printed to indicate an update is available.

## 2. Default Device

If a Default Device is set using [`wendy device set-default`](./commands/device/set-default.md),
the CLI attempts to connect to it. The saved value may be a hostname, IP
address, provider key, or explicit `host:port` value.

```sh
wendy device set-default my-mac.local:50051
wendy device info --json
wendy run
```

Use [`wendy device get-default`](./commands/device/get-default.md) to check
the current default. If the default device cannot be reached from an
interactive terminal, the CLI shows the picker so you can select another
device. In non-interactive shells, the command fails with a clear message
that the default is set but unreachable instead of opening a picker.

> **TODO (test)**: If the target device is outdated, and `--json` is not specified, a warning will be printed to indicate an update is available.

## 3. Show Picker

mDNS and BLE discover nearby [WendyOS](../../wendyos/),
[Wendy-Agent](../../wendy-agent/) and [Wendy Lite](../../wendy-lite/) devices.
A device picker is shown only when the terminal is interactive, so a user can
select their target device for the current command invocation.

When logged into Wendy Cloud, press `e` on a highlighted Local device to run
the `wendy device enroll` flow using the active cloud account. Enrollment
requires a LAN or USB network connection. After enrollment, the picker reopens;
press Enter to select a device for the original command.

Headless Mac advertises over Bonjour/mDNS as `_wendyos._udp` and appears
as a LAN device when local discovery is allowed by the network and macOS Local
Network permissions.

The picker table shows the same columns as
[`wendy discover`](./commands/discover.md#interactive-table). The leading
marker column displays provisioned state (`●`/`○`) for LAN devices alongside
the `✦` default marker. When the highlighted device is provisioned but this
CLI cannot read its agent details (unprovisioned CLI, or logged in with
credentials that don't have access), a footer hint explains why the version is
blank and suggests `wendy auth login`.

In scripts, CI, SSH sessions without a TTY, or any other non-interactive
context, no picker is shown. Pass `--device`, or configure a default with
`wendy device set-default` (confirm it with `wendy device get-default`),
before running commands that need a target.

> **TODO (test)**: If the target device is outdated, and `--json` is not specified, a warning will be printed to indicate an update is available.
> If the terminal is interactive, a prompt will be made to update the device right now.

## Local Targets

The picker can also show local provider targets when the host supports them.
These are not WendyOS devices.

### Docker

Use Docker for local container runs. Dockerfile, Containerfile, and Compose projects run
through the local Docker daemon. On macOS and Windows, Docker runs Linux
containers inside Docker's Linux environment rather than as native macOS or
Windows processes.

Use this target when you want to test container build and runtime behavior
without deploying to WendyOS hardware. Hardware-specific WendyOS behavior, device
entitlements, and target filesystem assumptions still need a real WendyOS target.

You can select it directly with:

```sh
wendy run --device docker
```

### Apple Container

On Apple silicon Macs, Wendy can use Apple's `container` CLI for local
Dockerfile and Containerfile runs without Docker Desktop:

```sh
wendy run --device apple-container
```

Wendy automatically checks for the `container` CLI and offers to install it via Homebrew if missing, and starts the `system` and `builder` services if they are not running.

Use this target when you want to build and run a single Dockerfile or
Containerfile project with Apple's lightweight Linux container runtime. Compose
projects still require the Docker target.

To deploy to a WendyOS device while using Apple Container only as the image
builder, keep `--device` pointed at the WendyOS device. On Apple silicon Macs,
Apple Container is tried first by default when it is installed and running, then
Docker is used as a fallback. Set `--builder apple-container` to require Apple
Container, or `--builder docker` to force Docker:

```sh
wendy --device my-wendy.local run --builder apple-container
```

### Local

Use the local target for host-native apps. The app runs directly on the computer
that is running the `wendy` CLI:

- On macOS, it runs as a macOS process.
- On Windows, it runs as a Windows process.
- On Linux, it runs as a Linux process.

The local target is intended for native Swift, Go, and Python projects. It does not
run inside Docker's Linux environment, does not emulate WendyOS, and does not
provide WendyOS container semantics, hardware entitlements, or device filesystem
layout.

You can select it directly with:

```sh
wendy run --device local
```
