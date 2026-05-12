# Plan: Friendly errors for unsupported `wendy os update` targets

## Problem

Running `wendy os update` against an Ubuntu/Linux host with `wendy-agent` installed can currently proceed far enough to:

1. update the agent,
2. fetch/select a WendyOS OTA artifact,
3. download and serve a multi-GB Mender artifact,
4. call `UpdateOS`,
5. fail only when `mender install` rejects the artifact.

That device is not WendyOS, so the CLI and agent should refuse the update early with a clear message.

## Unsupported combinations to catch

| Device state | Artifact input | Desired behavior |
| --- | --- | --- |
| No reachable gRPC agent / BLE-only / Wendy Lite / external provider | any | Friendly “requires a WendyOS LAN device” error |
| macOS / Windows agent | any | Block before update: OS updates only support WendyOS |
| Generic Linux, e.g. Ubuntu, no WendyOS markers | none / URL / local | Block before agent/OS artifact work |
| Generic Linux with `mender-update` installed | none / URL / local | Block before agent/OS artifact work |
| WendyOS marker present, but no `mender-update` | any | Friendly “this WendyOS image does not support OTA updates / is missing Mender” |
| WendyOS + mender, but missing `device_type` | no artifact | Block auto-update: cannot safely choose artifact |
| WendyOS + mender, unknown `device_type` not in manifest | no artifact | Block auto-update: no OTA artifact for this device type |
| WendyOS + mender, recognized `device_type`, no OTA for latest/nightly | no artifact | Block with specific “no OTA available” message |
| WendyOS + mender, explicit artifact | local/URL | Allow; Mender validates artifact compatibility |
| WendyOS + mender, no WiFi | auto/URL | Keep current local-download-and-serve behavior |

## Implementation plan

1. **Suppress automatic agent update before OS preflight**
   - Change `newOSUpdateCmd()` in `go/internal/cli/commands/os_cmd.go` to connect with `SuppressUpdateCheck()`.
   - Query `GetAgentVersion` immediately after connecting.
   - Do not run `ensureAgentUpToDate` until the target is confirmed to be WendyOS.

2. **Add a CLI WendyOS preflight helper**
   - Classify a target as WendyOS only when `GetAgentVersionResponse` includes WendyOS identity, currently `os_version` or `device_type`.
   - If neither is present:
     - for `os == "linux"`, return a friendly Ubuntu/generic Linux message;
     - for other OS values, return “Operating system updates are only supported on WendyOS devices.”
   - Check the feature set for `mender` after WendyOS identity is confirmed.

3. **Run agent update only after WendyOS preflight passes**
   - After the first preflight succeeds, run existing `ensureAgentUpToDate`.
   - Re-query `GetAgentVersion` after the potential restart.
   - Re-run the preflight before selecting/downloading artifacts.

4. **Remove unsafe OTA picker fallback for connected devices**
   - For `wendy os update` with no explicit artifact:
     - require `device_type`;
     - require that `device_type` exists in the manifest;
     - require an OTA artifact for the selected stable/nightly version.
   - Do not fall back to `pickOTAArtifactURL()` for a connected device; that can select the wrong artifact.
   - Keep the picker only if there is a separate explicit workflow that is not tied to a connected device.

5. **Keep explicit artifacts supported, but only on WendyOS**
   - If `[artifact-path]` or `--artifact-url` is supplied, skip manifest/device-type auto-selection.
   - Still require WendyOS identity and `mender` support before invoking `UpdateOS`.

6. **Add agent-side defense in depth**
   - In `go/internal/agent/services/agent_service.go` (`UpdateOS`) and `go/internal/agent/services/os_update_service.go` (`UpdateOS`), check WendyOS markers before `enableJetsonRootfsAB()` or Mender execution.
   - Return a friendly failed response / failed precondition when not WendyOS.
   - This protects MCP, direct RPC callers, and older CLIs.

7. **Tests**
   - Add table tests for the CLI preflight helper:
     - Ubuntu without Mender;
     - Ubuntu with Mender;
     - macOS;
     - WendyOS missing Mender;
     - WendyOS missing device type for auto-update;
     - WendyOS unknown device type;
     - WendyOS recognized device type;
     - explicit artifact case.
   - Add agent service tests proving non-WendyOS targets do not invoke Mender.

## Target user-facing error for the reported case

> This device is running Linux with wendy-agent installed, but it is not WendyOS. `wendy os update` only updates WendyOS devices. Use Ubuntu’s normal update tools for this machine, or run `wendy os install` to install WendyOS on supported hardware.
