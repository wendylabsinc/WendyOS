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
   realm. Give that account a suitable Cloud organization role. Cloud's normal
   service-principal admission rules apply; it cannot adopt an organization or
   gain owner access through adoption. Store its private key in the gateway's secret mount.
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

## Audit and tracing

Every inbound HTTP request gets a server-generated `X-Request-ID`. That same UUID
is `correlation_id` in logs and `x-correlation-id` on internal calls. A fresh
OpenTelemetry trace contains authorization, MCP tool, device RPC, and connection
spans. Valid incoming W3C trace context is retained only as a link; callers cannot
select the gateway's trace ID or sampling policy. Baggage and tracestate are not
forwarded. Initialization, discovery/list calls, protocol errors, and rejected
HTTP requests have diagnostic records as well.

Cloud appends successful and denied authorization decisions to its existing
per-organization hash-chained audit log before returning the decision. Gateway
start/completion reports refer to a recent decision issued to that same machine
and organization. Cloud takes the actor, target, and operation from its own
record and marks these reports `gateway_reported`. Device work does not start
if its start report cannot be persisted. Completion reports have a separate
five-second delivery deadline after disconnect; failures produce
`audit_delivery_failed`, and an unmatched start remains an unknown outcome.
There is no claim of exactly-once delivery or rollback after an audit failure.

A gRPC connection is reported connected only after it reaches Ready. The agent's
mTLS interceptors record traced RPC start/completion with the peer certificate
fingerprint and gRPC status. These records are `device_observed`; correlation is
explicitly a `peer_hint`. No human identity is inferred from headers. A compromised
gateway can omit hints or misreport its own events; certificate-plus-grant
enforcement remains a separate follow-up. Raw SSH/registry connections have
connection-level records, not command, file-content, or individual registry-request audits.

Records exclude OAuth tokens, DPoP proofs, certificate/key material, request and
response bodies, shell commands, MCP arguments/results, arbitrary JSON-RPC IDs,
and client-supplied names/User-Agent strings. They identify the verified Wendy
user and organization. They do not identify a ChatGPT account or assert that an
unverified client name proves a request originated from ChatGPT.

The standalone service writes JSON logs. Managed deployments send container logs
to Cloud Logging using the attached runtime identity, which needs only log-entry
creation, not deletion or log-configuration permissions. Cloud's database audit
is independent of the gateway's runtime identity. Self-hosted deployments can
collect stdout and retain Cloud's existing audit/checkpoint storage.

For actual trace export, set `trace_endpoint` in the gateway configuration to an
operator-controlled OTLP HTTP traces URL, such as `https://collector.example/v1/traces`.
Include the collector path. HTTPS is required except for a
loopback HTTP collector. Standard `OTEL_EXPORTER_OTLP_*` environment configuration
is also supported when supplied to the process. With no endpoint, structured logs
still have trace/span IDs, but spans are not exported to a tracing backend. Export
uses a bounded queue and does not gate authorization; durable Cloud audit does.
Configure the collector, access policies, retention, and alerts before rollout.

Search by the response's `X-Request-ID` to join gateway logs, Cloud audit rows,
Cloud tunnel records, and device logs. A finished tool span with an error can
coexist with a successfully opened connection; neither implies that an earlier
side effect was undone. Audit reads retain Cloud's tenant scoping and existing
checkpoint verification. Monitor missing completion records, audit delivery errors,
and trace export errors.

## Local verification (2026-10-05)

- 180 passing Go test cases, including subtests, under `-race` across `cloudmcp`,
  `agent/interceptor`, `browserauth`, `hostedmcp`, and `cloudrelay`.
- Two focused CLI command tests pass. The broader command suite has a failure in
  `TestSimulatorFilterAsksOnlyVMsItCanReachAndDoesNotKnow` when LAN discovery sees
  local devices; no simulator code was changed for this feature.
- Linux amd64 gateway binary builds with CGO disabled; scoped `go vet` passes.
- Companion Cloud: 93 Swift tests in nine suites pass, including real PostgreSQL
  opt-in, permission reduction, audience rejection, event binding, and audit-chain checks. Swift format
  lint passes; infrastructure tests pass in all three packages.
- No live DNS deployment, OAuth-client registration, or physical-device smoke test
  has been performed. Those checks remain part of rollout.
