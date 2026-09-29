# Wendy Agent — AI Assistant Guide

Wendy is a CLI and agent platform for developing and deploying applications on
WendyOS edge devices (Raspberry Pi, NVIDIA Jetson, x86 SBCs, and more).

## Setup

Install the CLI:

```sh
curl -fsSL https://install.wendy.dev/cli.sh | bash
```

Configure the MCP server for your AI coding tool:

```sh
wendy mcp setup
```

Supports: Claude Code, Claude Desktop, Cursor, Windsurf, Codex.

## Quick Start (with MCP)

1. Call `wendy_status` to see current connection state and a suggested next step.
   Check the running server's version and tool list; new skills do not update an existing MCP process.
2. Call `device_list` (optionally `scan: true`) to find available devices.
   - For USB-C tethered devices on Linux, run `sudo wendy device usb-setup` first.
3. Call `device_connect` or `cloud_connect` to connect.
4. Use container, WiFi, hardware, telemetry, and OS tools.

For a new board without WendyOS/Agent, use `os_install_plan` and `os_list_drives`
before connecting. For supported image installs, use `os_install_start`, show the
physical instructions, then `os_install_resume` to probe the target. Obtain explicit
erase authorization for the returned fingerprint before starting the write. Poll
`os_install_status`; resume with an explicit address to verify first boot, or use
`os_install_verify` independently. Follow the returned terminal command if elevation
is required. Unitree G1 PC2 keeps
vendor Ubuntu and receives the Agent rather than a generic Jetson image.

Enable specialist groups with `wendy_tools`: `simulator` manages local VM lifecycle,
`observability` adds `app_inspect` and kernel logs, `setup` includes project validation
and agent updates, and `cloud` includes tunnel list/close. Simulator creation returns
a stopped VM; connect to its `vm:name` selector to boot. Validate projects before
deployment and inspect individual service states afterward. Missing readiness
evidence stays unknown, including unverified cloud/simulator TCP forwarding.

To build and deploy a local project to any device (direct or cloud):

```sh
wendy run --device <name>
```

Use the `run` MCP tool to deploy from within an AI session.
It uses an explicit `device` selector or the current connection's target and
transport; legacy `device_name` selects cloud deployment. Detached success does
not verify readiness. Check container state, logs and actual app/ROS output.

## Connection Model

Most MCP tools require an active device connection. The `run` tool works
without a prior connection (it manages the cloud tunnel internally).

## Device troubleshooting

Start with [wendy-device-ops](plugins/wendy-agentic-coding/skills/wendy-device-ops/SKILL.md)
for device inspection and access rules. Use [wendy-device-debug](plugins/wendy-agentic-coding/skills/wendy-device-debug/SKILL.md)
to trace failures across the CLI, firmware, OS, and app.

## Authentication

Log in to Wendy Cloud:

```sh
wendy auth login
```

Discover cloud-enrolled devices:

```sh
wendy cloud discover
```
