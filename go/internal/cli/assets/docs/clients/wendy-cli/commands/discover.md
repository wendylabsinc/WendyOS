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

## Wendy Net mesh discovery

With a selected organization and a valid operator certificate, `wendy discover`
automatically joins that organization's Wendy Net from this computer. Mesh devices
appear in Nearby and in `lanDevices` in JSON output with `"mesh": true`. Without an
organization operator identity, discovery does not start mesh carriers or prompt
for enrollment. Select an existing organization with `wendy auth use` if needed.

Select a mesh row to connect or make it the default. Its address is a stable
`mesh:<device-UUID>` selector, usable with existing commands:

```sh
wendy discover
wendy device info --device mesh:<device-UUID>
wendy run --device mesh:<device-UUID>
```

`mesh:<name>` also works when the name is unique. While the discovery session is
active, ordinary `--device <name>` resolves names in its authenticated mesh roster.
Connections retain end-to-end operator mTLS and pin the target's device identity.
Signed device announcements and their currently authorized routes determine the
roster. A signed announcement does not prove the target's application is ready.

The CLI shares one private background mesh session across commands. It stops
when idle for two minutes, when the selected organization or certificate changes,
or when its certificate chain expires. It reuses the existing operator key and
never enrolls the computer as a device. The IPv6 stack stays inside the session;
it does not install host routes, change DNS, or expose local services. The
operator endpoint is stable per operator principal, so use only one computer's
mesh session at a time for the same operator in the same organization.

Ethernet, infrastructure Wi-Fi, BLE, and Wi-Fi Aware providers run independently.
The router uses carrier costs and can forward across multiple hops and switch
paths when links fail. It does not combine link bandwidth. USB Ethernet is
included when the operating system exposes it as an eligible network interface.
LAN mesh discovery currently requires an IPv4 interface and multicast access.
Operator connections also work across routed LAN subnets when ordinary device
discovery supplies the addresses, for example through a DNS-SD reflector between
VLANs. Devices must run an agent that supports routed operator links. Every link
still verifies the exact same-organization device identity before joining.

On Linux, Wi-Fi Aware requires compatible hardware, the existing Wendy NAN
helper, and the permissions needed to manage the interface. BLE requires BlueZ
and LE L2CAP support. On macOS, BLE uses CoreBluetooth as a central and requires
Bluetooth permission. [macOS does not support Wi-Fi Aware](https://developer.apple.com/forums/thread/827887).
Windows retains ordinary discovery but does not join Wendy Net. A carrier that
cannot start does not prevent other carriers from connecting.

Mesh peers must run an agent version that admits operator mesh identities.
Older agents continue to accept devices but reject operator joins. Before joining,
the CLI uses the existing auth renewal and refresh workflow for the selected
operator session. If it cannot obtain a current certificate, it reports the auth
recovery command and leaves ordinary discovery running.

## Platform support

### LAN (mDNS) discovery

mDNS discovery works on all platforms. On Linux, the CLI performs an mDNS browse
that requires UDP port 5353 open on the host firewall (e.g., `sudo ufw allow 5353/udp`).
On macOS, the CLI browses through mDNSResponder in-process and requires Local Network TCC permission.
For a USB-C-connected device on Linux, `wendy discover` notices when the host's
link to it isn't configured yet and offers to set it up before scanning (it needs
sudo). The offer only appears in an interactive terminal; with `--json` or
without a terminal, `wendy discover` prints a notice on stderr instead.

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
| `--json` | `false` | Output discovery results as JSON instead of a table |

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
| `d` | Make the selected device the default; press it again on the default to clear it |
| `s` / `x` | Start or stop the selected VM in Simulator |
| `c` / `r` | Create a VM, or remove the selected one and its disk (asks first), in Simulator |
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

LAN devices appear after their mDNS service resolves or their agent answers a
probe. Cached devices are checked in the background and stay hidden until
confirmed during the current scan.

| Column | Description |
|--------|-------------|
| Name | Device display name |
| Type | Transport(s) the device was discovered on (LAN, USB, BLE, …) |
| Address | IP address (or hostname) and port |
| Agent | Running agent version; `⚠` marks an agent older than the CLI; blank when the metadata probe hasn't succeeded |
| OS | OS version reported by the agent |
| Provisioned | `Provisioned` or `Unprovisioned` for LAN devices, from the mDNS-advertised mTLS state; blank for transports that don't report it (BLE-only, USB, external providers) |

### Connection hints

If the CLI cannot verify a device's agent, select its row to see what went
wrong and what to try next.

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
| `probeFailure` | object (optional) | Details of a failed connection and suggested recovery steps |

When provided, `probeFailure` contains a `code` identifying the failure, a
`message` explaining it, and optional `next_steps` listing suggested actions.
The same object is included in `--json` output.
