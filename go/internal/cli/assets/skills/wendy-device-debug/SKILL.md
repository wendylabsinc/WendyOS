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

Check device participation with `wendy net status --device <target>` and this
computer's router with `wendy --json net topology --local-cli`. Verify the route
with a command to the returned `mesh://...` destination. Respect an explicit
local-router disconnect; first-hop discovery is automatic.

A stale clock may need an Internet-sharing mesh gateway for normal time sync.
Keep that path available and verify time recovery. Do not bypass certificate
checks, delete mesh identity/history, or re-enroll to work around clock errors.

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
