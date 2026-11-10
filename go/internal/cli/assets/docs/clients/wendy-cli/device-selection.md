Many CLI operations run on the remote device. When a connection is to be
established, the CLI goes down a list in order to connect to this device.

For interactive examples, start with `wendy run` or `wendy device info` and choose a target when prompted. The picker is a fallback: `WENDY_DEVICE` may already select a device. Check the displayed target.

Use `--device` when scripting, running without a terminal, or deliberately addressing several devices. The sections below document selection precedence, not required setup steps.

## 1. `--device`

If the `--device` flag is specified, a connection is made against that target IP
address or hostname. Include a port when the target should not use the default
agent port:

```sh
wendy --device 192.168.1.42 device apps list
wendy run --device my-mac.local:50051
```

Failing to connect to an explicit device results in a failure.

A value made only of digits, such as `283`, is rejected: it is not a hostname or IP address, and the system resolver would turn it into an unrelated address. If it is a Wendy Cloud asset ID, use its cloud selector (`--device cloud://<cloud-grpc-host:port>/org/<org-id>/asset/283`) or `wendy cloud device <command> --device 283`. The error prints the exact selector when you are logged in.


## 2. `WENDY_DEVICE`

If `--device` is not given and the `WENDY_DEVICE` environment variable is set, the CLI connects to that device exactly as if it had been passed with `--device`. Use it to give one terminal, script or AI session its own target:

```sh
WENDY_DEVICE=my-pi.local wendy run
```

`--device` still takes precedence. A blank value is ignored.

## 3. Show Picker

