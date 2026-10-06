# Hosted organization MCP

The `wendy-cloud-mcp` executable serves Streamable HTTP at
`https://mcp.dev.wendy.sh/orgs/<organization-uuid>/mcp` in dev and
`https://mcp.wendy.dev/orgs/<organization-uuid>/mcp` in production.
WDY-3526 and WDY-3529 add the per-user authority path; WDY-3527 adds Cloud approval.

## OpenAI connection and approval

1. Register the gateway's ML-DSA-65 service-account key and Cloud role. Configure
   its subject in Auth's `WENDY_AUTH_HOSTED_MCP_POLICY` and its full SPIFFE service
   principal in PKI's `hosted_mcp_gateway_principals`. Both are deployment-owned.
   The service receives only a purpose-marked, DPoP-bound Cloud token, never an
   unrestricted identity certificate or PKI identity token.
2. Register a dedicated public OAuth client with exact OpenAI redirect URIs,
   PKCE S256, consent and the exact organization MCP resource. Require realm MFA.
   Grant `mcp:read`, and optionally `mcp:control` for start/stop. The access token
   lasts at most five minutes; rotating refresh families last at most 24 hours.
3. An authorized operator enables hosted MCP in Cloud Settings or with
   `wendy cloud mcp enable`. This signed organization setting is only an opt-in.
4. Connect ChatGPT. The gateway verifies OpenAI's managed TLS client certificate,
   then Cloud verifies the user's OAuth token. The gateway generates a separate
   ML-DSA-65 key and requests pending consent for that verified user.
5. In Cloud Settings, sign in again with a recorded second factor. Review the
   pending key fingerprint, gateway and audience. Select devices, explicit RPCs,
   app IDs for start/stop, and an expiry of one, eight or 24 hours. Sign approval
   with your operator key. Cloud requires fresh authentication within five minutes.
6. Retry the device action. PKI verifies the signed consent and Cloud grant and
   issues a leaf valid for at most five minutes. The user can prepare another
   approval after expiry. Keys are memory-only and isolated by tenant, user and
   gateway; restarting or reaching another replica may require another approval.

The external bearer exception is restricted to OpenAI's managed mTLS connection.
The shared certificate authenticates OpenAI's platform, not a particular ChatGPT
user or token holder. User identity comes from Wendy OAuth. Health and protected
resource metadata are public. Forwarded certificate headers are never trusted.
The default trust anchor is OpenAI's published connector intermediate, with
clientAuth EKU and exact DNS SAN `mtls.prod.connectors.openai.com`. A custom
connector CA requires a matching explicit DNS identity. See
[OpenAI authentication](https://developers.openai.com/plugins/build/auth#mutual-tls-mtls).
No live ChatGPT or Responses API compatibility claim is made by these changes.

## Authority on device calls

Every tool request intersects the user's current Cloud permissions, the gateway's
current permissions, OAuth scopes and signed delegation. Inventory additionally
requires `device:list`. Organization opt-in alone cannot authorize device work.
The implemented RPC policy covers device information, hardware capabilities and
app start/stop; app control needs `mcp:control` and exact approved app IDs. Unknown
RPCs, generic CLI transport and raw SSH/registry forwarding are denied.

PKI issues FleetScope version 2 with the exact user owner, devices, apps, MCP
audience and gateway. The gateway validates the actual leaf against independently
configured roots, its own key and the approved scope. No machine credential
fallback exists. Its scoped key signs the D18 request, including delegation ID,
device principal, audience and gateway. Cloud binds the issued leaf to the same
forwarded user; PKI validates the critical scope and proof independently. Only the
dedicated Cloud MCP route admits that request. The inner device TLS connection
uses the same user-owned leaf and verifies the enrolled device identity. Device
interceptors enforce the critical scope and method/app entitlements.

Generic identity enrollment, renewal and generic certificate validation continue
to reject this purpose or its critical extension. The existing P-256 relay join
keys and HPKE remain separate from the delegated ML-DSA-65 identity key.

## Running the service

Build from the repository root with `go build -o wendy-cloud-mcp
./go/cmd/wendy-cloud-mcp`. Example configurations live in `go/ops/cloud-mcp`.
Configure exactly one of `delegation_roots_file` or `delegation_roots_pem` with
independently trusted operator/device roots. The inline form supports existing
secret-mounted JSON deployments. Never take roots from a certificate response.
Configure the server TLS chain/key, pinned Auth/Cloud/relay endpoints and machine
accounts. Optional `connector_ca_file` and `connector_dns_name` must be set together.
All keys and configuration load at startup; roll instances after rotations.

Cloud migrations through 000092 and the companion Auth, PKI and WendyOS changes
are prerequisites. Apply policy to existing gateway identities before enabling
this flow; old generic credentials must no longer authorize enrollment or D18.
These PRs do not deploy anything or register OAuth clients.

Independent revocation/status distribution, a 60-second cancellation guarantee,
isolated durable key custody and one-use write approvals remain step 5. Full
adversarial rollout testing and live OpenAI/device verification remain step 6.
Existing live permission checks and expiry bounds are not a substitute for those
controls. In particular, do not claim immediate offline-device revocation.

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
gateway can omit hints or misreport its own events. The scoped certificate and
PKI-verified tunnel request independently constrain its authority. Raw SSH and
registry forwarding are denied for hosted MCP.

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


## Blanket app consent (WDY-3527)

FleetScope version 3 adds `all_apps: true` to the signed JSON delegation and a
trailing DER `allApps BOOLEAN OPTIONAL` after audience and gateway. Version 3
requires true, an empty app-ID sequence, and the same exact nonempty device set,
owner, audience and gateway bindings as version 2. Versions 1 and 2 forbid true.
The optional false value is omitted in canonical DER, preserving existing encodings.
The consent and issued scope must match exactly; a Cloud grant cannot expand a
specific-app consent into all-app consent. Old verifiers reject the new version.

The web UI defaults approval to 30 minutes. Consent still has a hard 24-hour cap,
with leaves capped at five minutes and at the signed consent/owner/grant expiry.
Refresh never renews consent. Devices are snapshotted at approval (maximum 128);
new devices need new approval. All apps on those devices are covered only for
supported, explicitly entitled RPCs. Live Cloud user/workload checks remain required.
