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

With a selected organization and a valid cached operator certificate,
`wendy discover` can automatically join that organization's Wendy Net from this
computer. It does not enroll the computer as a device or start a Cloud login or
credential renewal. Select an existing organization with `wendy auth use` if
needed. Mesh devices appear in Nearby; JSON output lists them under `meshDevices`
and reports discovery state under `meshDiscovery`.

Highlight a mesh row and press Enter to copy its details. Use its
`mesh://TENANT_UUID/DEVICE_UUID` selector with device commands:

```sh
wendy discover --type mesh
wendy device info --device mesh://TENANT_UUID/DEVICE_UUID
wendy run --device mesh://TENANT_UUID/DEVICE_UUID
```

A unique device name also works with `--device <name>`. Use the full selector if
names are ambiguous. Connections authenticate the exact destination device and
organization end to end, independently of intermediate routers. Devices appear
only while they have a current signed announcement and an authorized route.
An announcement does not prove that the target's application is ready.

Commands share a private background router. An automatic session can stop after
five minutes without command activity or transit traffic. To keep your computer
available as a router between commands, enable it explicitly:

```sh
wendy device wendy-net connect
```

Stop the current router and its connections with `wendy device wendy-net disconnect`.
Later requested discovery can start it again. Credential changes or expiry also
end a session. Each installation has an independent, persistent router identity,
so multiple computers can participate with the same operator account. Routing
stays inside the private session; it does not install host routes or change DNS.

The CLI discovers first-hop routers over LAN and Bluetooth, then follows routes
across the mesh. LAN mesh discovery requires an IPv4 interface and multicast
access. Routed LAN subnets can work when ordinary device discovery supplies
addresses, for example through a DNS-SD reflector between VLANs, and the agents
support routed operator links. USB Ethernet is included when the operating
system exposes an eligible interface. The router can switch paths and forward
across several hops; it does not combine link bandwidth.

On Linux, Bluetooth requires BlueZ and LE L2CAP support. On macOS it uses
CoreBluetooth and requires Bluetooth permission. The CLI does not discover
first hops through Wi-Fi Aware. A failed carrier does not prevent another from
connecting. Windows mesh code builds, but device interoperability has not been
qualified; ordinary discovery remains available.

First-hop routers need operator-capable agents or supported PSRAM Lite firmware;
older agents reject operator joins. If mesh discovery is unavailable, check the
selected organization, certificate and router version. Ordinary discovery
continues. See [Connect from your computer](/docs/advanced/development/local-mesh-radio)
for Lite limits, service forwarding and explicit first-hop selection.

## Platform support

### LAN (mDNS) discovery

mDNS discovery works on all platforms. On Linux, the CLI performs an mDNS browse
that requires UDP port 5353 open on the host firewall (e.g., `sudo ufw allow 5353/udp`).
On macOS, the CLI browses through mDNSResponder in-process and requires Local Network TCC permission.
For a USB-connected device on Linux, `wendy discover` notices when the host's
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
| `--type` | `all` | Local discovery type: `usb`, `lan`, `bluetooth`, `external`, `mesh`, or `all`. Does not affect the Cloud tab. |
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