The picker finds nearby [WendyOS](/docs/installation), Wendy Agent and
[Wendy Lite](/docs/advanced/wendy-lite/) devices using local-network and Bluetooth discovery.
Finding a device here is separate from enabling
[Wendy Net Bluetooth connections](/docs/hardware/bluetooth#wendy-net-over-bluetooth)
between devices.
A device picker is shown only when the terminal is interactive, so a user can
select their target device for the current command invocation.

Cached LAN devices stay hidden until their agent answers a probe or mDNS
resolves their service during the current scan.

When logged into Wendy Cloud, press `e` on a highlighted Local device to run
the `wendy device enroll` flow using the active cloud account. Enrollment
requires a LAN or USB network connection. After enrollment, the picker reopens;
press Enter to select a device for the original command.

Headless Mac advertises over Bonjour/mDNS as `_wendyos._udp` and appears
as a LAN device when local discovery is allowed by the network and macOS Local
Network permissions.

The picker table shows the same columns as
[`wendy discover`](./commands/discover.md#interactive-tui). The leading
marker column displays provisioned state (`●`/`○`) for LAN devices. If the CLI cannot verify the highlighted device's agent,
a footer hint explains what went wrong and what to try next.

In scripts, CI, SSH sessions without a TTY, or any other non-interactive
context, no picker is shown. Pass `--device` or set `WENDY_DEVICE` before
running commands that need a target.

> If the terminal is interactive, a prompt will be made to update the device right now.

## Device identity

Every enrolled-device connection needs a trusted expected device identity.
The CLI checks the certificate against the selected device, including its
organisation and asset or device ID. Older organisation/asset certificate
identities remain supported. This applies to LAN, Bluetooth, Cloud tunnels,
and MCP connections.

Cloud connections use the selected device's authenticated inventory record.
For local connections, the CLI needs a saved binding for the selected name.
`wendy cloud discover` refreshes bindings under the inventory's device names;
verified enrollment also saves a binding under the selected hostname and
enrollment name. Finding a nearby device or resolving its address does not
establish its identity. An IP address is a route to the device, and a different
LAN or Bluetooth name needs its own trusted binding.

If the CLI reports that the expected device identity is missing:

- For an enrolled Cloud device, sign in to its organisation and run
  `wendy cloud discover`, then select its inventory name or Cloud selector.
  Refreshing inventory does not bind an arbitrary local alias or IP address.
- For a new or reset, unprovisioned device, use `wendy device enroll` over its
  LAN or USB network connection. This explicit setup flow verifies the enrolled
  identity before saving the binding.
- If the device is already enrolled under a different local name, use a name
  with an existing trusted binding or its Cloud selector.

Retrying, refreshing client certificates, and synchronizing the device clock
do not supply a missing device identity. The CLI does not retry this failure
with weaker certificate checks.

Routine certificate renewal is accepted when the device identity stays the
same. A different organisation, issuing Cloud, asset, or device identity is
refused. A previously pinned device that no longer presents an enrolled
identity is also refused. These checks apply in interactive, `--json`, and
non-interactive runs, without a trust override prompt.

`wendy device unpin <hostname>` clears saved trust for an intentional reset or
replacement. It does not authorize learning an identity from the next
connection: refresh the trusted inventory binding, or complete enrollment if
the device is unprovisioned. Keep the pin and verify the device if the change
was unexpected.

Loopback addresses share the pin of their bare host, whatever the port:
`127.0.0.1:50051` and `127.0.0.1:50061` are both checked against the
`127.0.0.1` pin, and `localhost:50051` against the `localhost` pin. The
exception is a running local VM: `127.0.0.1` on its forwarded agent port, or on
the mTLS port after it, is that VM, pinned as `vm:<name>` like
`--device vm:<name>`, so each VM keeps its own pin. `wendy device unpin
vm:<name>` clears only that VM's pin, never the shared `127.0.0.1` pin.

## Simulator startup and authentication failures

While waiting for a simulator to boot, the CLI retries connection failures
such as an agent that is not yet listening. It stops immediately if the
connection fails because of a TLS authentication rejection, a device identity
refusal, a [missing expected device identity](#device-identity), an organization
mismatch, or a gRPC `Unauthenticated` or `PermissionDenied` response.

A TLS authentication failure can mean a wrong device clock or incompatible
credentials. If the device clock may be wrong, run `wendy device sync-time`
on the same LAN, then retry. Refresh certificates when the CLI reports expired
client credentials, after checking this computer's clock. Use
`WENDY_TLS_DEBUG=1` for connection details.

Authentication failures omit the simulator's boot-console output. Ordinary
startup timeouts still include the available boot-console tail to help
diagnose guest startup problems.

## Automatic connection recovery

After a possible clock-related TLS failure, the CLI broadcasts a signed time
proof on the LAN and retries the connection, sending the proof again before
each of three retries over about 10 seconds. This applies to a device named
with `--device` or `WENDY_DEVICE`, and to a device chosen in the picker.
Nothing acknowledges the proof, so the CLI counts it as accepted only when the
device accepts the connection. If the device still rejects it, the CLI says so
and names the next step: give the device internet access so it sets its own
clock, or, when no time server answered and the CLI sent a saved proof, give
this computer internet access.

A device's certificate names the organization it belongs to. If your login for
that organization has expired, the CLI says so and does not send a time proof:
the device clock is not the problem. A login from `wendy auth login` renews
itself and the CLI retries the connection once. If it cannot renew, run
`wendy auth login` and sign in to that organization, then retry.

The CLI offers to refresh certificates when it has evidence that the client
credentials need renewal. A timeout or generic TLS rejection alone does not
trigger that offer.

`wendy mcp serve` keeps its device connection for as long as it runs and
checks it every 30 seconds. When the connection's certificate has expired and
the connection has dropped, for example after a device reboot, the server
connects to the same device again with the stored login. Before it reconnects
a LAN or USB device, it renews an expired login from `wendy auth login`, once
per connection. A Cloud device's connection renews its own login. While the
device does not answer, the server keeps the connection and tries again at
each check. If the device refuses the login, or no current login can be
offered (a service-account login, or a renewal that failed), the server closes
the connection and its tools report that no device is connected: run
`wendy auth login`, then connect again. The server never reconnects a
simulator this way, because connecting to a simulator starts it.

## Local Targets

The picker can also show local provider targets when the host supports them.
These are not WendyOS devices.

### Docker

Use Docker for local container runs. Dockerfile, Containerfile, and Compose projects run
through the local Docker daemon. On macOS and Windows, Docker runs Linux
containers inside Docker's Linux environment rather than as native macOS or
Windows processes.

Use this target when you want to test container build and runtime behavior
without deploying to WendyOS hardware. Hardware-specific WendyOS behavior, device
entitlements, and target filesystem assumptions still need a real WendyOS target.

You can select it directly with:

```sh
wendy run --device docker
```

### Apple Container

On Apple silicon Macs, Wendy can use Apple's `container` CLI for local
Dockerfile and Containerfile runs without Docker Desktop:

```sh
wendy run --device apple-container
```

Wendy automatically checks for the `container` CLI and offers to install it via Homebrew if missing, and starts the `system` and `builder` services if they are not running.

Use this target when you want to build and run a single Dockerfile or
Containerfile project with Apple's lightweight Linux container runtime. Compose
projects still require the Docker target.

To deploy to a WendyOS device while using Apple Container only as the image
builder, keep `--device` pointed at the WendyOS device. On Apple silicon Macs,
Apple Container is tried first by default when it is installed and running, then
Docker is used as a fallback. Set `--builder apple-container` to require Apple
Container, or `--builder docker` to force Docker:

```sh
wendy --device my-wendy.local run --builder apple-container
```

### Local

Use the local target for host-native apps. The app runs directly on the computer
that is running the `wendy` CLI:

- On macOS, it runs as a macOS process.
- On Windows, it runs as a Windows process.
- On Linux, it runs as a Linux process.

The local target is intended for native Swift, Go, and Python projects. It does not
run inside Docker's Linux environment, does not emulate WendyOS, and does not
provide WendyOS container semantics, hardware entitlements, or device filesystem
layout.

You can select it directly with:

```sh
wendy run --device local
```
