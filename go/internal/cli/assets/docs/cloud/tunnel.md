# Cloud Tunnel

The `wendy cloud tunnel` command forwards a local TCP or UDP port to a service on a cloud-enrolled WendyOS device. It listens on `127.0.0.1` on your development machine and reaches the remote port on the device's loopback address through the cloud broker.

## Prerequisites

- Authenticated with Wendy Cloud (`wendy auth login`).
- At least one compute device enrolled and **online** in your organization.

## Usage

```sh
wendy cloud tunnel <local-port>:<remote-port>[/udp] [--cloud-grpc <endpoint>] [--device <id|name>]
```

For example, forward local TCP port 8080 to port 80 on the device:

```sh
wendy cloud tunnel 8080:80 --device shop-floor-01
```

Use `/udp` for a UDP service:

```sh
wendy cloud tunnel 9000:9000/udp --device shop-floor-01
```

Without a protocol suffix, the command uses TCP. A single port such as `9000/udp` uses that port at both ends. Keep the command running while using the service, then press Ctrl+C to close the forward. Closing it does not stop the remote app.

The CLI:

1. Lists the online compute devices enrolled in your organization, up to a cap of **10,000 devices**. If more are returned, the command exits with an error: `cloud returned more than 10000 devices`.
2. Selects the target device:
   - When `--device` is set, matches it against device names (case-insensitive exact match) and falls back to treating a plain integer as the numeric asset ID. This also supports devices enrolled without a name.
   - When `--device` is unset and exactly one device is online, connects to it directly.
   - When `--device` is unset and more than one device is online:
     - In an **interactive terminal**, presents the cloud discover TUI in picker mode (`↑/↓` to navigate, `enter` to select, `u` to update a device before connecting, `q` to cancel).
     - In a **non-interactive environment**, exits with an error that enumerates available devices as `id=name` pairs (unnamed devices show as `(unnamed)`). Pass `--device <id|name>` to select one directly.
3. Starts the local listener and forwards traffic to the requested port on the selected device.

Only online devices (those with an active broker presence) are shown. If you need to inspect enrolled-but-offline devices, use [`wendy cloud discover --all`](../clients/wendy-cli/commands/cloud/discover.md). Run [`wendy cloud discover --json`](../clients/wendy-cli/commands/cloud/discover.md) to list the numeric asset IDs you can pass to `--device`.

## Flags

| Flag | Description |
|------|-------------|
| `--cloud-grpc` | Override the cloud gRPC endpoint. Overrides session selection. When multiple sessions are stored and no default is set, an interactive terminal shows a session picker; a non-interactive environment errors. |
| `--device` | Target a specific device by name (case-insensitive exact match) or numeric asset ID. When omitted in a non-interactive context with multiple devices, the command exits with an error listing `id=name` pairs. |
| `--broker-url` | Override the tunnel broker host and port. Defaults to `WENDY_BROKER_URL` when set, otherwise the cloud endpoint's broker address. |

## Related

- [Cloud Connectivity](./connectivity.md)
- [`wendy cloud discover`](../clients/wendy-cli/commands/cloud/discover.md)
- [Reach a UDP service through Wendy Cloud](/docs/guides/tutorials/python/cloud-udp-service)
