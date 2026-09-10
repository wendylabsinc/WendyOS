# `wendy device info`

Shows agent version, OS, architecture, GPU, and hardware info for the target device.

## Usage

```sh
wendy device info [flags]
```

## Description

`wendy device info` queries the connected device's agent and prints its version, operating system, CPU architecture, CPU core count, total RAM, GPU presence, and other hardware details. Use this command anywhere device metadata is needed — in scripts, CI pipelines, or interactively.

The output format follows the standard `--json` / human-readable convention shared across all device commands.

### CPU and memory output

Two headline hardware specs are included when the agent reports them. Both are omitted from the human-readable output and the JSON map when the agent cannot read them (older agents or non-Linux hosts), so treat them as optional.

| Field (JSON) | Human-readable label | Description |
|---|---|---|
| `cpuCount` | `CPU Cores:` | Number of online logical CPU cores. |
| `memTotalBytes` | `Memory:` | Total physical RAM in bytes; human-readable output shows a formatted size (e.g. `131.9 GB`). |

For **live** CPU and memory utilization, use [`wendy device top`](top.md).

### GPU output fields

On GPU-capable devices, the following GPU fields are included. Each is omitted from both the human-readable output and the JSON map when the agent does not report it (e.g. non-GPU devices or older agents), so consumers should treat every field as optional.

| Field (JSON) | Human-readable label | Description |
|---|---|---|
| `gpuVendor` | `GPU:` | GPU vendor (e.g. `nvidia`, `qualcomm`); shown as `unknown` in human-readable output when a GPU is present but the vendor is unreported. |
| `jetpackVersion` | `JetPack:` | JetPack/L4T version string (Jetson only). |
| `cudaVersion` | `CUDA:` | CUDA toolkit version (e.g. `12.6`). |
| `gpuArch` | `GPU Arch:` | GPU architecture identifier. Format is vendor-specific (e.g. `sm_87` for NVIDIA, `a623` for a Qualcomm Adreno). |
| `gpuCapabilities.computeBackends` | `GPU Compute:` | Host compute backends an app can use: `cuda`, `rocm`, `metal`, or `qnn` (the Qualcomm Hexagon NPU over FastRPC on Dragonwing). An empty list prints `none detected`; the object is absent on older agents. |

`wendy device info` reports static GPU *metadata* (vendor, architecture, toolkit versions). For **live** GPU utilization, memory, temperature, and power draw, use [`wendy device top`](top.md).

### Network output

The output includes the device's network interfaces and their routable IP addresses under a `Network:` block (human-readable) or a `networkInterfaces` array (JSON). This is the address to use to reach an app running on the device — the `.local` hostname does not always resolve on every network.

Loopback, down, and container/virtual bridge interfaces are omitted, as are link-local addresses. The section is absent entirely when the agent cannot enumerate interfaces (e.g. older agents).

| Field (JSON) | Human-readable label | Description |
|---|---|---|
| `networkInterfaces[].name` | (row label) | Interface name (e.g. `eth0`, `wlan0`). |
| `networkInterfaces[].ipAddresses` | (row value) | Routable IPv4/IPv6 addresses assigned to the interface. |

### OS Update output

On WendyOS devices that use the **wendyos-update** OTA engine, `wendy device info` prints a compact `OS Update:` block showing the live A/B slot state and the last recorded update outcome:

```
OS Update:
  Slot A: booted, rootfs normal, WendyOS 0.17.0
  Slot B: inactive, retries 2 (trial boot pending)
  Pending: wendyos-jetson 0.18.0 (installed, target slot B)
  Last update: committed (0.16.0 → 0.17.0)
```

The block is omitted entirely when:
- the device does not use the wendyos-update engine,
- the agent is too old to support the engine-status probe, or
- no update record exists and the live probe returns nothing.

When the last update outcome is not `committed`, a hint is printed:

```
  Details: wendy os update-status
```

