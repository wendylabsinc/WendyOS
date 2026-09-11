# `wendy cloud discover`

Lists enrolled compute devices visible to the authenticated Wendy Cloud organization.

## Usage

```sh
wendy cloud discover [flags]
```

## Description

`wendy cloud discover` queries the Wendy Cloud asset service for devices registered in your organization. By default **only online devices** (those with an active broker presence) are returned. Pass `--all` to include devices that are currently offline.

In an interactive terminal the command launches a **live TUI** that refreshes the device list every 10 seconds and fetches the running agent version for each device concurrently. In non-interactive environments (pipes, CI, or when `--json` is passed) a JSON array is written to stdout instead.

When no devices match the filter, a contextual message is shown:

- **Default (online only):** `No online devices found. Use --all to include offline devices.`
- **With `--all`:** `No enrolled devices found.`

## Interactive TUI

The TUI displays a table with the following columns for each device:

| Column | Description |
|--------|-------------|
| Name | Device name |
| Type | Hardware device type |
| Version | Running agent version (fetched live; `—` while loading) |

The device's IP address is not shown as a column — cloud devices are reached by
name/ID through the broker tunnel. The address is still included in the
clipboard/JSON output (see below).

### Keyboard shortcuts

| Key | Action |
|-----|--------|
| `↑` / `↓` | Navigate the device list |
| `enter` | Copy the selected device's info as JSON to the clipboard |
| `a` | Copy all listed devices as JSON to the clipboard |
| `u` | Update the selected device's agent binary to the latest release |
| `q` / `Ctrl+C` | Quit |

### In-place device update (`u`)

Pressing `u` on a highlighted device downloads the latest agent release from GitHub, uploads it to the device via the broker tunnel, and waits for the agent to restart (up to 60 seconds). A status message is shown during the update and the version column refreshes automatically on completion.

## JSON output

When run non-interactively (output is piped) or with `--json`, a JSON array is written to stdout. Each element contains:

| Field | Type | Description |
|-------|------|-------------|
| `id` | string or integer | UUID string for principal-based Cloud v2 sessions; numeric asset ID for legacy v1 sessions (omitted when zero). |
| `name` | string | Device name (may be empty for devices enrolled without a name). |
| `type` | string | Human-readable hardware device type. |
| `address` | string | IP address reported by the cloud. |
| `version` | string | Running agent version. Omitted when it could not be determined. |

Principal-based sessions use the Cloud v2 API for interactive discovery and JSON
output, including when output is piped. Clipboard copies preserve device UUIDs,
and interactive version probes, updates, and picker selections use v2 tunnels.
The standalone `cloud tunnel --device` command still resolves legacy numeric IDs.

Example:

```json
[
  {"id": 42, "name": "playful-reed", "type": "Raspberry Pi 5", "address": "192.168.1.10", "version": "0.10.4"},
  {"id": 43, "name": "", "type": "Raspberry Pi 5", "address": "192.168.1.11"}
]
```

The `id` field is the primary mechanism for addressing a device that was enrolled without a name — pass it to `wendy cloud tunnel --device <id>`.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--all` | `false` | Include offline devices in the results. When omitted only devices with an active broker presence are shown. |
| `--broker-url` | `$WENDY_BROKER_URL` (or derived from cloud endpoint) | Tunnel broker `host:port`. When empty the CLI derives the address from the cloud gRPC endpoint. |
| `--cloud-grpc` | `""` | Cloud gRPC endpoint. Overrides session selection. When multiple sessions are stored and no default is set, an interactive terminal shows a session picker; a non-interactive environment errors. |

## Examples

Show only online devices (default):

```sh
wendy cloud discover
```

Show all enrolled devices, including offline ones:

```sh
wendy cloud discover --all
```

Point at a specific cloud gRPC endpoint:

```sh
wendy cloud discover --cloud-grpc grpc.cloud.wendy.dev:443
```

Output JSON (non-interactive / scripting):

```sh
wendy cloud discover --json
# or pipe to force non-interactive mode:
wendy cloud discover | cat
```

## See also

- [`wendy cloud tunnel`](./tunnel.md) — open a tunnel to a discovered device.
