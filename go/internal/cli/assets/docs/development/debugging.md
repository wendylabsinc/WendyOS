# Debugging the wendy-agent

This page covers debugging the agent daemon itself — its startup, gRPC server, container management, and internal subsystems. For debugging *apps running on* a device (attaching a debugger to your Python or Swift container), see [apps/debugging](../apps/).

## Log Output

The agent uses `go.uber.org/zap` for structured logging. By default it emits production-format JSON logs.

### Enable verbose / development logging

Set `WENDY_DEBUG` to any non-empty value to switch to the development log format (human-readable, all levels including debug):

```sh
WENDY_DEBUG=1 ./wendy-agent
```

In production (systemd service), override the environment in `/etc/default/wendy-agent`:

```
WENDY_DEBUG=1
```

Then restart the service:

```sh
sudo systemctl restart wendy-agent
```

### Reading systemd journal logs

```sh
# Follow logs in real time
journalctl -u wendy-agent -f

# Show logs with timestamps since boot
journalctl -u wendy-agent -b

# Show the last 200 lines
journalctl -u wendy-agent -n 200
```

### Internal telemetry log stream

The agent tees its own `zap` logger into the telemetry broadcaster, which means internal agent logs are forwarded through the OTEL telemetry stream alongside container logs. You can observe them via `wendy device telemetry-stream` from the CLI (requires the device to be provisioned).

## Environment Variables

All environment variables are read at startup. Restart the agent after changing them.

| Variable | Default | Description |
|---|---|---|
| `WENDY_DEBUG` | _(unset)_ | Set to any value to enable development-mode logging (verbose, human-readable) |
| `WENDY_TLS_DEBUG` | _(unset)_ | Set to any value to log TLS handshake details for debugging mTLS connection issues |
| `WENDY_MDNS_DEBUG` | _(unset)_ | Set to any value to log mDNS query failures during discovery |
| `WENDY_MDNS_TIMEOUT` | `4s` | Timeout for mDNS browse fallback on Linux/Windows hosts (range: 1s–30s) |
| `WENDY_CONFIG_PATH` | `/etc/wendy-agent` | Directory for provisioning certificates and config |
| `WENDY_AGENT_HOST` | `::` | Host address for the plaintext gRPC server (Swift agent only; accepted values: `127.0.0.1`, `::1`, `localhost`) |
| `WENDY_AGENT_PORT` | `50051` | Port for the plaintext gRPC server (pre-provisioning only) |
| `WENDY_CONTAINERD_ADDR` | _(containerd default)_ | Unix socket path for the containerd client |
| `WENDY_REGISTRY_ADDR` | _(internal default)_ | Address for the embedded OCI registry |
| `WENDY_BROKER_URL` | _(derived from cloud host)_ | WebSocket URL for the cloud tunnel broker |
| `WENDY_OTEL_PORT` | `4317` | Port for the OTEL gRPC receiver |
| `WENDY_OTEL_HTTP_PORT` | `4318` | Port for the OTEL HTTP/protobuf receiver |
| `WENDY_MTLS_ORG_ENFORCEMENT` | `strict` | mTLS client organization enforcement on the device's mTLS gate: `off` (no org check), `grace` (reject any client cert whose organization differs from the device's own org, but allow legacy certs that carry no org identity), `strict` (as `grace`, and additionally reject certs that carry no org identity). Unrecognized values also default to `strict`. See note below. |

### `WENDY_MTLS_ORG_ENFORCEMENT` migration

The device derives its own organization from its provisioning certificate and rejects connecting client certificates from a different organization. The default mode is `strict`: client certificates that carry no org identity are rejected.

If your fleet has certs that predate org identity (no `urn:wendy:org:…` SAN URI and no org-bearing CN), set `WENDY_MTLS_ORG_ENFORCEMENT=grace` during the migration window. In `grace` mode, cross-org certs that *do* carry an org are still rejected, while org-less legacy certs are allowed and logged at WARN.

After user certs have rotated to carry the org SAN URI, remove the override and let the agent run in the default `strict` mode. If the device cannot determine its own org from its certificate, enforcement is disabled for safety and logged at ERROR.

An unrecognized value for `WENDY_MTLS_ORG_ENFORCEMENT` is treated as `strict` (fail-safe), and the agent logs a warning.

## gRPC Ports and Provisioning State

The agent runs different gRPC servers depending on provisioning state:

| Port | Protocol | When active |
|---|---|---|
| `50051` (default) | Plaintext gRPC | Before the device is provisioned |
| `50052` (default, `agentPort + 1`) | mTLS gRPC | After the device is provisioned |
| `4317` | OTEL gRPC | Always |
| `4318` | OTEL HTTP | Always |
| `/var/lib/wendy/agent-control/agent.sock` | Plaintext gRPC (unix socket) | Always; gated by `admin` entitlement mount. Appears inside an entitled container as `/run/wendy/agent/agent.sock` (`WENDY_AGENT_SOCKET`) |

Once a device is provisioned, the plaintext port is shut down with `GracefulStop()`. The local unix socket serves the full gRPC API with no authentication; access is gated by the `admin` entitlement which bind-mounts the socket into entitled containers only. If you are connecting to a provisioned device and getting connection-refused errors on port 50051, check whether the device is already provisioned (look for certificates in `WENDY_CONFIG_PATH`).

## mTLS Authentication

After provisioning, all gRPC traffic uses mutual TLS backed by ML-DSA (post-quantum) certificates issued by the Wendy PKI. The agent enforces `tls.RequireAnyClientCert` and verifies the chain with a custom `VerifyPeerCertificate` callback because Go's `crypto/x509` does not natively handle ML-DSA signatures.

