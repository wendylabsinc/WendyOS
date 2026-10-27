---
name: wendy
description: 'Build and deploy apps to WendyOS robots and edge devices, including NVIDIA Jetson, Raspberry Pi, Qualcomm Dragonwing, and MuJoCo simulation. Configure Wendy Data campaigns, record sensors and application data, and retrieve episodes.'
---

## Bundled workflows

This package includes the complete Wendy end-user skill group. Read the
relevant workflow below before acting, and resolve its references relative to
that file. These supporting skills need no separate installation.

For first-time setup, read
[wendy-onboarding](references/skills/wendy-onboarding/SKILL.md). It guides the user
from their goal through CLI installation when needed, a local or hosted
connection, and their first physical device or simulator. It also works before
any Wendy MCP tools are connected. Do not assume the user knows Wendy or has
installed its CLI.

Inspect the available tools first. The local CLI server uses `wendy_status` and
`device_list`. The public hosted relay uses `wendy_status` and `cloud_discover`.
It requires a signed-in CLI, pairing through `wendy cloud mcp`, and a running
relay process. Its default policy permits built-in read-only device and Cloud
tools that do not access the host, with confirmation for each call. `--allow-read`
skips confirmation for those tools only. `device_connect` and local discovery
through `device_list` require explicit `--allow-tool` permission and per-call
terminal confirmation. `--allow-all` permits every supported relay tool and
skips all per-call confirmations. `--auto-accept` controls ChatGPT pairing only.
Use the connected server's tools and explicit target identifiers. Use CLI
commands only when a terminal on the intended computer is available. A skill
cannot grant access or add tools missing from the server.

- [wendy](references/skills/wendy/SKILL.md)
- [wendy-app-lifecycle](references/skills/wendy-app-lifecycle/SKILL.md)
- [wendy-data](references/skills/wendy-data/SKILL.md)
- [wendy-device-debug](references/skills/wendy-device-debug/SKILL.md)
- [wendy-device-install](references/skills/wendy-device-install/SKILL.md)
- [wendy-device-ops](references/skills/wendy-device-ops/SKILL.md)
- [wendy-entitlements](references/skills/wendy-entitlements/SKILL.md)
- [wendy-install](references/skills/wendy-install/SKILL.md)
- [wendy-mcp-setup](references/skills/wendy-mcp-setup/SKILL.md)
- [wendy-onboarding](references/skills/wendy-onboarding/SKILL.md)
- [wendy-project-setup](references/skills/wendy-project-setup/SKILL.md)
- [wendy-robot-deploy](references/skills/wendy-robot-deploy/SKILL.md)
- [wendy-template-app](references/skills/wendy-template-app/SKILL.md)


# WendyOS

Wendy builds container apps and deploys them to WendyOS devices (NVIDIA Jetson
on JetPack 7.2 or later, Raspberry Pi 4/5, x86), local simulators (`vm:<name>`)
and the local Docker provider (`docker`).

## Choose the workflow

- Wendy MCP tools are connected (`wendy_status` is available): read
  [references/mcp.md](references/mcp.md) and use the tools.
- Build, deploy, logs, start, stop or remove with the CLI: use the commands
  below, and `wendy-app-lifecycle` for result fields, variants and the Docker
  provider.
- New app from a template: `wendy-template-app`. Entitlements: `wendy-entitlements`.
  Robots and ROS 2: `wendy-robot-deploy`. Sensor recording: `wendy-data`.
- Device info, WiFi and agent updates: `wendy-device-ops`. A failure the deploy
  result and logs do not explain: `wendy-device-debug`.
- No CLI yet: `wendy-install`. First-time user: `wendy-onboarding`. New board
  or empty discovery: `wendy-device-install`.
- ESP32 boards run Wendy Lite: `wendy docs wendy-lite/getting-started`.

## CLI commands

Pass `--device <selector>` to every command. Without a terminal the CLI prints
JSON on stdout and progress on stderr. `--yes` accepts prompts and creates a
minimal `wendy.json` when the project has none.

```bash
wendy --json run --device <selector> --detach --wait-ready --yes
wendy --json device apps list --device <selector>
wendy --json device logs --app <app-id> --tail 50 --no-follow --device <selector>
wendy json validate
wendy discover --json
wendy --json vm list
```

Run `--help` only for a command this skill and `wendy-app-lifecycle` do not
cover, or when one of these commands is rejected.

A deploy result shows that the app started and, with `--wait-ready`, that its
port accepts connections. Then check the behavior the user asked for once, such
as one HTTP request to the result's `url`, and report what you checked.

Keep tool output small: print status, identity, errors and the relevant lines
rather than full build logs, inspect objects or long JSON arrays.

## No device available

A simulator runs WendyOS with the same agent, containerd and entitlements as
hardware. When no physical device is listed, use one for builds, deploys, app
state, logs, HTTP endpoints, configuration, and ROS 2 apps on a robot profile
(`go2`, `g1`, `rosmaster-r2`). A request to test "on the device" counts unless
it needs the hardware listed below.

1. Use a connected or listed physical device when there is one.
2. Otherwise use an existing simulator from `wendy --json vm list` or a
   `simulator` row in MCP `device_list`. Tell the user which one and use it
   without waiting for approval: `--device vm:<name>` or `device_connect` boots
   a stopped one. Prefer a `generic` VM unless the task needs a robot profile.
3. With no simulator, ask the user before creating one (`wendy vm create` or
   MCP `simulator_create`): it downloads an image of a few hundred MB, and the
   first boot can take minutes.
4. Report results as verified on simulator `vm:<name>`, and say that the
   user's hardware was not connected. Leave the VM running unless asked.

Physical cameras, microphones, speakers, Bluetooth, GPU or CUDA, GPIO, USB and
serial peripherals, OS installation, real network conditions, performance and
physical motion still need hardware. Run what the simulator can and name what
remains unverified.
