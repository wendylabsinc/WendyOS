# Cloud Tunnel

The `wendy cloud tunnel` command forwards a local TCP or UDP port to an authorized
service on a cloud-enrolled device. Cloud v2 authorizes each port through its
service catalog; legacy sessions use the legacy broker. It listens on
`127.0.0.1` on your development machine and reaches the remote service through
the Cloud broker. Start with the
[Cloud v2 or Legacy tab](/docs/cloud) if you have not selected a session yet.

## Prerequisites

- Authenticated with the intended Cloud context. Use `wendy cloud login` for v2,
  or `wendy cloud login --legacy` for v1, then check `wendy cloud status`.
- At least one compute device enrolled and **online** in your organization.

## Usage

```sh
wendy cloud tunnel <local-port>:<remote-port>[/udp] [--cloud-grpc <endpoint>] [--device <id|name>]
```

For example, forward local port 15052 to the Wendy Agent service:

```sh
wendy cloud tunnel 15052:50052 --device shop-floor-01
```

This is a transport for an authenticated Agent client, not an HTTP endpoint.
Forwarding does not bypass the Agent's authentication. Keep the command running
while using it, then press Ctrl+C to close the forward.

Without a protocol suffix, the command uses TCP. Append `/udp` to forward UDP
datagrams. A single port uses that port at both ends.

Cloud v2 supports any TCP or UDP port from 1 through 65535 on the device's
loopback interface. For example:

```sh
wendy cloud tunnel 8080:3007 --device shop-floor-01
wendy cloud tunnel 9000:9000/udp --device shop-floor-01
```

Arbitrary ports require an updated Cloud service, plus CLI and agent
[2026.10.22-211023](https://github.com/wendylabsinc/WendyOS/releases/tag/2026.10.22-211023)
or newer. Older v2 versions support only TCP ports 22 and 50052. Legacy
sessions retain their existing TCP and UDP forwarding behavior.

Each v2 UDP session authorizes one remote port. Separate local peers use
separate flows, with up to 64 active flows and 16 new flows per second.
Empty datagrams are supported; the maximum payload is 65507 bytes.

Port 22 requires an SSH server on the target. Release Jetson and Raspberry Pi images do not
include one; listing `ssh` in the catalog does not make SSH available there.
Before moving a custom application port or UDP workflow from v1, test it on v2
with the updated versions. See [Moving to Cloud v2](/docs/cloud-migration).

The CLI:

1. Lists the online compute devices enrolled in your organization, up to a cap of **10,000 devices**. If more are returned, the command exits with an error: `cloud returned more than 10000 devices`.
2. Selects the target device:
   - When `--device` is set, matches it against device names (case-insensitive exact match). On a legacy session a plain integer is also accepted as the numeric asset ID, which is how devices enrolled without a name are reached; on a cloud (OIDC) session there is no numeric fallback — pass the device's name or its UUID.
   - When `--device` is unset and exactly one device is online, connects to it directly.
   - When `--device` is unset and more than one device is online:
     - In an **interactive terminal**, presents the cloud discover TUI in picker mode (`↑/↓` to navigate, `enter` to select, `u` to update a device before connecting, `q` to cancel).
     - In a **non-interactive environment**, exits with an error that enumerates available devices as `id=name` pairs (unnamed devices show as `(unnamed)`). Pass `--device <id|name>` to select one directly.
3. Starts the local listener and forwards traffic to the requested port on the selected device.

Only online devices are shown. To inspect enrolled but offline devices, use
[`wendy cloud discover --all`](../clients/wendy-cli/commands/cloud/discover.md).
Use `wendy cloud discover --json` to obtain selectors: UUIDs for Cloud v2,
numeric asset IDs for legacy Cloud, or device names in either environment.

## Reconnection and app uploads

For [app uploads over Cloud v2](../clients/wendy-cli/commands/run.md#uploads-over-cloud-v2),
a lost connection reports the device and upload progress across resume attempts.
Devices without a display name are identified by their Cloud ID. A failed upload
reconnect, or a connection loss while waiting for the app's start acknowledgement,
reports a retryable `device_unreachable` failure (exit 5). Check that the device
is online with `wendy cloud discover`, then retry. There is no registry fallback
over Cloud v2.

Device updates and legacy Cloud resume keep their existing restart-wait messages.

## Flags

| Flag | Description |
|------|-------------|
| `--cloud-grpc` | Override the cloud gRPC endpoint. Overrides session selection. When multiple sessions are stored and no default is set, an interactive terminal shows a session picker; a non-interactive environment errors. |
| `--device` | Target a specific device by name (case-insensitive exact match), by UUID, or — on legacy sessions only — by numeric asset ID. When omitted in a non-interactive context with multiple devices, the command exits with an error listing `id=name` pairs. |
| `--broker-url` | Override the tunnel broker host and port. **Legacy sessions only**; defaults to `WENDY_BROKER_URL` when set, otherwise the cloud endpoint's broker address. On a cloud (OIDC) session Cloud selects the authorized relay and passing this flag errors. |

## Related

- [Cloud Connectivity](./connectivity.md)
- [`wendy cloud discover`](../clients/wendy-cli/commands/cloud/discover.md)
- [Reach a UDP service through Wendy Cloud](/docs/guides/tutorials/python/cloud-udp-service)
