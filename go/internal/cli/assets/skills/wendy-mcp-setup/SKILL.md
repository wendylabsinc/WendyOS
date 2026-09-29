---
name: wendy-mcp-setup
description: Use when configuring, verifying, or explaining the Wendy CLI MCP server for AI assistants with `wendy mcp setup` or `wendy mcp serve`, especially Claude Code, Claude Desktop, Codex, or device-tool access.
---

# Wendy MCP Setup Workflow

Use this when a developer wants AI assistants to access Wendy device tools through the Wendy CLI MCP server.

## Preconditions

Verify the CLI first:

```bash
wendy --version
wendy --json info
```

If the MCP server should connect to a device on startup, either set a default device or use `--device` when launching the server:

```bash
wendy discover --json --timeout 5s
wendy device set-default <hostname>
wendy device get-default
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

## Manual server command

The MCP server is stdio-based and is normally launched by an MCP host:

```bash
wendy mcp serve
wendy mcp serve --device <hostname>
```

`--device` can also be written as `-d`. If omitted, the server attempts to use the configured default device.

For a host not detected by `wendy mcp setup`, configure a stdio server named `wendy` with command `wendy` and args `["mcp", "serve"]`. Add `--device <hostname>` only when the user wants that MCP server pinned to one device.

## Verification

Call `wendy_status` after restarting the host and inspect the actual tool list.
The server reports its CLI version; changing the CLI on disk does not replace an
already running MCP process. A skill mentioning a tool is not evidence that the
connected server exposes it. Setup installs the same end-user skill group for
Codex and OpenCode under `~/.agents/skills`, and Claude Code under
`~/.claude/skills`, including references. Internal engineering skills are
separate. If the optional plugin is also installed, choose one installation
method to avoid duplicate skill selectors.

No connected device is required for `os_install_plan` or `os_list_drives`.
Use `wendy-device-install` for a blank board before debugging device RPCs.

Before blaming MCP, verify device access directly with `wendy-device-ops`:

```bash
wendy --json device version --device <hostname>
wendy --json device hardware list --device <hostname>
```

If `wendy mcp serve` logs a warning that it could not connect to a device, set a default device or configure the MCP host to launch `wendy mcp serve --device <hostname>`.

## Safety

MCP exposes operational device tools to the assistant host. Do not configure it silently. Tell the user what local config file was changed when the CLI reports it, and do not copy tokens, WiFi passwords, or device-specific secrets into reusable docs or issue text.
