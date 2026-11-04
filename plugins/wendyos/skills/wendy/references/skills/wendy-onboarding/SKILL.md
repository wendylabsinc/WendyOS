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

- Local hardware, simulators, and project files use the CLI, or a local MCP
  connection in a desktop coding assistant. A Wendy Cloud account is optional.
- The public hosted plugin at `https://mcp.wendy.dev` uses a CLI relay.
  It needs a signed-in Wendy CLI on a computer, a paired `wendy cloud mcp`
  process, and that process to stay running. A Cloud account alone is not
  enough to use this connection.
- Browser/mobile conversations cannot directly launch laptop processes. Use
  the hosted relay after setup on a computer.

## Connect the public hosted plugin

Check `wendy --version` and `wendy cloud mcp --help` on the computer that will
run the relay. If the CLI is missing or does not support the command, use
[wendy-install](../wendy-install/SKILL.md). Do not install it into a remote
execution environment and call that the user's laptop. Without terminal access
to the chosen computer, explain each command and have the user run it there.

1. Use the intended Wendy Cloud operator session. If sign-in is needed, run
   `wendy auth login` and let the user finish authentication.
2. Start `wendy cloud mcp` in an interactive terminal. Calls run on this
   computer under that operator session, and access ends when the command
   exits. Keep the terminal and process running while using the plugin.
3. Connect the Wendy plugin in ChatGPT, inspect the tools it exposes, and call
   `wendy_status`. The first tool call starts pairing; OAuth sign-in alone does
   not. If the relay returns a pairing code, show it and have the user compare
   it with the code in the terminal. They must match before acceptance. Approval
   links that signed-in connection across chats while the relay session stays
   active. A new sign-in grant requires a new matching-code approval. Never ask
   the user to paste the pairing code into the conversation.
4. After acceptance, retry `wendy_status` and report the actual connection and
   authentication state before continuing. With `--auto-accept`, the initial
   call may still return `relay_not_paired` while approval is being processed;
   retry the harmless status call after the CLI reports acceptance. Do not
   retry state-changing tool calls automatically.

The default relay policy permits built-in read-only device and Cloud tools that
do not access the host, and asks for confirmation before each call. Add
`--allow-read` to skip confirmation for those tools only. Use
`cloud_discover(online_only=false)` to list the signed-in account's enrolled
devices, including offline devices. To allow these reads without prompting and
connect to one of those devices, restart the relay with:

```sh
wendy cloud mcp --allow-read --allow-tool device_connect
```

Use an exact selector returned by `cloud_discover`. The user must confirm the
connection call in the terminal. For LAN devices or simulators, `device_list`
also requires `--allow-tool device_list` and per-call terminal confirmation,
even without a network scan. Add other tool names only when needed for the
user's task. State changes and host access still require per-call
terminal confirmation after `--allow-tool`; enabling a tool does not approve
its next call. Read-only annotations on host or proxied tools do not qualify
for `--allow-read`. `--allow-all` permits every supported relay tool and skips
every per-call confirmation. Use it only when the user authorizes that access.

The public relay exposes a fixed catalog of tools. It does not expose
`wendy_tools`; use only the tools actually listed by the connected server.

If the user explicitly wants automatic ChatGPT pairing, `--auto-accept` accepts
requests from ChatGPT's known OAuth client identity. It does not trust the
display name alone, grant extra tool permissions, or skip per-call terminal
confirmations. Other clients keep manual pairing. Explain that new ChatGPT
connections for the same signed-in account can pair while the CLI is waiting.

If the organization disables MCP, the relay has exited, or the CLI and relay
report incompatible tool catalogs, explain that specific blocker. Restore the
authorized connection or update the CLI as appropriate. Do not switch accounts,
widen permissions, or claim another connection is available to bypass the failure.

## Set up a local MCP connection

If a local terminal is available on the user's selected computer, check whether
`wendy --version` works. If absent, use
[wendy-install](../wendy-install/SKILL.md). Their request to get started with a
local workflow authorizes ordinary CLI installation and connection setup;
explain what you are installing and proceed within the host's permissions.
Do not install into a remote execution environment and call it their laptop.
Without local terminal access, provide the OS-specific command and wait for
the user to report the result. Never ask them to run an unexplained list of
commands or to install all supported AI clients.

For a desktop coding assistant, run `wendy mcp setup` and follow
[wendy-mcp-setup](../wendy-mcp-setup/SKILL.md). Missing capabilities require a
CLI update and a new MCP process, not repeatedly calling unavailable tools.

## Choose a starting point

Use what the conversation and available tools already establish. If the user has
not chosen, ask whether they have a device ready to install or want to start with
a simulator while waiting for hardware. Offer the simulator path when they have
no hardware. Do not turn an empty discovery result into a flashing decision.

Inspect the running server's tools before calling them:

- With the public hosted relay, call `wendy_status` and `cloud_discover`.
  Calls follow the permission and confirmation rules above. `device_connect`
  and local discovery through `device_list` need separate permission.
- With the local CLI MCP server, call `wendy_status` and `device_list`. Enable
  `setup` for installation or `simulator` for local VMs through `wendy_tools`.
- With a local terminal, check `wendy --version`. Follow
  [wendy-install](../wendy-install/SKILL.md) only if CLI installation is needed.
  Use [wendy-mcp-setup](../wendy-mcp-setup/SKILL.md) when the user needs a local MCP
  connection. A hosted relay session does not imply access to their laptop.

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

Connect using `device_connect`. Do not invent a selector when the installed
device is not yet listed. Explain the remaining connection or enrollment step.

Installation tools require a local stdio session. The hosted relay cannot flash
the user's laptop media. If the tools are absent, follow the installation
skill's terminal path on the appropriate host. On Linux, a USB discovery warning means the user must run `wendy discover`
in a terminal and accept the host USB setup prompt before tethered access works.

## Start without hardware

List existing simulators with `simulator_list` before creating one. Reuse a VM
when it matches the user's purpose; preserve other VMs and their disks. Use
`generic` for ordinary WendyOS app development. Choose `go2` or `g1` only when
the user wants that robot simulation.

Create a named VM with `simulator_create`, using a name from the user's context
or an unused simple name such as `wendy-dev`. Omit `version` for the published
stable default unless the project requires a specific image. Explain that the
first download and boot can take several minutes.

Creation returns a stopped VM. Start it through the tools the server exposes:

- CLI MCP: `device_connect(device="vm:<name>")` boots and connects to it.
- Terminal: inspect `wendy vm --help`, create with
  `wendy vm create <name> --profile generic`, then connect with a device inspection
  such as `wendy --json device info --device vm:<name>`.

Refresh the simulator state and inspect the agent to verify the connection. For
a robot profile, call `simulator_viewer` when available; only describe its viewer
as ready when the result reports both `ready` and `healthy`. If the VM's agent
lacks robot support, `simulator_update_agent` can finish setup when available;
then start and verify again. Do not replace or delete the VM to recover an
uncertain operation.

Simulator operations require a local connection. When unavailable, explain
that requirement or use the installed CLI on the user's development host. Do
not claim a simulator was created on the user's laptop from a remote-only
connection.

## Verify and continue

Report the chosen target, physical or simulated, its explicit selector,
and what verification succeeded. Separate a running VM, an agent
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
