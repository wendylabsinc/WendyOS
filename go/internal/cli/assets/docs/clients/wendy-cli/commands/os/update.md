# `wendy os update`

Updates the OS on a WendyOS device using an A/B OTA mechanism, driven by the
device's in-house **wendyos-update** engine.

```sh
# Auto-detect the latest stable release for the connected device
wendy os update

# Use the latest nightly build
wendy os update --nightly

# OTA update to a pull-request build
wendy os update --pr 123

# Provide a specific artifact URL
wendy os update --artifact-url https://example.com/update.wendy

# Provide a local artifact file
wendy os update ./update.wendy
```

---

## Supported targets

`wendy os update` only works with WendyOS devices that have OTA support. The command validates the target before doing anything else, including before updating the agent binary.

A target is treated as a WendyOS OTA target when the agent reports an `os_version` beginning with `WendyOS-`, or a non-empty `device_type`. This check does not depend on the reported `os` field, which is the `/etc/os-release` ID (e.g. `"ubuntu"`, `"wendyos"`) rather than a fixed value.

Hosts that are not WendyOS OTA targets — including macOS, Windows, unknown platforms, Wendy Lite / BLE-only targets, external/local-provider targets, and generic Linux hosts with `wendy-agent` installed but no WendyOS identity — are rejected immediately with an actionable error message. A WendyOS target must also advertise the `wendyos-update` featureset flag; without it, the update is rejected with a dedicated error.

| Circumstance | Error |
|---|---|
| macOS, Windows, unknown non-WendyOS platform, Wendy Lite, external/local provider | `This setup cannot be updated with wendy os update. Use this machine’s normal OS update tools instead. To use WendyOS OTA updates, install WendyOS on supported hardware with wendy os install.` |
| Generic Linux host with `wendy-agent` installed but no WendyOS identity | `This Linux host has wendy-agent installed, but it cannot be updated with WendyOS OTA artifacts. Use the Linux distribution’s package manager, such as apt, dnf, or pacman, to update this machine.` |
| WendyOS identity present but the device does not advertise the `wendyos-update` featureset | `This WendyOS image does not support OTA updates because the wendyos-update engine was not found on the device. Reinstall or upgrade to a WendyOS image with OTA support.` |
| WendyOS device running a version older than 0.17.0 | `this device runs WendyOS ‹version›. WendyOS 0.17.0 introduces a new update system with no backward compatibility, so it cannot be updated over the air.\nReflash it with \`wendy os install\` to continue receiving updates.` |
| No explicit artifact and the device type is missing or unrecognized in the update catalog | Shows a warning and prompts the user to select the correct device type from a picker. The latest version (stable or nightly) is then chosen automatically. |

> **Note:** macOS agents report a host OS version, but this does not qualify the host as a WendyOS OTA target. Only a `WendyOS-`-prefixed `os_version` or a non-empty `device_type` qualifies.

---

## Update sequence

