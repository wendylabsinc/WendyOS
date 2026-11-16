Updates the wendy-agent installation on the remote device, then checks for a newer WendyOS image. By default downloads the latest release binary from GitHub matching the device's CPU architecture. Pass `--binary <path>` to upload a locally built binary instead (e.g. a cross-compiled development build). When an upload is performed, the command waits for the restarted agent to become reachable, [confirms what the device is actually running](#verification), then checks the OS update outcome. The command exits non-zero if the update was rolled back.

If the device responds with "an update is already in progress" (a `FailedPrecondition` error), a previous upload likely committed without the agent restarting — a bug fixed in this release. Reboot the device to clear the stale lock, then retry.

On the auto-download path, if the device already runs the resolved release version, the upload and agent restart are skipped and the command reports that it is already up to date. The OS update step below still runs afterward — except in `--json` mode, where the command returns immediately and the OS update step is not run. Because `--json` skips the OS step, combining it with `--pr` is refused outright rather than silently ignoring the requested PR image.

GitHub release lookups use the `GITHUB_TOKEN` environment variable for authentication when it is present, and fall back to unauthenticated requests otherwise.

## Verification

A successful reconnect only proves the device is reachable — a silent no-op, a rollback, or an arch-mismatched binary that never starts would answer just as well. So after **every** agent upload, confirmed or not, `wendy device update` queries the restarted agent and checks what the device actually runs before reporting success.

If the connection drops while the agent binary is being uploaded (the agent restarts the moment the binary lands, which can close the stream before the confirmation arrives), the command treats the outcome as **unconfirmed** rather than an error: it prints an informational message, reconnects, and lets the check below decide the outcome.

The check has two levels:

1. **Binary SHA-256 — definitive.** When the agent reports the SHA-256 of the executable it was started from, the CLI compares it against the hash of the binary it uploaded. A match is proof the upload landed; a mismatch exits non-zero and asks you to re-run `wendy device update`. This is the only way to prove a dev-over-dev `--binary` push landed, since dev builds share identical version strings. The comparison is skipped for macOS targets — the upload payload there is an app-bundle ZIP, whose hash can never equal the running executable's — so those fall through to the version check.
2. **Version comparison — fallback.** When no hash is available (the macOS agent, agents from releases predating the field), the CLI compares version strings. On the auto-download path the device must report the resolved release version or newer. A device that still reports a **dev**-build version after a **release** update fails verification: dev builds sort as newer than any release, so without this guard a silent no-op would pass vacuously. When the expected version is itself dev-stamped (a `workflow_dispatch` publish can stamp a `-dev` suffix into the manifest), a dev report is the correct outcome and is accepted. The `--binary` path has no release version to compare against, so with no hash available any reachable agent is accepted.

The first reconnect can land on the still-alive *old* agent — it exits shortly after committing the update, and takes longer to do so on macOS — whose answer would mis-fail a good update. A failing verdict is therefore re-polled within the same restart budget used to wait for the agent to come back, and only becomes final when that window expires.

On success the reported version is included in the message:

```
Agent updated successfully (agent reports 2026.07.01-223311).
```

## JSON output (`--json`)

With `--json`, a successful agent update emits a single JSON object and the command returns without running the OS update step:

```json
{
  "status": "success",
  "message": "Agent updated successfully.",
  "version": "2026.07.01-223311"
}
```

`version` is the agent version the device reported during verification. It is empty only if the agent reports no version.

When the device already runs the resolved release version, no upload happens and the object is instead:

```json
{
  "status": "up-to-date",
  "message": "Agent is already up to date."
}
```

## OS update step

After the agent is updated, the command checks for an OS update on WendyOS devices that advertise the in-house **wendyos-update** OTA engine. When a newer image is available it prompts before applying (default no); use `--yes` to apply without prompting, and `--nightly` to track the nightly channel for both the agent and the OS. Non-interactive runs report the available update without applying it. Devices without an OTA backend, and non-WendyOS hosts, skip this step silently — `device update` still succeeds as an agent-only update.

If the available artifact uses the wendyos-update stack (`.wendy` format) but the device image predates that stack, the auto-detected OS update is not offered — the command prints an explanation instead of prompting, and the agent-only update still counts as a success. This check runs only when an update is actually available; an already-current device is not warned.

An explicit `--artifact-url` pointing to a `.wendy` artifact on an incompatible device is **not** silently skipped: `device update` exits non-zero with the same reflash explanation.

## Update the OS to a pull-request build

