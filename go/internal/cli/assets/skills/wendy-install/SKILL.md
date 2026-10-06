---
name: wendy-install
description: Use when a developer needs to install, update, verify, or repair the Wendy CLI, MCP server, or the plugin itself before using `wendy run`, `wendy discover`, or device workflows.
---

# Wendy CLI Install Workflow

Use this when the user asks to set up Wendy, install the Wendy CLI, verify their local Wendy installation, or prepare a machine for Wendy development.

## Install policy

Installing this plugin must not silently install Wendy CLI. Treat Wendy CLI installation as an explicit task that the agent performs after the user asks for setup or after the user approves a proposed setup step.

Do not run an OS-level installer if `wendy --version` already works unless the user explicitly asks to reinstall or upgrade.

Check the installed CLI's help and the running MCP tool list for each required capability. A historical minimum version does not establish that installation planning, ROS diagnostics or newer deployment routing is available. Update the CLI when needed, then restart the MCP host.

## Detect the host

Use the current OS, not assumptions:

```bash
uname -s
command -v wendy || true
wendy --version
```

On Windows, use PowerShell equivalents:

```powershell
Get-Command wendy -ErrorAction SilentlyContinue
wendy --version
```

## Install commands

macOS and Linux:

```bash
curl -fsSL https://install.wendy.dev/cli.sh | bash
```

Windows:

```powershell
winget install WendyLabs.Wendy --source winget
```

If a shell needs reloading after installation, tell the user exactly which command to run or open a new terminal. Do not assume PATH changes have already propagated.

## Verify

After install or repair:

```bash
wendy --version
wendy discover --json
```

When working from source, use the checkout's documented build command and record the binary path being tested.

## Update

### Wendy CLI

Re-run the installer to upgrade in-place:

macOS and Linux:
```bash
curl -fsSL https://install.wendy.dev/cli.sh | bash
```

Windows:
```powershell
winget upgrade WendyLabs.Wendy --source winget
```

After updating, verify the version meets the minimum requirement:
```bash
wendy --version
```

### Wendy MCP

After a CLI update, reconfigure the MCP server so AI tools pick up the updated binary:
```bash
wendy mcp setup
```

Then restart or reload the target AI tool so its MCP process uses the new binary.

### End-user skills

`wendy mcp setup` installs the same end-user skill group for detected Codex,
Claude Code, and OpenCode installations, including supporting references.
Rerun it after updating the CLI. It does not install Wendy engineering skills.

If the user installed the optional `wendy-agentic-coding` plugin instead, update
it through the assistant's plugin manager. Avoid installing both copies of the
same skills. Do not send end-users to the internal `claude-skills` collection.

## Common follow-up

For first-time ChatGPT users, explain Wendy in one sentence and return to
[wendy-onboarding](../wendy-onboarding/SKILL.md) after verifying installation.
Use `wendy mcp setup chatgpt` for the local desktop connection; it requires no
Cloud account or configured target. Hosted inspection and app control use the
existing registered plugin and do not require installing this CLI. A request
to get started with a local workflow includes ordinary CLI and MCP setup;
plugin installation alone does not.

If `wendy discover --json` finds no device, establish whether WendyOS/Agent was ever installed. Use `wendy-device-install` for a new board or robot; use `wendy-device-debug` for an existing installation that became unreachable. An empty scan by itself proves neither installation failure nor a network fault.
