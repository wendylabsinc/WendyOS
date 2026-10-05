# Hosted organization MCP

The `wendy-cloud-mcp` executable serves these Streamable HTTP resources:

- Production: `https://mcp.wendy.dev/orgs/<organization-uuid>/mcp`
- Test: `https://mcp.dev.wendy.sh/orgs/<organization-uuid>/mcp`

It is a separate, always-on service. Cloud owns organization opt-in, identity
verification, inventory, and authorization. The gateway owns the MCP transport,
CLI proxy, and each organization's machine-account private key. Keys are never
returned to clients. This implementation requires the companion Cloud changes
in `feat/hosted-org-mcp`, including migration 000085.

## Provisioning and owner opt-in

1. Register an ML-DSA-65 machine-account key in the organization's Wendy Auth
   realm. Give that account an explicit Cloud organization role. It gets no
   automatic membership. Store its private key in the gateway's secret mount.
2. Add its organization UUID, issuer, subject, and `key_file` (or `key_pem` inside
   the Secret Manager configuration) to the deployment's `machines` array.
   The organization UUID must be its immutable tenant UUID. Restart/roll the
   gateway after updating this configuration.
3. Register the connecting OAuth clients in the realm. Permit the organization's
   exact MCP URL as an OAuth resource and configure the clients' exact redirect
   URIs. Hosted clients use their registered OAuth client ID/secret; this does
   not add dynamic client registration to Wendy Auth.
4. An organization owner signs into the ordinary Cloud API and enables access:

   ```sh
   wendy cloud mcp enable --organization <uuid> --service-account <subject> \
     --cloud-http https://api.dev.wendy.sh
   wendy cloud mcp status --organization <uuid> --cloud-http https://api.dev.wendy.sh
   ```

   Production uses `https://api.wendy.dev`. Enabling and disabling are owner-only,
   DPoP-protected Cloud requests and write an audit event with the settings change.
   Provisioning the machine account is a prerequisite; enabling the setting does
   not create an Auth account or upload its key.
5. Connect the organization MCP URL in a standard OAuth-capable MCP client.
   Protected-resource metadata advertises that organization's realm issuer.

Disable with `wendy cloud mcp disable --organization <uuid> --cloud-http <origin>`.
New work is denied immediately after the setting commits. Active streams recheck
at five-second intervals with a five-second backend timeout and close on a failed
check, identity change, or token expiry. Backend failures fail closed.

## Authorization and certificates

The incoming user-to-MCP connection accepts OAuth bearer tokens, including direct
hosted clients. This is a resource-specific exception: it does not relax Cloud's
machine authentication or the device transport. Cloud verifies the user's token
against the exact organization MCP audience, issuer, tenant and revocation state.
The token travels only to Cloud's authorization component of this resource server;
it is not used as a Cloud API credential or forwarded to devices.

Every operation requires BOTH the user's and the configured machine account's
current organization role and resolved permissions. Device work requires
`device:read` and `tunnel:connect`; inventory also requires `device:list`. These
are Cloud's existing device/tunnel permissions, including its deny rules. This
change does not invent a separate per-RPC permission catalog. The selected asset
must belong to that organization and have a PKI enrollment binding.

The machine uses RFC 7523 assertions signed by its registered ML-DSA-65 key. It
obtains a DPoP-bound PKI token and a `/service/<subject>` operator-capable
certificate, then a separate DPoP-bound Cloud API token. Certificate identity,
public key, validity, and token binding are checked. Device RPCs use operator mTLS
inside Cloud's signed relay, with the device certificate identity pinned to its
Cloud enrollment record.

The existing relay protocol still uses P-256 ephemeral session keys and its
specified HPKE suite. Those ephemeral transport keys are separate from the
ML-DSA-65 service-account identity and certificate.

## Tools and CLI

`device_list` lists the eligible device inventory (up to 1,000 rows).
`device_methods` lists Agent RPCs or their top-level request fields. `device_rpc`
accepts a device UUID, a full RPC method, and a protobuf JSON request. It handles
unary and bounded server streams. Defaults are 30 seconds and 20 responses;
maximums are 60 seconds, 100 responses and 1 MiB of result data. Client-streaming
and bidirectional operations use the CLI transport.

Log into a separate saved MCP session using the existing PKCE login flow. Giving
it its own `--cloud-grpc` value keeps it separate from the ordinary Cloud session:

```sh
wendy auth login --issuer https://auth.dev.wendy.sh/realms/<realm> \
  --cloud-grpc mcp.dev.wendy.sh:443 \
  --resource https://mcp.dev.wendy.sh/orgs/<uuid>/mcp
wendy device info --device mcp://mcp.dev.wendy.sh/orgs/<uuid>/devices/<asset-uuid>
wendy run --device mcp://mcp.dev.wendy.sh/orgs/<uuid>/devices/<asset-uuid>
```

Use the corresponding production Auth/PKI login flags for production, as with an
ordinary production CLI login. The CLI refreshes the saved OAuth session and
opens a TLS WebSocket carrying gRPC. The gateway terminates gRPC, authorizes each
method, and creates the machine-authenticated device connection. It does not
forward caller identity headers. Streams are limited to one hour or the earlier
user/machine token expiry; reconnect obtains fresh credentials.

Image registry uploads use explicit Cloud catalog services: `wendy-registry`
(loopback port 5000) and `wendy-registry-darwin` (5555). Devices must run the updated
relay catalog for these entries. SSH forwarding uses the existing `ssh` entry:

```sh
wendy cloud mcp tunnel \
  --device mcp://mcp.dev.wendy.sh/orgs/<uuid>/devices/<asset-uuid> \
  --service ssh --listen 127.0.0.1:2222
```

Local forwarding binds only loopback. Arbitrary hosts/ports and raw `wendy-agent`
forwarding are refused. Device calls go through the gRPC authorization path.

## Running the service

Build from the WendyOS repository root:

```sh
go build -o wendy-cloud-mcp ./go/cmd/wendy-cloud-mcp
```

Example configurations and a systemd unit are in `go/ops/cloud-mcp`. The service
requires an HTTPS origin, a TLS chain/key, pinned Auth/PKI/Cloud endpoints, and
machine-account configuration. It never reads a developer's saved login.
`/healthz` reports process liveness. SIGTERM cancels active requests and drains
HTTP connections. Mounted keys/configuration and server TLS certificates are
loaded at startup; roll instances after rotations.

The Cloud Pulumi changes create a separate COS instance group and IPv4/IPv6
passthrough load-balancer frontends when `hostedMCPImageDigest` is configured.
They do not deploy until bootstrap provides the dedicated runtime identity,
secret containers, external dual-stack subnet, DNS zone and immutable images.
Production still deploys only through Cloud's immutable release-tag workflow.

## Local verification (2026-10-05)

- 124 passing Go test cases, including subtests, under `-race` across `cloudmcp`,
  `browserauth`, `hostedmcp`, and `cloudrelay`.
- Two focused CLI command tests pass. The broader command suite has a failure in
  `TestSimulatorFilterAsksOnlyVMsItCanReachAndDoesNotKnow` when LAN discovery sees
  local devices; no simulator code was changed for this feature.
- Linux amd64 gateway binary builds with CGO disabled; scoped `go vet` passes.
- Companion Cloud: 88 Swift tests in eight suites pass, including real PostgreSQL
  opt-in, permission reduction, audience rejection and audit checks. Swift format
  lint passes; infrastructure tests pass in all three packages.
- No live DNS deployment, OAuth-client registration, or physical-device smoke test
  has been performed. Those checks remain part of rollout.