Slot health is colour-coded: `normal` is green, anything else is red. Outcome words follow the same scheme: `committed` is green, `rolled back` is amber, and failure outcomes are red.

### JSON: `osUpdate` field

When OS-update information is available, `--json` includes an `osUpdate` object. It is omitted entirely when nothing is reportable.

```json
{
  "osUpdate": {
    "lastUpdate": {
      "outcome": "committed",
      "createdAtUnix": 1750000000,
      "oldOsVersion": "0.16.0",
      "newOsVersion": "0.17.0"
    },
    "engine": {
      "connector": "tegrauefi",
      "currentSlot": "A",
      "slots": [
        { "slot": "A", "booted": true, "partition": "/dev/nvme0n1p1", "distro": "WendyOS 0.17.0", "kernel": "5.15.148-tegra", "rootfsHealth": "normal", "retries": "", "note": "" },
        { "slot": "B", "booted": false, "partition": "/dev/nvme0n1p2", "retries": "2", "note": "trial boot pending" }
      ],
      "system": [
        { "key": "bootloader", "value": "36.3.0" }
      ],
      "pending": {
        "artifactName": "wendyos-jetson",
        "artifactVersion": "0.18.0",
        "phase": "installed",
        "targetSlot": "B"
      }
    }
  }
}
```

| Field | Description |
|---|---|
| `osUpdate.lastUpdate.outcome` | One of `committed`, `rolled back`, `rollback failed`, `commit failed`, `unknown`. |
| `osUpdate.lastUpdate.createdAtUnix` | Unix timestamp of the recorded update outcome. |
| `osUpdate.lastUpdate.oldOsVersion` / `newOsVersion` | OS versions before and after the update (omitted when not recorded). |
| `osUpdate.lastUpdate.note` | Failure reason from the updater, if any (omitted when empty). |
| `osUpdate.engine` | Live snapshot from `wendy os update-status --json`; omitted when not available. |
| `osUpdate.engine.connector` | Platform connector, e.g. `tegrauefi` or `ubootenv`. |
| `osUpdate.engine.currentSlot` | Slot the device is booted from (`A` or `B`). |
| `osUpdate.engine.slots[].rootfsHealth` | Bootloader-reported slot health, e.g. `normal`. |
| `osUpdate.engine.slots[].retries` | Remaining boot-trial retries (string; empty when not tracked). |
| `osUpdate.engine.system` | Connector-specific key/value pairs (e.g. bootloader version); JSON only, not shown in human-readable output. |
| `osUpdate.engine.pending` | In-flight update; omitted when none is pending. |
| `osUpdate.engine.pending.phase` | Engine phase, e.g. `installed` (awaiting reboot + commit). |

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--check-updates` | `false` | Check whether a newer agent release is available and include the result in the output. |
| `--prerelease` | `false` | Include pre-release (nightly) versions when checking for updates. |

## Examples

Print device info in human-readable form:

```sh
wendy device info --device my-device.local
```

Print device info as JSON (useful in scripts):

```sh
wendy device info --device my-device.local --json
```

Extract specific fields with `jq`:

```sh
wendy device info --device my-device.local --json | jq '{osVersion, agentVersion: .version, deviceType, cpuCount, memTotalBytes}'
```

## Deprecated alias

`wendy device version` is a deprecated alias for this command. It remains functional for backward compatibility but is hidden from help output. When invoked in non-JSON mode it prints a deprecation warning to stderr:

```
Warning: 'wendy device version' is deprecated; use 'wendy device info' instead.
```

No warning is emitted when `--json` is passed, so existing machine-readable scripts that use `wendy device version --json` continue to work without noise on stderr.

Migrate any usage of `wendy device version` to `wendy device info`.

## Related

- [`wendy os update-status`](../os/update.md) — full OS update record including service-level healthcheck details and the live engine snapshot
- [`wendy os update`](../os/update.md) — apply a WendyOS OTA update
