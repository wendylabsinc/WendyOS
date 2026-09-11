# `wendy cloud tunnel`

Forwards a local TCP or UDP port to a service on a cloud-enrolled device.

## Usage

```sh
wendy cloud tunnel <local-port>:<remote-port>[/udp] [flags]
```

## Description

`wendy cloud tunnel` fetches the list of **online** compute devices from Wendy Cloud and either connects directly when only one device is available, selects the device named or identified by `--device`, or prompts you to choose one interactively.

The local listener binds to `127.0.0.1`. Traffic is forwarded through the cloud broker to `127.0.0.1:<remote-port>` on the device. The remote service must be listening on that address or on all device interfaces.

The protocol defaults to TCP. Append `/udp` to forward UDP datagrams, or `/tcp` to select TCP explicitly. A single port, such as `9000/udp`, uses the same port locally and remotely. Ports must be between 1 and 65535.

Keep the command running while using the forward. Press Ctrl+C to close it; the remote service keeps running.

### Selecting a device

`--device` resolves a device in two steps:

1. **By name:** a case-insensitive exact match against device names. If the value matches more than one device, the command errors with `multiple devices match …; use a more specific name`.
2. **By numeric asset ID:** if no name matches and the value is a plain integer, it is treated as the device's numeric asset ID. This is how you target a device that was enrolled without a name. Run [`wendy cloud discover --json`](./discover.md) to list IDs.

If neither matches, the command errors with `no device named or with id "<value>" found; run 'wendy cloud discover --json' to list ids`.

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
| `--device` | `""` | Target a specific cloud device by **name** (case-insensitive exact match) or **numeric asset ID**. When omitted with multiple online devices, an interactive terminal shows the picker and a non-interactive environment errors with the available `id=name` pairs. |
| `--broker-url` | `WENDY_BROKER_URL`, otherwise the cloud endpoint's broker address | Override the tunnel broker host and port. |

## Examples

Forward local TCP port 8080 to device port 80, choosing a device interactively when needed:

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
