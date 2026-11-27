---
name: wendy-mcp-setup
description: Use when configuring, verifying, or explaining the Wendy CLI MCP server for AI assistants with `wendy mcp setup` or `wendy mcp serve`, especially Claude Code, Claude Desktop, Codex, or device-tool access.
---

# Wendy MCP Setup Workflow

Use this when a developer wants AI assistants to access Wendy device tools through the Wendy CLI MCP server.

## Preconditions

If the user does not yet have the CLI, first use
[wendy-install](../wendy-install/SKILL.md). A hosted ChatGPT connection needs no
local CLI; use its existing plugin connection flow instead. Local setup must
run on the user's intended computer.

Verify the CLI first:

```bash
wendy --version
wendy --json info
```

If the MCP server should connect to a device on startup, find its hostname and pass it with `--device` when launching the server, or set `WENDY_DEVICE` in the MCP host's `env` block:

```bash
wendy discover --json --timeout 5s
```

## Automatic setup

Run this only when the user wants Wendy MCP written into local AI tool configuration:

```bash
wendy mcp setup
```

Setup configures detected Claude Code, Claude Desktop, Codex, Cursor, Windsurf and OpenCode clients. It writes a stdio MCP server entry that runs the installed `wendy` binary with:

```bash
wendy mcp serve
```

After setup, restart or reload the target AI tool so it discovers the new MCP server.

## ChatGPT connections

ChatGPT uses the hosted Wendy plugin. It reaches devices through the Cloud
relay and a signed-in CLI session on the user's computer:

```bash
wendy auth login
wendy cloud mcp --allow-read --allow-tool device_connect
```

The user connects the plugin in ChatGPT with the same account and organization,
asks for Wendy's status, and accepts the matching pairing code in the terminal.
Calls stop when `wendy cloud mcp` exits. `wendy mcp setup` does not configure
ChatGPT.

## Manual server command

The MCP server is stdio-based and is normally launched by an MCP host:

```bash
wendy mcp serve
wendy mcp serve --device <hostname>
```

`--device` can also be written as `-d`. If omitted, the server uses `WENDY_DEVICE` when it is set, and otherwise starts without a device.

Only core tools are advertised at first. `--tool-groups` chooses the initial
groups (`core`, `setup`, `simulator`, `hardware`, `robotics`, `observability`,
`cloud` or `all`), and `wendy_tools` changes them while the server runs. Use
`--tool-groups all` for a host that does not refresh its tool list.

For a host not detected by `wendy mcp setup`, configure a stdio server named `wendy` with command `wendy` and args `["mcp", "serve"]`. Add `--device <hostname>` only when the user wants that MCP server pinned to one device.

## Verification

Call `wendy_status` after restarting the host and inspect the actual tool list;
`wendy_tools` with no arguments lists the enabled groups and their tools. If a
tool returns `AUTH_REQUIRED`, or `wendy_status` reports `auth` as `logged_out` or
`expired` and the task needs a Cloud device, call `auth_login` and show the user
its link. Local simulators and LAN devices that are not Cloud-enrolled need no
sign-in. The server reports its CLI version; changing the CLI on disk does
not replace an already running MCP process. It also serves this skill bundle
through the MCP skills extension, its guide as `wendy://guide`, and Wendy docs
under `wendy://docs/`. A skill mentioning a tool is not evidence that the
connected server exposes it. Setup installs the same end-user skill group for
Codex and OpenCode under `~/.agents/skills`, and Claude Code under
`~/.claude/skills`, including references. Internal engineering skills are
separate. If the optional plugin is also installed, choose one installation
method to avoid duplicate skill selectors.

No connected device is required for `os_install_plan` or `os_list_drives`. They are
in the `setup` tool group: call `wendy_tools(groups=["setup"])` to list them.
Use `wendy-device-install` for a blank board before debugging device RPCs.

Before blaming MCP, verify device access directly with `wendy-device-ops`:

```bash
wendy --json device version --device <hostname>
wendy --json device hardware list --device <hostname>
```

If `wendy mcp serve` logs a warning that it could not connect to a device, configure the MCP host to launch `wendy mcp serve --device <hostname>` or to set `WENDY_DEVICE`.

## Safety

MCP exposes operational device tools to the assistant host. Do not configure it silently. Tell the user what local config file was changed when the CLI reports it, and do not copy tokens, WiFi passwords, or device-specific secrets into reusable docs or issue text.