1. **Validate target identity** — query the agent and confirm the target is a WendyOS OTA target. Exits immediately with an error if not.
2. **Check minimum OS version** — if the device reports a WendyOS version older than 0.17.0, exit non-zero with guidance to reflash using `wendy os install`. Dev builds, empty, and unparseable versions are allowed through. This check runs before the agent is updated, so no agent update is wasted on a device that must be reflashed.
3. **Update the agent** — ensure the agent binary is at the latest release before proceeding with the OS image update. GitHub release lookups use the `GITHUB_TOKEN` environment variable when present, and fall back to unauthenticated requests otherwise.
4. **Re-query version** — query the agent's version again after the agent update.
5. **Validate OTA support** — confirm the device advertises the `wendyos-update` featureset.
6. **Resolve artifact** — if no artifact or URL was provided, look up the latest OTA artifact for the device's reported `device_type`. If the device type is missing or not recognized, shows a warning and prompts the user to select the correct device type.
7. **Check current version** — if the device is already at the latest version, exits without updating.
8. **Stack-mismatch check** — if the resolved artifact is a `.wendy` file and the device does not advertise the `wendyos-update` featureset, exit non-zero with an explanation that a reflash is required. Skipped only when the device was already reported current in step 7.
9. **Stream update** — instruct the agent to apply the update, which runs `wendyos-update install` and streams progress to the terminal. The agent then reboots into the updated OS.
10. **Wait for reboot** — poll the device until it is reachable again (up to 10 minutes, enough for a rollback's second reboot).
11. **Report the outcome** — query the device for the post-update commit/rollback verdict and print it. The command exits non-zero when the update was rolled back.

---

## Jetson A/B preparation

Before starting `wendyos-update install`, the agent checks Jetson rootfs A/B
configuration and the alternate `APP_b` partition. It repairs missing or disabled
persistent EFI settings, preserves valid retry counts, and verifies each write
by reading the variable back. This preparation applies to both agent API versions.

If firmware rejects a write, the agent stops before downloading or staging the
OS update. It also refuses to change a disabled volatile setting: that change
would be lost at reboot. Such devices need an A/B-enabled firmware configuration
or image. Preparation does not repair an incompatible bootloader capsule.

## How the agent reboots

The agent always flushes filesystems before restarting. Without that, an
immediate kernel restart can discard recently written data — and on Jetson the
data at risk is the UEFI capsule that `wendyos-update` staged onto the ESP.

On WendyOS images older than **0.18.1** the agent additionally hands the reboot
to systemd (`systemctl --no-block reboot`) so filesystems are unmounted, not just
flushed. Those images ship a `wendyos-update` that fsyncs only the capsule file
and not the ESP, and because the capsule path deliberately leaves the rootfs slot
switch to the firmware, a lost capsule means the update silently reboots back into
the old OS and rolls back (WDY-2200). The orderly shutdown is the combination
validated on hardware for those releases.

An orderly shutdown can hang, so it is bounded: if the device is still running a
minute after the request, the agent forces an immediate restart. Expect an update
on a pre-0.18.1 image to take slightly longer to go down than on a current one.

Devices on 0.18.1 or newer, dev builds, and images whose version cannot be parsed
all take the plain flush-and-restart path.

## Post-update commit and automatic rollback

wendyos-update uses A/B rootfs slots, so an update boots into the new slot while keeping the previous OS intact. On the first boot after an update, the agent's gate runs `wendyos-update commit`. The health verdict is entirely delegated to that command: `commit` runs its own health checks internally (`/etc/wendyos-update/health.d`) before deciding whether the update is accepted, and the agent's gate just acts on the result — it does not run any healthchecks of its own.

- If `commit` accepts the update, it becomes permanent.
- If `commit` rejects the update (its `health.d` checks failed, or the deployment is otherwise marked failed), the agent runs `wendyos-update rollback` and reboots back into the previous OS slot.
- If the `wendyos-update` binary is missing at commit time, or the commit call times out, no health verdict was rendered — the agent keeps the pending-update marker and retries on the next agent start rather than rolling back a possibly-healthy slot. This retry window is bounded to one hour, after which the marker is discarded and treated as a plain commit.

The verdict — including any failure reason reported by `wendyos-update commit` — is persisted on the device's data partition, so it survives a rollback. `wendy os update` reports it once the device is back online:

```
Update failed post-reboot healthchecks and was rolled back to WendyOS-0.10.4.
Reason: wendyos-update commit failed: exit status 4 (health hook "50-containerd.sh" failed: exit status 1)
```

*(the text in parentheses is whatever `wendyos-update commit` itself reported as the failure reason)*

A rollback is not always a healthcheck failure, and the first line says which it
was. When the new OS never booted — the firmware fell back to the old slot, so
`health.d` never ran at all — the report names that instead:

```
The new OS did not boot; the device fell back to WendyOS-0.10.4 and the update was rolled back.
Reason: wendyos-update commit failed: exit status 1 (pending update wendyos-image-... is marked failed; run rollback)
```

If the reason is one the CLI cannot classify, it reports the rollback and the
captured reason without attributing a cause:

```
Update was rolled back to WendyOS-0.10.4.
Reason: wendyos-update commit failed: exit status 4 (platform verify: ESRT status 6163)
```

`wendy os update-status` reports the same record (including the `Reason:` line) after the fact, without re-running the update — useful for diagnosing a commit failure without shell access to the device. In addition to the persisted record, `wendy os update-status` queries the live wendyos-update engine snapshot, which distinguishes a committed nightly from a rolled-back one even when the OS version string is identical across builds.

`wendy os update-status` always requests the live `wendyos-update` engine snapshot in addition to the persisted commit/rollback record. When the agent returns it, the command prints a second block:

```
Engine status (wendyos-update):
  Connector:    tegrauefi
  Booted slot:  A
* slot A health=good distro=WendyOS-x.y.z
  slot B health=trial distro=WendyOS-x.y.z retries=0
  Diagnostics (raw):
    RootfsStatusSlotA = 0x01
    RootfsStatusSlotB = 0x00
```

The booted slot is marked with `*`. Diagnostic keys and values are connector-specific (e.g. tegra `RootfsStatusSlot{A,B}` bytes, EFI boot-chain/capsule variables, or uboot env entries) and may vary between firmware versions.

Pass `--json` to emit the complete status as indented JSON (useful in scripts or when capturing diagnostics off a device without shell access):

```sh
wendy os update-status --json
```

> **Note:** for older target images whose agent does not report an update result, the CLI falls back to comparing the OS version before and after the reboot, and warns when the device appears to have rolled back.

---

## Artifact auto-selection

When no artifact path or `--artifact-url` is given, the CLI uses the device's `device_type` field to look up the latest OTA artifact from the WendyOS release manifest. A device that reports `nvme` as its storage medium (see [`wendy device info`](../device/info.md)) gets the NVMe artifact when the release publishes one; every other device gets the default artifact. If the device type is missing or not recognized in the update catalog, the CLI shows a warning and falls back to an interactive device-type picker. The latest version (stable or nightly) is then chosen automatically.

Use `--nightly` to select nightly (pre-release) artifacts instead of stable ones.

Cloud-connected devices download the artifact directly from its URL. Ethernet
devices with a routable address and NAT-connected simulators also use the URL;
disconnected WiFi alone does not trigger a download on your computer. For an
isolated local connection with no reported routable address or active WiFi,
the CLI downloads and serves the artifact locally after checking the local route.

If a bare device name such as `--device ci-mesh-2` does not resolve locally,
the CLI looks for that exact device in your Cloud account. Explicit IP addresses,
hostnames with a domain, and host:port endpoints retain their direct connection.

Local artifact files require a direct LAN or USB connection. With a cloud tunnel,
use an artifact URL the device can reach. Reboot and update-result checks retain
the selected cloud device's identity instead of resolving its display name as a
LAN hostname.

After the artifact is resolved, the CLI checks its format against the device's advertised backend featureset. A `.wendy` artifact requires the `wendyos-update` engine; if the device does not advertise it, the update errors out with a reflash explanation rather than being applied.

---

## Update to a pull-request build

```sh
wendy os update --pr 123
```

OTA-updates the connected device to the WendyOS image built by wendyos-builder
PR #123. PR images are **debug builds**: SSH is enabled, root login is
passwordless, and the serial console is active. They are for testing the PR on
hardware — **never install a PR image on a production device.** Artifacts are
deleted when the PR is closed.

`--pr` is supported for Linux disk-image devices (Raspberry Pi, Jetson Orin
Nano, Jetson AGX Orin) with OTA support. It is not supported for Jetson AGX
Thor or ESP32 targets. `--pr` is mutually exclusive with `--nightly`,
`--artifact-url`, and a positional artifact file path.

---

## Flags reference

| Flag | Default | Description |
|------|---------|-------------|
| `--pr` | — | Update to wendyos-builder PR #N (mutually exclusive with `--nightly`, `--artifact-url`, positional path; Linux disk-image devices only) |
| `--artifact-url` | — | URL of an artifact (`.wendy`) to install directly. The CLI checks the artifact's format against the device's advertised backend before applying it; a `.wendy` URL on a device that predates the wendyos-update stack is refused with a reflash explanation. |
| `--nightly` | false | Use nightly/pre-release builds for auto-selection |
| `--drivers-dir` | — | Stage driver add-ons for the target kernel from local `.raw` files in this directory instead of the registry, for networks with no internet access. |
| `--no-drivers` | false | Update even though driver add-ons will not be staged for the target kernel; skips both the staging and its confirmation. |

A positional argument (a local `.wendy` file path, or a directory containing one) can be used instead of `--artifact-url`.

> **Tip:** After an update, `wendy device info` shows the last recorded update outcome. For live A/B slot state, pending updates, healthcheck results, and engine diagnostics, run `wendy os update-status`.