If you see TLS handshake failures:
- Confirm the CLI has valid certificates: `wendy auth status`
- Re-fetch certificates: `wendy auth refresh-certs`
- Check that `WENDY_CONFIG_PATH` on the agent contains `cert.pem`, `chain.pem`, and `key.pem`

## gRPC Tracing

The standard Go gRPC environment variables work with the agent and CLI:

```sh
# Log all gRPC events (very verbose)
GRPC_GO_LOG_VERBOSITY_LEVEL=99 GRPC_GO_LOG_SEVERITY_LEVEL=info ./wendy discover
```

You can also use `grpcurl` to call individual agent RPCs directly (useful when the CLI behaviour is not what you expect):

```sh
# List services on an unprovisioned device
grpcurl -plaintext <device-ip>:50051 list

# Call GetDeviceInfo
grpcurl -plaintext <device-ip>:50051 wendy.agent.services.v1.WendyAgentService/GetDeviceInfo
```

For a provisioned device you need the mTLS certificates:

```sh
grpcurl \
  -cert ~/.config/wendy/cert.pem \
  -key  ~/.config/wendy/key.pem  \
  -cacert ~/.config/wendy/chain.pem \
  <device-ip>:50052 list
```

## Common Issues

### Agent fails to start: "Failed to connect to containerd"

The agent logs a warning (not a fatal error) if containerd is unavailable:

```
{"level":"warn","msg":"Failed to connect to containerd (container features will be unavailable)"}
```

Container-related RPCs will return errors, but the rest of the agent (network management, provisioning, OTA updates) continues to function. Ensure containerd is running:

```sh
sudo systemctl start containerd
sudo systemctl status containerd
```

If you are using a non-standard socket path, set `WENDY_CONTAINERD_ADDR`.

### Agent fails to start: "Failed to listen on agent port"

Port 50051 is already in use. Find what is holding it:

```sh
ss -tlnp | grep 50051
```

Override the port via `WENDY_AGENT_PORT` if necessary.

### xdg-dbus-proxy not found

```
{"level":"warn","msg":"xdg-dbus-proxy not found, Bluetooth containers will have unfiltered D-Bus access"}
```

Install `xdg-dbus-proxy` for proper D-Bus isolation inside Bluetooth containers:

```sh
sudo apt install xdg-dbus-proxy
```

The agent continues to work without it, but Bluetooth container isolation is degraded.

### Network manager not detected

The Go `wendy-agent` drives WiFi through `nmcli` only — it does not auto-detect
between ConnMan and NetworkManager, and it ignores `WENDY_NETWORK_MANAGER`
(that variable is a Swift-agent feature). If `nmcli` is not on the agent's
`PATH`, every WiFi management RPC fails with `Unavailable: WiFi management is
not available (nmcli not found)`. Check:

```sh
which nmcli && nmcli general status
```

### Device not visible via `wendy discover`

`wendy discover` uses mDNS (`.local` hostnames). On macOS, the CLI runner must have the Local Network TCC permission. If running in a CI or non-interactive context, use `ssh localhost` to get a session that holds the permission (see the `integration-tests.yml` workflow for the pattern).

On Linux, check that `avahi-daemon` is running on the device:

```sh
sudo systemctl status avahi-daemon
```

The agent advertises itself via Avahi using the service definition in `/etc/avahi/services/wendy-agent.service`.

The CLI itself performs an mDNS browse on Linux hosts (shipped binaries are CGO_ENABLED=0 and cannot use nss-mdns). If discovery fails, the CLI prints hints about `avahi-daemon` and UDP port 5353. Set `WENDY_MDNS_DEBUG=1` to log mDNS query failures, or `WENDY_MDNS_TIMEOUT=8s` to extend the browse window on slow networks.

### mTLS handshake fails after re-provisioning

If the CLI reports expired client certificates, check this computer's clock, then refresh them:

```sh
wendy auth refresh-certs
```

If the device certificate was regenerated, the old mTLS port listener may still be running from the previous provisioning. Restart the agent to pick up the new certificate:

```sh
sudo systemctl restart wendy-agent
```

### mTLS handshake fails with "certificate not yet valid"

If the device's real-time clock (RTC) is not synchronized (common on first boot or after power loss), the device clock may predate the provisioning certificate's `NotBefore` time. This causes all client certificates to appear "not yet valid" from the device's perspective.

From a computer on the same LAN, send a signed time proof and retry the connection:

```sh
wendy device sync-time
```

The CLI also attempts this broadcast automatically after a possible clock-related
TLS failure. It reports whether sending failed and whether the connection retry
succeeded. A successful broadcast does not confirm that the device received the
proof or changed its clock. Giving the device internet access also allows its
own time synchronization to recover.

The agent logs a warning at startup if it detects clock skew. For TLS handshake details, run the CLI with:

```sh
WENDY_TLS_DEBUG=1 wendy device version
```

## Inspecting Agent Internals

### Container state

```sh
# List containers visible to containerd
sudo ctr -n wendy containers list
sudo ctr -n wendy tasks list

# List via agent gRPC
wendy apps list
```

### OTA update state

The agent tracks OTA update state in `WENDY_CONFIG_PATH`. To see the current provisioning info:

```sh
wendy device info
```

### Agent metrics

The agent collects CPU and memory metrics for itself (via `services.CollectAgentMetrics`) and for each running container (via `services.CollectContainerMetrics`). These are streamed through the OTEL broadcaster and available via:

```sh
wendy device telemetry-stream
```

### Running the agent under a debugger

Build with debug symbols (omit the `-s -w` ldflags stripping):

```sh
cd go
go build -o bin/wendy-agent-dbg ./cmd/wendy-agent
```

Then attach with `dlv`:

```sh
dlv exec ./bin/wendy-agent-dbg
# or attach to a running process:
dlv attach $(pgrep wendy-agent)
```
