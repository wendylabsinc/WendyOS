# `wendy discover`

Discovers local and Wendy Cloud devices in a tabbed terminal interface.

## Usage

```sh
wendy discover [flags]
```

## Description

Without output or timeout flags, `wendy discover` opens a live TUI with three
tabs:

- **Nearby** discovers devices over LAN, USB, Bluetooth, and external providers.
- **Simulator** lists local simulator devices.
- **Cloud** lists online devices enrolled in the active Wendy Cloud
  organization.

Use `tab` or `shift+tab` to switch tabs. Cloud discovery starts lazily on the
first visit to the Cloud tab, so the command makes no cloud connection if you
only use Nearby discovery.

Local discovery combines these mechanisms and merges their results:

- **Ethernet (USB NCM) discovery** — enumerates host network adapters and
  returns those whose name or interface description contains "wendy"
  (case-insensitive).
- **LAN discovery** — uses mDNS/Bonjour to find WendyOS devices and Headless Mac targets advertising themselves on the local network.

## Platform support

### LAN (mDNS) discovery

mDNS discovery works on all platforms. On Linux, the CLI performs an mDNS browse
that requires UDP port 5353 open on the host firewall (e.g., `sudo ufw allow 5353/udp`).
On macOS, the CLI browses through mDNSResponder in-process and requires Local Network TCC permission.
For USB-connected devices on Linux, run `wendy device usb-setup` first to bring up
the interface.

Headless Mac advertises the same `_wendyos._udp` service. When discovery
succeeds, Mac agents appear under `lanDevices` in JSON output with
`"os": "darwin"`. For automation, prefer an explicit target such as
`--device {hostname}:50051`, because discovery can be blocked by network policy
or macOS permissions.

## Local run targets

By default `wendy discover` hides **local run targets** — this machine,
Docker/OrbStack, and Apple Container — so the table shows only separate WendyOS
devices. Set `WENDY_SHOW_LOCAL_DEVICES=1` to include them:

```sh
WENDY_SHOW_LOCAL_DEVICES=1 wendy discover
```

> **Note:** JSON output (`wendy discover --json`) always includes local run
> targets regardless of `WENDY_SHOW_LOCAL_DEVICES`, so scripts and MCP callers
> continue to receive the full set.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--type` | `all` | Local discovery type: `usb`, `lan`, `bluetooth`, `external`, or `all`. Does not affect the Cloud tab. |
| `--timeout` | `5s` | Scan local devices once for this duration, print the results, and exit. When omitted, the live tabbed TUI runs until quit. |
| `--json` | `false` | Output results as a JSON array instead of a table |

## Environment variables

| Variable | Description |
|----------|-------------|
| `WENDY_SHOW_LOCAL_DEVICES` | When truthy (`1`/`true`/`yes`/`on`), include local run targets (this machine, Docker/OrbStack, Apple Container) in the table. JSON output always includes them regardless. |

## Interactive TUI

Without `--json` or an explicitly set `--timeout`, discover renders Nearby, Simulator, and
Cloud tables that refresh as devices come and go. A leading `✦` marks the
current default device or organization.

### Keyboard shortcuts

| Key | Action |
|-----|--------|
| `tab` / `shift+tab` | Switch between Nearby, Simulator, and Cloud tabs |
| `↑` / `↓` | Navigate the active device list |
| `enter` | Copy the selected device as JSON; when logged out in Cloud, start login |
| `a` | Copy all devices in the active tab as JSON |
| `u` | Update the selected device's agent |
| `d` / `x` | Set or clear the default device in Nearby |
| `o` | Switch the active organization in Cloud |
| `q` / `Ctrl+C` | Quit |

### Nearby tab

The Nearby tab shows devices found through the enabled local discovery
transports. The `--type` flag filters this tab only.

### Cloud tab

The Cloud tab shows online, cloud-enrolled devices for the active organization.
Its header includes the organization name and ID and marks the persisted
default organization with `✦ default`.

If no cloud credentials are stored, the Cloud tab displays `Wendy Cloud login —
Not logged in`. Press `enter` to start browser-based login; after login succeeds,
discovery restarts with the Cloud tab available. Press `tab` to return to Nearby
or `q` to quit without logging in.

### Switching organizations (`o`)

Press `o` in the Cloud tab to choose from all organizations available through
the stored sessions, including organizations for which this machine does not
yet have credentials.

- An organization with stored credentials becomes active immediately.
- An organization without local credentials starts browser-based login against
  its cloud environment. After login, the CLI verifies that credentials for the
  selected organization were stored; if not, it reports an error and you can
  repeat the flow.

### Nearby table columns

| Column | Description |
|--------|-------------|
| Name | Device display name |
| Type | Transport(s) the device was discovered on (LAN, USB, BLE, …) |
| Address | IP address (or hostname) and port |
| Agent | Running agent version; `⚠` marks an agent older than the CLI; blank when the metadata probe hasn't succeeded |
| OS | OS version reported by the agent |
| Provisioned | `Provisioned` or `Unprovisioned` for LAN devices, from the mDNS-advertised mTLS state; blank for transports that don't report it (BLE-only, USB, external providers) |

### No-access hint

When the highlighted row is a provisioned LAN device whose agent metadata could
not be read — the signature of an unprovisioned CLI, or one logged in with
credentials that don't have access to the device — an amber hint appears below
the table:

```
⚠  This device is provisioned and this CLI does not have access, so agent details cannot be read. Run 'wendy auth login' with an account that can access it.
```

The hint clears automatically once a probe succeeds (for example, after
`wendy auth login` with an authorized account). If a version is already known
from an earlier successful probe or another transport, it stays in the table
and the hint is suppressed.

### Clipboard JSON

Pressing `enter` copies the highlighted device as a JSON object; `a` copies all
devices as a JSON array. Each object contains:

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Device display name |
| `type` | string | Transport(s), e.g. `LAN` or `USB, LAN` |
| `usb` | string | USB interface summary (omitted when not connected over USB) |
| `address` | string | IP address (or hostname) and port |
| `version` | string | Agent version (omitted when unknown) |
| `provisioned` | string | `Provisioned` or `Unprovisioned` for LAN devices (omitted for other transports) |
