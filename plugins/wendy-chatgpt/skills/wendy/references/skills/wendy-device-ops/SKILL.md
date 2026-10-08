---
name: wendy-device-ops
description: "Use for Wendy CLI device operations on WendyOS and Wendy Lite: discover devices, inspect hardware and software, configure WiFi, update the agent, or gather evidence before debugging."
---

# Wendy Device Ops Workflow

Use this for device operations that are not app build/run lifecycle. Access devices through Wendy CLI or MCP only. Do not use SSH. If Wendy cannot retrieve a needed fact, report the missing capability or failed check.

## Start with device info

Before asking the user for their board, firmware, OS, or agent version, inspect the device yourself:

```bash
wendy --json device info --device <target>
```

Reuse the target from the failing command or conversation, or omit `--device` to use the configured default. If the target is unknown, use discovery below and select the unambiguous match to the user's context. Ask only for an unresolved target choice or relevant facts the inspection could not retrieve.

## Discover and select a device

Bound discovery so agents do not hang:

```bash
wendy discover --json --type all --timeout 5s
```

Use explicit hostnames when possible:

```bash
wendy device set-default <hostname>
wendy device get-default
wendy device unset-default
```

Passing the hostname to `set-default` avoids the interactive picker. Change the default only when that is part of the user's task. For diagnostic commands, use `--device <target>` to select the device without changing the default.

## Inspect device state

Local CLI info:

```bash
wendy --json info
```

Hardware capabilities:

```bash
wendy --json device hardware list --device <hostname>
wendy --json device hardware list --category gpu --device <hostname>
wendy --json device hardware list --category camera --device <hostname>
wendy --json device hardware list --category audio --device <hostname>
```

Use the output to decide entitlements before guessing. For example, check `device hardware list --category camera` before adding camera-specific assumptions to an app.

## WiFi operations

Read-only checks:

```bash
wendy --json device wifi status --device <hostname>
wendy --json device wifi list --device <hostname>
```

Mutating commands:

```bash
wendy device wifi connect --ssid "<ssid>" --password "<password>" --device <hostname>
wendy device wifi disconnect --device <hostname>
wendy device wifi forget --ssid "<ssid>" --device <hostname>
wendy device wifi rank --ssid "<ssid>" --priority 10 --device <hostname>
wendy device wifi rank --order "Home,Office,Cafe" --device <hostname>
```

Do not echo or store WiFi passwords in issues, PRs, reusable docs, or final summaries. If a password was passed on the command line, avoid copying the full command into shared output.

## Agent update

Only update the agent when the user asks, when the device is clearly behind the CLI, or when a fix requires a newer agent. Prefer checking first:

```bash
wendy --json device info --check-updates --device <hostname>
```

Stable update:

```bash
wendy --json device update --device <hostname>
```

Nightly or local binary:

```bash
wendy --json device update --nightly --device <hostname>
wendy --json device update --binary ./path/to/wendy-agent --device <hostname>
```

## Routing

- Use `wendy-app-lifecycle` for build, run, logs, app start/stop/remove, and volumes.
- Use `wendy-device-debug` when the root cause spans CLI, agent, WendyOS image state, containerd, or app behavior.
- Use `wendy-mcp-setup` when the user wants AI tools to access Wendy device operations through MCP.
