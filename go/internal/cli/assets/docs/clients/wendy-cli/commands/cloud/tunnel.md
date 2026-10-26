# `wendy cloud tunnel`

Forwards a local TCP or UDP port to a service on a cloud-enrolled device. Cloud
v2 forwards any port with updated Cloud, CLI, and Agent versions; legacy
sessions use the legacy broker.

## Usage

```sh
wendy cloud tunnel <local-port>:<remote-port>[/udp] [flags]
```

## Description

`wendy cloud tunnel` fetches the list of **online** compute devices from Wendy Cloud and either connects directly when only one device is available, selects the device named or identified by `--device`, or prompts you to choose one interactively.

The local listener binds to `127.0.0.1`. Traffic is forwarded through the cloud broker to `127.0.0.1:<remote-port>` on the device. The remote service must be listening on that address or on all device interfaces.

The protocol defaults to TCP. Append `/udp` to forward UDP datagrams, or `/tcp`
to select TCP explicitly. A single port uses the same port locally and remotely.
Ports must be between 1 and 65535.

### Cloud v2 ports

Cloud v2 forwards TCP or UDP to any device port with an updated Cloud service
and CLI and Agent [2026.10.22-211023](https://github.com/wendylabsinc/WendyOS/releases/tag/2026.10.22-211023)
or newer. Older v2 versions forward only TCP port 50052 for Wendy Agent and TCP
port 22 for SSH. Cloud selects the relay, so `--broker-url` is not accepted for
v2 sessions. Port 22 requires an actual SSH server; release Jetson and Raspberry
Pi images do not include one.

Use the [Cloud guide](/docs/cloud) to choose the Cloud v2 or Legacy workflow.
See the [tunnel reference](/docs/advanced/cloud/tunnel) for UDP limits and
catalog errors, and [Moving to Cloud v2](/docs/cloud-migration) before moving an
app-port integration.

Keep the command running while using the forward. Press Ctrl+C to close it; the remote service keeps running.

### Selecting a device

Use a device name or UUID for Cloud v2. Legacy sessions also accept a numeric
asset ID. Run [`wendy cloud discover --json`](./discover.md) in the intended
context to obtain the selector. Numeric IDs from a v1 fleet do not identify
devices in v2.

When `--device` is omitted and multiple devices are online:

- In an **interactive terminal**, the cloud discover TUI opens in picker mode.
- In a **non-interactive environment** (pipe/CI), the command exits with an error that enumerates the candidates as `id=name` pairs (unnamed devices show as `(unnamed)`), so you can re-run with a working `--device`:

  ```
  multiple cloud devices found; rerun with --device <id|name> (42=playful-reed, 43=(unnamed))
  ```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--cloud-grpc` | `""` | Cloud gRPC endpoint. Overrides all session selection. When multiple sessions are stored and no default is set, an interactive terminal shows a session picker; a non-interactive environment errors. |
| `--device` | `""` | Device name or v2 UUID; numeric asset IDs are legacy-only. With multiple online devices, an interactive terminal shows the picker and a non-interactive call requires a selector. |
| `--broker-url` | `WENDY_BROKER_URL`, otherwise the cloud endpoint's broker address | Legacy-only broker override. Cloud v2 selects its authorized relay. |

## Examples

### Cloud v2

Sign in, check the active context, and forward the Wendy Agent service:

```sh
wendy cloud login
wendy cloud status
wendy cloud tunnel 15052:50052 --device playful-reed
```

An authenticated Agent client can use this transport. It is not a browser URL
and does not bypass the service's authentication.

With the updated versions above, forward an application port or a UDP service:

```sh
wendy cloud tunnel 8080:3000 --device playful-reed
wendy cloud tunnel 9000:9000/udp --device playful-reed
```

### Legacy

Select an existing v1 session with `wendy cloud login --legacy` and check
`wendy cloud status`. If needed, choose its saved context with
`wendy auth use <legacy-context-name>`.

Forward local TCP port 8080 to an existing HTTP service on device port 80:

```sh
wendy cloud tunnel 8080:80
```

Target a device by name:

```sh
wendy cloud tunnel 8080:80 --device playful-reed
```

Target an unnamed device by its numeric asset ID (from `wendy cloud discover --json`):

```sh
wendy cloud tunnel 8080:80 --device 43
```

Forward UDP port 9000:

```sh
wendy cloud tunnel 9000:9000/udp --device playful-reed
```

Check connectivity to the device's agent:

```sh
wendy cloud ping --device playful-reed --count 3
```

Cloud ping measures echo round trips through the broker to the agent. It requires no privileged ICMP sockets and does not test whether an application port is open.

## See also

- [`wendy cloud discover`](./discover.md): list available devices (supports `--all` to include offline devices)
- [Reach a UDP service through Wendy Cloud](/docs/guides/tutorials/python/cloud-udp-service): deploy an echo server and test a reply through the tunnel
