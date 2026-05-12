# Plan: Friendly errors for unsupported `wendy os update` targets

## Problem

Running `wendy os update` against an Ubuntu/Linux host with `wendy-agent` installed can currently proceed far enough to:

1. update the agent,
2. fetch/select a WendyOS OTA artifact,
3. download and serve a multi-GB Mender artifact,
4. call `UpdateOS`,
5. fail only when `mender install` rejects the artifact.

That setup is not compatible with the current Mender/WendyOS OTA path, so the CLI and agent should refuse the update early with a clear message that explains the appropriate update mechanism for that host.

## Unsupported combinations to catch

| Device state | Artifact input | Desired behavior |
| --- | --- | --- |
| No reachable gRPC agent / BLE-only / Wendy Lite / external provider | any | Friendly “this target cannot apply OS image updates through `wendy os update`” error with next steps |
| macOS / Windows agent | any | Block this OS-image update path and point to the host OS/package-manager update flow |
| Generic Linux, e.g. Ubuntu, no WendyOS markers | none / URL / local | Block before agent/OS artifact work and recommend the distro package manager for host updates |
| Generic Linux with `mender-update` installed | none / URL / local | Block before agent/OS artifact work and explain that Mender alone does not make it a compatible WendyOS OTA target |
| WendyOS marker present, but no `mender-update` | any | Friendly “this WendyOS image does not support OTA updates / is missing Mender” |
| WendyOS + mender, but missing `device_type` | no artifact | Block auto-update: cannot safely choose artifact |
| WendyOS + mender, unknown `device_type` not in manifest | no artifact | Block auto-update: no OTA artifact for this device type |
| WendyOS + mender, recognized `device_type`, no OTA for latest/nightly | no artifact | Block with specific “no OTA available” message |
| WendyOS + mender, explicit artifact | local/URL | Allow; Mender validates artifact compatibility |
| WendyOS + mender, no WiFi | auto/URL | Keep current local-download-and-serve behavior |

## Implementation plan

1. **Suppress automatic agent update before OS-image preflight**
   - Change `newOSUpdateCmd()` in `go/internal/cli/commands/os_cmd.go` to connect with `SuppressUpdateCheck()`.
   - Query `GetAgentVersion` immediately after connecting.
   - Do not run `ensureAgentUpToDate` until the target is confirmed to be compatible with this `wendy os update` flow. Agent updates may be valid on other platforms, but this command should not update an agent as a side effect when the OS update itself cannot proceed.

2. **Add a CLI preflight helper for the current Mender/WendyOS OTA path**
   - Classify a target as compatible with the current OTA path only when `GetAgentVersionResponse` includes WendyOS identity, currently `os_version` or `device_type`.
   - If neither is present:
     - for `os == "linux"`, return a friendly Ubuntu/generic Linux message that says this installation cannot be updated with WendyOS OTA artifacts and recommends the distro package manager for host OS updates;
     - for other OS values, return a platform-specific message that says this setup does not support `wendy os update` and points users to the platform's normal update mechanism.
   - Check the feature set for `mender` after WendyOS identity is confirmed.

3. **Run agent update only after OS-image preflight passes**
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

5. **Keep explicit artifacts supported for compatible OTA targets**
   - If `[artifact-path]` or `--artifact-url` is supplied, skip manifest/device-type auto-selection.
   - Still require compatibility with the current Mender/WendyOS OTA path before invoking `UpdateOS`.

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

> This device is running Linux with wendy-agent installed, but this setup cannot be updated with `wendy os update`. Use Ubuntu’s normal update tools, such as `apt`, to update this machine. To use WendyOS OTA updates, install WendyOS on supported hardware with `wendy os install`.
