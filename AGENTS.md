# Wendy Agent — AI Assistant Guide

Wendy is a CLI and agent platform for developing and deploying applications on
WendyOS edge devices (Raspberry Pi, NVIDIA Jetson, x86 SBCs, and more).

## Pull request descriptions

Every PR created from now on must use only the sections below, in this order.
Keep each section concise, list only affected items, and write `None` when a
section does not apply. Do not add a summary, test plan, checklist, or other
sections. These limits apply to the PR description, not to performing tests or
other required checks.

1. **Affected build products**
   - WendyOS
   - Wendy Lite
   - Wendy Agent
   - CLI
   - iOS/Android/macOS app, specifying the affected platforms
   - WASM
2. **Affected components**
   - Networking, specifying mesh networking when affected
   - Container Runtime
   - Cloud Connectivity
   - TLS/Cryptography or Security
   - Name other affected components explicitly instead of writing `Etc.`
3. **Type of change(s)**
   - Identify the applicable types, including UI/UX, wire protocol, bugfix,
     feature, refactor, or documentation.
   - UI/UX includes iOS, Android, macOS, and CLI developer experience.
   - For CLI or app UI/UX changes, include a GIF, screenshots, or video in this
     section demonstrating the changed behavior.
4. **Significant items of note**
   - Explicitly identify wire protocol breaking changes and added API surface.
   - Identify other breaking changes and any action existing users must take.
5. **Risk**
   - The PR author must state `low`, `medium`, or `high` and apply exactly one
     matching PR label:
     `risk: low`, `risk: medium`, or `risk: high`.
   - Risk means the chance of breaking existing users' workflows, including
     crashes, vulnerabilities, and compatibility regressions. Assess the actual
     behavior affected, not the size of the diff or whether it is called a bugfix.
   - Include a brief reason for the rating in this section.
6. **Architectural changes**
   - Explicitly describe any broad architectural change, such as adding a major
     ecosystem component or changing responsibilities between components.
   - If the PR is solely a bugfix or a feature users could reasonably expect to
     already exist, and makes no architectural change, state that here. For
     example: `No architectural change. This fixes existing expected behavior.`
     This lets reviewers avoid unnecessary architecture review. It does not
     replace disclosure of breaking changes, added APIs, or risk above.

## Optional AI reviews

Security review runs automatically on PRs ready for review and subsequent pushes.
To request additional AI reviews, apply the corresponding PR labels:

- `review: api` for API and compatibility review, including a risk recommendation.
- `review: docs` for documentation coverage suggestions.
- `review: integration-tests` for integration-test coverage suggestions.

Each label starts its review and enables reruns on subsequent pushes until removed.
Explicitly requested reviews can run on drafts. API review recommends a risk rating
in its comment but does not change the author's risk label. Its `api-review` finding
label is separate from the `review: api` opt-in label. Existing manual API validation
remains available without publishing results. Builds and tests run as before.

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
   If it reports `auth` as `logged_out` or `expired` and the task needs a Cloud
   device, call `auth_login` and show the user the returned link. Local simulators
   and LAN devices that are not Cloud-enrolled need no sign-in.
2. Call `device_list` (optionally `scan: true`) to find available devices.
   - On Linux, a USB-tethered device can't be reached until the host's USB link is
     configured; `device_list` then returns a `usb` warning. Ask the user to run
     `wendy discover` in a terminal and accept its USB setup prompt (it needs sudo).
   - It also lists local simulators (`vm:name`). Without hardware, connect to one
     rather than skipping on-device checks; ask the user before creating a new one.
3. Call `device_connect` with a selector from `device_list`: host:port for LAN,
   `vm:name` for a simulator, or a `cloud://` selector for a cloud device.
4. Use container, WiFi, hardware, telemetry, and OS tools. Only core tools are
   listed at first; enable the others by group with `wendy_tools` (below).

For a new board without WendyOS/Agent, enable the `setup` group with
`wendy_tools(groups=["setup"])`, then use `os_install_plan` and `os_list_drives`
before connecting. For supported image installs, use `os_install_start`, show the
physical instructions, then `os_install_resume` to probe the target. Obtain explicit
erase authorization for the returned fingerprint before starting the write. Poll
`os_install_status`; resume with an explicit address to verify first boot, or use
`os_install_verify` independently. Follow the returned terminal command if elevation
is required. Unitree G1 PC2 keeps
vendor Ubuntu and receives the Agent rather than a generic Jetson image.

Enable specialist groups with `wendy_tools`: `simulator` manages local VM lifecycle,
`observability` adds `app_inspect` and kernel logs, `setup` includes OS installs,
WiFi, project validation and agent updates, `hardware` adds cameras and Bluetooth,
`robotics` adds ROS 2 inspection, and `cloud` includes `cloud_connect` and tunnels.
Simulator creation returns a stopped VM; connect to its `vm:name` selector to
boot. Validate projects before deployment and inspect individual service states
afterward. Missing readiness evidence stays unknown, including unverified
cloud/simulator TCP forwarding.

To build and deploy a local project to any device (direct or cloud):

```sh
wendy run --device <name>
```

Use the `run` MCP tool to deploy from within an AI session.
It uses an explicit `device` selector or the current connection's target and
transport; legacy `device_name` selects cloud deployment. Detached success does
not verify readiness. Check container state, logs and actual app/ROS output.

## Connection Model

Most MCP tools require an active device connection. The `run` tool deploys to
the connected target or an explicit `device`; with neither it returns
`NOT_CONNECTED` rather than picking a device.

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
