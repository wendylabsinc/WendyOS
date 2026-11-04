---
name: wendy-device-debug
description: Diagnose an end-user's unreachable WendyOS or Wendy Lite device, failed app deployment, ESP32 camera/SensorLink failure, missing camera or GPU, or unhealthy app using CLI and MCP observations, device state, and app logs. For a never-installed board use wendy-device-install.
---

# Wendy Device Debug Workflow

Use this for deployment and device problems in an end-user's app. Work with the
installed CLI, live MCP tools, and app project. A Wendy source checkout is not
required.

## Triage sequence

1. Capture the exact failing command, CLI version, MCP status and error output.
2. Confirm the target with `device_list` or `wendy discover --json`. An empty
   scan on new hardware routes to `wendy-device-install`. While the device is
   unreachable, a simulator can show whether the app builds, starts and serves
   correctly, separating app defects from device faults; it cannot diagnose the
   device itself. Choose one as the [wendy](../wendy/SKILL.md) skill's
   *No device available* section describes.
3. Follow [wendy-device-ops](../wendy-device-ops/SKILL.md) for device access and
   inspection of versions, connectivity, hardware and WiFi. Keep an explicit
   device selector throughout diagnosis. For Wendy Lite, check the reported
   board/target, firmware version, WASM/native app support and SensorLink manifest.
4. Validate the app's `wendy.json`. Check its Dockerfile/Containerfile,
   dependencies, architecture, entitlements, environment and startup logs.
5. Use `wendy-app-lifecycle` to inspect app state and logs. A successful deploy
   or running container is not a health check. Probe the app's actual output.
   With MCP, `container_list` shows `termination_reason` (for example
   `ENTITLEMENT_DENIED`) and `telemetry_logs` returns app logs. Enable
   `observability` for `app_inspect` (service states, recorded exit, recent
   errors) and `device_os_logs` (kernel messages such as driver, USB or
   out-of-memory events). The `diagnose_container` prompt follows these steps.
   Read `wendy://diagnostics` when an expected MCP tool is missing.
6. For robot sensor, DDS or stop behavior, follow `wendy-robot-deploy` and keep
   physical motion stopped during diagnosis.

Prefer fixes in the app container or `wendy.json`. Use documented CLI operations
for device repair. Do not patch Wendy's agent or OS implementation as part of
ordinary app debugging, and do not publish credentials or device-specific secrets.

## Mesh connectivity and clock errors

Distinguish a device's ordinary LAN connection from this computer's Wendy Net
router. A device with mesh participation disabled can still be managed over LAN.
For mesh problems, inspect `wendy device wendy-net status --device <target>` and
`wendy --json device wendy-net topology --local-cli` separately. A ready local
connection does not prove that the destination has a current route; verify it
with a real command to the returned `mesh://...` destination.

If the user explicitly disconnected the local mesh router, preserve that choice.
Explain how to reconnect rather than enabling it as a side effect of diagnosis.
Router addresses, Bluetooth identifiers and PSMs are discovered automatically.
Do not suggest removed manual first-hop flags.

For certificate or clock errors, retain the exact error and inspect versions and
time state through the available CLI/MCP tools. A device recovering from an old
clock may need a working mesh path to a gateway with Internet access before its
normal time synchronization can finish. Keep that path available and check the
result; do not assume a selected gateway proves time recovery. Do not bypass
certificate verification, delete mesh identity/history, or re-enroll as a routine
workaround. Report a missing clock-inspection capability instead of inventing a
tool or shell access.

## Jetson GPU checks

Use device info, hardware capabilities, and app logs to check:

- The reported device type identifies the expected Jetson variant.
- The agent maps the device type to the expected Wendy platform.
- The application handles CUDA absence gracefully and exposes enough debug state.

If evidence points to GPU provisioning, record the device's JetPack version.
JetPack 6 uses `/etc/cdi/nvidia.yaml`; JetPack 5 uses the L4T CSV fallback at
`/etc/nvidia-container-runtime/host-files-for-container.d/*.csv`. If CLI/MCP
cannot confirm this state, report the missing check with the platform issue.

## Escalate a platform bug

If evidence points to the CLI, agent or OS, prepare a minimal reproduction with
versions, board type, sanitized logs, expected behavior and observed behavior.
Explain any documented upgrade or workaround. Leave implementation changes to
a separate Wendy engineering task; do not require the user to clone Wendy's
repositories to finish app setup.

## Output discipline

State what the evidence establishes, what remains uncertain, the app or device
fix applied, and the verification result.
