---
name: wendy-device-ops
description: "Use for Wendy CLI or MCP device operations on WendyOS and Wendy Lite: discover devices, inspect hardware and software, configure WiFi, pair SensorLink sources, update the agent or OS, use cameras, Bluetooth or Cloud tunnels, or gather evidence before debugging."
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

Ordinary LAN discovery and direct device management work even when Wendy Net is
disabled on the device. Use the returned selector and advertised port; enrolled
WendyOS agents commonly use TLS on port 50052, while unprovisioned agents use
port 50051. On Linux, a USB-tethered device needs a one-time host setup: ask the user
to run `wendy discover` in a terminal and accept its USB setup prompt (it
needs sudo). An empty scan can also mean a board without WendyOS; use
`wendy-device-install` for that.

Use explicit hostnames when possible:

```bash
wendy device set-default <hostname>
wendy device get-default
wendy device unset-default
```

Passing the hostname to `set-default` avoids the interactive picker. Change the default only when that is part of the user's task. For diagnostic commands, use `--device <target>` to select the device without changing the default.

## Reach devices through Wendy Net

For a device that is reachable only through the mesh, use a terminal on the
user's intended computer to sign in and join:

```bash
wendy auth login
wendy device wendy-net connect
wendy discover --json --type mesh --timeout 5s
wendy --device mesh://TENANT_UUID/DEVICE_UUID device info
```

Use the exact `mesh://...` destination returned by discovery. The CLI discovers
nearby routers over LAN and Bluetooth automatically; do not ask the user for a
router address, router UUID, Bluetooth identifier or PSM. Supported WendyOS and
PSRAM Lite routers can connect the computer; a C6 is a destination leaf, not an
entry router. A Lite destination supports its own management commands, not
WendyOS-only container operations.

With local MCP, `device_list` can list mesh destinations while the local
background router is connected. Use `device_connect(device=...)` for a returned
WendyOS mesh selector. `scan=true` adds ordinary LAN discovery; it does not
enable mesh participation. Use the CLI for Lite operations not exposed by the
connected MCP server. If no terminal is available on the intended computer,
explain the missing access rather than inventing a mesh-join tool.

To inspect the existing router on this computer, use
`wendy --json device wendy-net topology --local-cli`. Without `--local-cli`,
`topology` inspects the selected device instead. Device mesh status can be active
through Bluetooth while LAN is still waiting; verify the destination with a real
device command rather than inferring reachability from a status label.

`device_disconnect` ends the current MCP device connection. To stop the local
mesh router itself, use `wendy device wendy-net disconnect` when requested.
After that, discovery and device commands must leave it disabled until the user
explicitly reconnects with `wendy device wendy-net connect`. Ordinary LAN device
management remains available.

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

## SensorLink pairing

Pair a SensorLink source, such as an ESP32 camera, with the consuming device only when the user asks:

```bash
wendy --json device pair --device <hostname> --list
wendy --json device pair --device <hostname> --sensorlink "<source>"
wendy --json device pair --device <hostname> --forget "<source>"
```

Name the source by its name, asset ID or principal URI; `--forget` takes the pairing name `--list` shows. When none or several match, the error lists candidates; ask the user instead of guessing.

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

## With MCP tools

The Wendy MCP server covers the same operations on its connected device. Enable
the groups named below with `wendy_tools(groups=[...])` first; each call replaces
the enabled set, so list every group you still need.

- Inspect (core): `device_info` (versions, storage, battery) and
  `hardware_capabilities(category=...)`.
- Default device (`setup`): `device_set_default(address=...)`.
- WiFi (`setup`): `wifi_status`, `wifi_list`, `wifi_known_networks`,
  `wifi_connect(ssid=..., password=...)` and `wifi_disconnect`. Connecting drops
  the current network, so the session may lose the device. Forget and rank have
  no MCP tool; use the CLI.
- Updates (`setup`): `device_update_agent` (`nightly`, `binary_path`) updates only
  the agent, verifies it and reconnects. `os_update` starts an OS update;
  `os_update_status` reports whether it committed or rolled back, and why.
- Cameras and Bluetooth (`hardware`): `camera_list`, `camera_controls`,
  `camera_set_control`, `camera_snapshot(device_id=...)` (activates the camera,
  so ask first), `bluetooth_scan`, `bluetooth_connect` and `bluetooth_disconnect`.
- Cloud (`cloud`): `cloud_discover`, `cloud_connect`, `cloud_ping`, and
  `cloud_tunnel(local_port=..., remote_port=...)` with `cloud_tunnel_list` and
  `cloud_tunnel_close(id=...)` for port forwards owned by this server.
- Enrollment (`setup`): `cloud_enroll_device` uses the current Cloud login;
  `provisioning_start` needs a supplied enrollment token; `provisioning_status`
  checks the result.

## Routing

- Use `wendy-app-lifecycle` for build, run, logs, app start/stop/remove, and volumes.
- Use `wendy-device-debug` when the root cause spans CLI, agent, WendyOS image state, containerd, or app behavior.
- Use `wendy-mcp-setup` when the user wants AI tools to access Wendy device operations through MCP.
