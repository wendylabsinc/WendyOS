---
name: wendy-onboarding
description: Help a first-time user who may not know Wendy or have its CLI installed choose local or hosted access, install tools when needed, and verify a first physical device or simulator. Use for getting started and plugin onboarding, including when no MCP tools are connected.
---

# Get started with WendyOS

Start with what the user wants to build or try. If Wendy is unfamiliar, explain
it once in plain language: "Wendy helps you build and run apps on robots and
small computers, and lets you try them in a simulator first." Do not require
knowledge of WendyOS, the CLI, MCP, device selectors, or Cloud accounts.

Help the user reach one verified target they can develop against. They can
start with hardware or a simulator and keep the same project when hardware
arrives. Plugin installation does not establish that the CLI exists or that
any MCP server successfully started.

## Start before tools are connected

Use the user's goal and environment first. When missing, ask which computer OS
they use and whether they have hardware, want a simulator, or already have
devices connected to a hosted account. Ask only what the next step needs.

- Local hardware, simulators, and project files use the desktop connection.
  A Wendy Cloud account is optional. A local gateway can also access Cloud
  devices when the user explicitly adds those targets and logs in.
- Existing hosted devices use the registered hosted plugin and its connection
  flow. No CLI is needed on the user's computer for hosted inspection and app
  control. Do not install a CLI just to connect their hosted account.
- Browser/mobile conversations cannot directly launch laptop processes. Use
  the hosted connection or guide the user through local desktop setup. A
  private OpenAI MCP tunnel is an optional advanced route, with its own setup.

If a local terminal is available on the user's selected computer, check whether
`wendy --version` works. If absent, use
[wendy-install](../wendy-install/SKILL.md). Their request to get started with a
local workflow authorizes ordinary CLI installation and connection setup;
explain what you are installing and proceed within the host's permissions.
Do not install into a remote execution environment and call it their laptop.
Without local terminal access, provide the OS-specific command and wait for
the user to report the result. Never ask them to run an unexplained list of
commands or to install all supported AI clients.

After CLI installation, check `wendy mcp setup chatgpt --help`. If available,
run `wendy mcp setup chatgpt` for the local desktop connection. It creates a
restricted gateway policy that can start before there are any devices, plus a
personal plugin marketplace. Use the printed paths and restart/install steps.
No Cloud login, simulator, disk write, or broader developer toolset is enabled
by this default setup. Missing capabilities require a CLI update and a new MCP
process, not repeatedly calling unavailable tools.

Apply only the permissions needed for the user's chosen next step:

```sh
# A discovered physical device, using the exact address returned by discovery:
wendy mcp setup chatgpt --device <address>
# A simulator, before hardware arrives:
wendy mcp setup chatgpt --simulators
# A project the user wants to edit and deploy, bound to the chosen targets:
wendy mcp setup chatgpt --workspace <project-directory> --device <address>
# Local installation tools when the user asks to install hardware:
wendy mcp setup chatgpt --host-operations
```

Repeat `--device` to authorize both local and Cloud targets. Use `wendy discover
--json` for local discovery. A device is read-only by default; app control and
camera permissions are separate from simply listing it. Restart the gateway
after a policy change. Do not widen physical-device app or camera permissions
without the corresponding user task.

Hosted personal/workspace packaging uses an existing registered app ID with
`--connection hosted --app-id <registered-app-id>`. `--connection both` creates
separate local and hosted entries. These commands are operator packaging steps,
not prerequisites for a novice connecting an already available hosted plugin.
They do not deploy a server, register an app, or publish to the public directory.

The broader `wendy mcp serve` tools are opt-in through `--developer-tools`.
Explain that they have broader access and do not inherit the gateway policy.
When both toolsets exist, use the gateway for scoped device UI operations and
explicit `robot_id` values. Use the developer server only for the requested
developer task and its explicit target. Its selected device does not select a
device in the gateway. Never use it to work around a denied gateway operation.

## Choose a starting point

Use what the conversation and available tools already establish. If the user has
not chosen, ask whether they have a device ready to install or want to start with
a simulator while waiting for hardware. Offer the simulator path when they have
no hardware. Do not turn an empty discovery result into a flashing decision.

Inspect the running server's tools before calling them:

- With the CLI MCP server, call `wendy_status`. Enable `setup` for installation
  or `simulator` for local VMs through `wendy_tools`.
- With the ChatGPT gateway, use `list_robots` to find existing authorized devices.
  Call `open_devices` when available to open the device and simulator workspace.
  It prefers fullscreen, but the host chooses the actual display mode. Continue
  in the conversation when no UI is available.
- With a local terminal, check `wendy --version`. Follow
  [wendy-install](../wendy-install/SKILL.md) only if CLI installation is needed.
  Use [wendy-mcp-setup](../wendy-mcp-setup/SKILL.md) when the user needs a local MCP
  connection. A running remote gateway does not imply access to their laptop.

