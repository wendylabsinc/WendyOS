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
   scan on new hardware routes to `wendy-device-install`.
3. Follow [wendy-device-ops](../wendy-device-ops/SKILL.md) for device access and
   inspection of versions, connectivity, hardware and WiFi. Keep an explicit
   device selector throughout diagnosis. For Wendy Lite, check the reported
   board/target, firmware version, WASM/native app support and SensorLink manifest.
4. Validate the app's `wendy.json`. Check its Dockerfile/Containerfile,
   dependencies, architecture, entitlements, environment and startup logs.
5. Use `wendy-app-lifecycle` to inspect app state and logs. A successful deploy
   or running container is not a health check. Probe the app's actual output.
6. For robot sensor, DDS or stop behavior, follow `wendy-robot-deploy` and keep
   physical motion stopped during diagnosis.

Prefer fixes in the app container or `wendy.json`. Use documented CLI operations
for device repair. Do not patch Wendy's agent or OS implementation as part of
ordinary app debugging, and do not publish credentials or device-specific secrets.

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
