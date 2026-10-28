Many CLI operations run on the remote device. When a connection is to be
established, the CLI goes down a list in order to connect to this device.

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

> **TODO (test)**: If the target device is outdated, and `--json` is not specified, a warning will be printed to indicate an update is available.

## 2. `WENDY_DEVICE`

If `--device` is not given and the `WENDY_DEVICE` environment variable is set, the CLI connects to that device exactly as if it had been passed with `--device`. Use it to give one terminal, script or AI session its own target:

```sh
WENDY_DEVICE=my-pi.local wendy run
```

`--device` still takes precedence. A blank value is ignored.

## 3. Show Picker

The picker finds nearby [WendyOS](/docs/#2-set-up-a-device), Wendy Agent and
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

> **TODO (test)**: If the target device is outdated, and `--json` is not specified, a warning will be printed to indicate an update is available.
> If the terminal is interactive, a prompt will be made to update the device right now.

## Device identity pinning

On the first successful connection to a named device, the CLI **pins** the
device's identity — the organisation and cloud host its TLS certificate belongs
to. On every later connection under that name, the CLI checks that the device
still presents that same organisation and cloud host.

A routine certificate **renewal or re-enrollment** keeps the same organisation
and cloud, so it is accepted silently. A change of organisation or cloud host —
which can indicate a man-in-the-middle or a swapped device — **refuses the
connection**. So does a device that was pinned while enrolled but now answers
with no identity at all, which means it was reflashed, factory reset, or
replaced by something squatting its name.

Both refusals are **unconditional**: they read identically in interactive,
`--json`, and non-interactive runs, and there is deliberately **no "trust this
anyway?" prompt** — a man-in-the-middle warning that can be dismissed gets
dismissed. Clearing the pin is a separate, deliberate act:

```sh
wendy device unpin <host>
```

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
refusal, an organization mismatch, or a gRPC `Unauthenticated` or
`PermissionDenied` response.

A TLS authentication failure can mean a wrong device clock or incompatible
credentials. If the device clock may be wrong, run `wendy device sync-time`
on the same LAN, then retry. Refresh certificates when the CLI reports expired
client credentials, after checking this computer's clock. Use
`WENDY_TLS_DEBUG=1` for connection details.

Authentication failures omit the simulator's boot-console output. Ordinary
startup timeouts still include the available boot-console tail to help
diagnose guest startup problems.

## Automatic connection recovery

After a possible clock-related TLS failure, the CLI attempts to broadcast a
signed time proof on the LAN and retries the connection. It reports whether
sending failed and whether the retry connected; sending a proof does not confirm
that the device received it or changed its clock.

The CLI offers to refresh certificates when it has evidence that the client
credentials need renewal. A timeout or generic TLS rejection alone does not
trigger that offer.

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