Plugin installation alone does not authorize an OS installer or a disk erase.
The user's setup request authorizes ordinary setup work; get specific erase
authorization only after identifying the actual target and scope.

## First physical device

Identify the board and carrier, current OS, host OS, intended storage, and network.
Inspect available device information first and ask only for missing details. If
WendyOS is already installed, verify that device instead of reinstalling it.

Read [wendy-device-install](../wendy-device-install/SKILL.md) for board-specific
requirements. Jetson developer-kit images need a confirmed developer-kit carrier.
Unitree G1 PC2 keeps vendor Ubuntu and receives the Agent.

For a supported image install through MCP:

1. Call `os_install_plan`. For raw media, call `os_list_drives` and match the
   intended SD card or SSD by path, model, and capacity. Re-plan with that drive.
2. Call `os_install_start` and retain its `job_id`. Show its physical instructions
   for cables, recovery mode, media, and power before continuing.
3. After the user completes those steps, call `os_install_resume` to probe the
   target. Show the returned fingerprint and erase scope. Only after explicit
   authorization for that target, resume with that exact `target_id` and
   `confirm_erase=true`. Set `confirm_internal=true` only when separately approved.
4. Poll `os_install_status`. If elevation or an unsupported installation method
   requires a terminal, give the returned command and explain which host runs it.
   Never collect an administrator password in chat. Inspect an interrupted job
   before retrying; a timeout does not mean the write failed.
5. Follow the first-boot instructions. Discover the intended device and resume
   the job with its explicit address, or use `os_install_verify` with the planned
   device type and OS version. Record a verified identity before connecting.

With the CLI server, connect using `device_connect`. With the gateway, refresh
`list_robots` and use the authorized `robot_id` with `inspect_robot`. Do not invent
an ID or widen a grant when the installed device is not yet listed. Explain the
remaining connection or enrollment step.

Gateway installation tools require an enabled local stdio session with host
permissions. HTTP gateways cannot flash the user's laptop media. If the tools
are absent, follow the installation skill's terminal path on the appropriate
host. On Linux, a USB-C discovery warning means the user must run `wendy discover`
in a terminal and accept the host USB setup prompt before tethered access works.

## Start without hardware

List existing simulators with `simulator_list` before creating one. Reuse a VM
when it matches the user's purpose; preserve other VMs and their disks. Use
`generic` for ordinary WendyOS app development. Choose `go2` or `g1` only when
the user wants that robot simulation.

Before a robot simulation task, read
[wendy-robot-deploy](../wendy-robot-deploy/SKILL.md). Simulation and sim-to-real
must NEVER use any form of scripting. Interpret live sensor readings and measured
motor movement through the target's supported tools for each behavior decision.
Do not create a scripted demo or replay motion to verify the simulator.

Create a named VM with `simulator_create`, using a name from the user's context
or an unused simple name such as `wendy-dev`. Omit `version` for the published
stable default unless the project requires a specific image. Explain that the
first download and boot can take several minutes.

Creation returns a stopped VM. Start it through the tools the server exposes:

- CLI MCP: `device_connect(device="vm:<name>")` boots and connects to it.
- Local ChatGPT gateway: `simulator_start(name="<name>")` boots it. Use task
  augmentation when supported and poll that task rather than starting it again.
- Terminal: inspect `wendy vm --help`, create with
  `wendy vm create <name> --profile <selected-profile>`, using the `generic`, `go2`
  or `g1` profile selected above, then connect with a device inspection
  such as `wendy --json device info --device vm:<name>`.

Refresh the simulator state and inspect the agent to verify the connection. For
a robot profile, call `simulator_viewer` when available; only describe its viewer
as ready when the result reports both `ready` and `healthy`. If the VM's agent
lacks robot support, `simulator_update_agent` can finish setup when available;
then start and verify again. Do not replace or delete the VM to recover an
uncertain operation.

Gateway simulator operations require a local stdio connection and permission
through `allow_simulators` with `simulators:manage`, or approved host operations.
When unavailable, explain that local connection requirement or use the installed
CLI on the user's development host. Do not claim a simulator was created on the
user's laptop from a remote-only connection.

## Verify and continue

Report the chosen target, physical or simulated, its explicit selector or
authorized ID, and what verification succeeded. Separate a running VM, an agent
connection, and app readiness. A simulator cannot verify physical cameras, GPU
availability, or robot motion on hardware.

If the user also wants to build an app, continue with
[wendy-template-app](../wendy-template-app/SKILL.md) and
[wendy-app-lifecycle](../wendy-app-lifecycle/SKILL.md). Deploy to the verified
target explicitly. For VM HTTP apps, use the returned forwarded URL and test the
actual response; `vm:<name>` is a device selector, not a browser address.

When physical hardware arrives, install and verify it using the physical path,
check its capabilities and entitlements, then deploy the same project with an
explicit new target. Keep the simulator unless the user asks to remove it.
For robot sim-to-real, follow `wendy-robot-deploy` and interpret fresh hardware
sensor and motor feedback. Never transfer or replay a scripted simulator sequence.