```sh
wendy device update --pr 123 --yes
```

`--pr N` makes the OS update step install the WendyOS image built by
wendyos-builder PR #N instead of the manifest's latest. The agent-binary step is
unaffected: `--nightly` still selects the agent channel, and `--binary` still
uploads your build and re-applies it after the OS reboot.

PR images are **debug builds**: SSH is enabled, root login is passwordless, and
the serial console is active. They are for testing the PR on hardware — **never
install a PR image on a production device.** Artifacts are deleted when the PR
is closed.

`--pr` is supported for Linux disk-image devices (Raspberry Pi, Jetson Orin
Nano, Jetson AGX Orin) with OTA support; it is not supported for Jetson AGX
Thor or ESP32 targets. It is mutually exclusive with `--artifact-url` and
`--json`.

A `--pr` run never skips as "already current": a PR's version tag (`pr-N`) is
constant across rebuilds, so re-running after pushing a new commit to the same
PR always re-flashes. And unlike the default OS step — which degrades to an
agent-only success when the OS check fails — a `--pr` run must install that
PR's build or exit non-zero.

`--pr` works over the cloud tunnel (`wendy cloud device update --pr N`): the
resolved artifact is a public URL the device downloads directly, with only the
control stream tunneled. As with any cloud-tunneled OS update, the command does
not wait for the reboot (see [Post-update outcome](#post-update-outcome)).

## Pre-0.17.0 devices require a reflash

WendyOS 0.17.0 introduces a new OTA update system that is not backward-compatible with older images. When the device reports a WendyOS version older than 0.17.0, the OS step is refused and the command exits non-zero with:

```
this device runs WendyOS ‹version›. WendyOS 0.17.0 introduces a new update system
with no backward compatibility, so it cannot be updated over the air.
Reflash it with `wendy os install` to continue receiving updates.
```

The agent-binary update (including `--binary`) still runs and lands successfully; only the OS OTA step is blocked. To resume receiving OTA updates, reflash the device to WendyOS 0.17.0 or later with [`wendy os install`](../os/install.md). Dev builds and devices whose OS version cannot be parsed are allowed through.

## Post-update outcome

After the device is back online, `wendy device update` queries the post-reboot commit/rollback verdict from the device (the same status record that `wendy os update` and `wendy os update-status` report). If the update was rolled back, the command prints the rollback reason and exits non-zero:

```
Update failed post-reboot healthchecks and was rolled back to WendyOS-0.10.4.
Reason: wendyos-update commit failed: exit status 4 (health hook "50-containerd.sh" failed: exit status 1)
```

The first line distinguishes a healthcheck failure from an update that never
booted (the firmware fell back to the old slot, so `health.d` never ran) and from
a rollback whose reason the CLI cannot classify. See
[`wendy os update`](../os/update.md) for all three forms.

On cloud-tunneled (asset) devices the command does not wait for the reboot and does not query the outcome; it prints instructions to reconnect and re-run if needed.

## `--binary` survives the OS update

An OS update reboots into a new image that ships its own bundled agent, which would otherwise replace a `--binary` build. When `--binary` was provided and an OS update is applied, the command re-uploads the same binary after the device comes back online, so the development agent you asked for is what ends up running. (The auto-download path is not re-applied, to avoid downgrading the new image's bundled agent.) On cloud-tunneled devices the command does not wait for the reboot, so it prints instructions to re-run `device update --binary` once the device is back online.

The re-apply is verified the same way as the initial upload — by hash, which is what catches the failure this step exists for (the image's bundled agent still running instead of your binary):

- If the restarted agent reports a binary SHA-256 matching the re-uploaded binary, the command confirms the dev agent survived the OS update, including the version it reports.
- If the agent reports no hash, nothing was proven: the command prints an informational message saying the re-apply could not be verified and that the running agent may still be the OS image's bundled one. This is not treated as a failure.

## Artifact signature

`wendy device update` passes a detached **ML-DSA65** signature alongside the binary. The agent verifies the signature over the SHA256 digest of the binary before installing it.

By default no signature is sent (the verification key is not yet embedded in production builds, so the check is a fail-safe no-op and the install proceeds as before). When a signing pipeline is deployed, set `WENDY_AGENT_SIGNATURE_PATH` to the path of the detached signature file and `wendy device update` will include it automatically.

Once a real key is embedded in the agent, sending an unsigned binary — or a binary whose signature does not match — causes the agent to reject the install and return an error.

> **TODO**: On ubuntu machines, this should use `apt upgrade wendy-agent`
