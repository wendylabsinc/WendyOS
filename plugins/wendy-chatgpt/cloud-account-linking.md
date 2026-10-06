# Hosted Cloud account linking

`http.cloud_link` gives each ChatGPT connection its own Wendy Cloud identity.
The hosted gateway uses that account's Cloud tokens and operator certificate
for inventory, Cloud relay grants, and device mTLS. It does not read the CLI's
saved login. Static robot policies, grants, local simulators, workspaces, host
operations, and operator Cloud sources cannot be combined with this mode.

The local stdio plugin is unchanged. This implementation has local protocol,
security, and lifecycle tests; it has not been deployed or verified against
production Wendy Cloud or a real ChatGPT account connection.

## Connection flow

1. ChatGPT discovers the protected-resource and authorization-server metadata.
2. The gateway validates a predefined client's exact callback, MCP resource,
   requested scopes, and PKCE S256 challenge. The user sees a permission form.
3. After approval, the user signs in with Wendy. The gateway uses a separate
   upstream state and PKCE transaction, verifies DPoP-bound JWTs, provisions that
   user's operator certificate, and obtains that user's Cloud token.
4. ChatGPT receives a short authorization code. Its PKCE exchange returns
   gateway access and refresh tokens. Cloud tokens, keys, and certificates stay
   in encrypted server storage.
5. Each MCP request resolves one linked account. Inventory refreshes under that
   account; connecting checks the Cloud asset's tenant and certificate identity
   before device mTLS.

Gateway access tokens last at most one hour. Refresh tokens rotate, and reuse
revokes the whole connection. Revoking an access or refresh token removes its
Cloud credentials and invalidates that connection's other tokens. Connections
require a fresh Wendy sign-in after 30 days; an expired or revoked Cloud session
or operator certificate can require reconnecting earlier. In-progress calls may
finish after revocation; subsequent authenticated requests fail.

## Configuration and review

[gateway.cloud-link.example.json](gateway.cloud-link.example.json) is a schema
example with placeholders, not a production service map. Fill it with verified
production service URLs and the actual registered OAuth clients after review.
Do not infer production endpoints by removing `dev` from existing domains.

Register a Wendy OAuth public client that permits the gateway's exact
`https://<gateway-origin>/auth/callback` URI. It must support PKCE S256, DPoP,
refresh-token rotation, the identity resource, the Cloud resource, and operator
certificate enrollment for the authenticated user. The existing browser login
contract implements these operations; this repository does not register the
hosted callback or change Cloud authorization policy.

Register the downstream ChatGPT client and exact callback in `clients`. The
gateway supports predefined public clients and confidential clients with
`client_secret_basic` or `client_secret_post`. Omit `secret_env` for a predefined
public client. It does not advertise dynamic registration or CIMD support.
Predefined clients are a supported option in
[OpenAI's authentication guide](https://developers.openai.com/plugins/build/auth).
Keep client registrations valid while users and reviewers have connections.

Provision a private state directory with mode `0700`, an encrypted account file
on persistent storage, and `WENDY_CHATGPT_STATE_KEY` containing standard base64
for a random 32-byte key. Store the key in the deployment secret manager; it is
not a user-facing setting. If using a confidential downstream client, provision
its secret with at least 32 bytes in the configured environment variable.
Back up the encrypted file and its key separately. Changing the MCP resource or
Cloud service configuration prevents reopening an existing credential file;
migrate deliberately or require users to reconnect. Losing or changing the key
also requires reconnecting.

Run one gateway process per account state file. An exclusive file lock prevents
concurrent writers. State writes are atomic; the service has bounded storage
for 10,000 linked accounts and 20,000 access/refresh records each. Retired refresh
records remain until expiration to detect replay, so effective connection
capacity depends on refresh frequency. A replicated production deployment needs
a shared transactional credential store and coordinated token rotation.

Serve `wendy mcp gateway --config <policy.json> --listen 127.0.0.1:8788` behind
verified HTTPS at `resource_url`. Route `/mcp`, both protected-resource metadata
paths, `/.well-known/oauth-authorization-server`, `/oauth/authorize`,
`/auth/callback`, `/oauth/token`, and `/oauth/revoke` to this process. Apply edge
rate limits to authorization and token routes. A `/healthz` response proves
process liveness only. Camera capture still needs the gateway host's GStreamer
runtime and appropriate plugins.

The supported hosted scopes are `robots:read`, `cameras:capture`, `apps:control`,
and `preferences:write`. Capture and app control also require their policy
opt-ins. Host builds, project access, dynamic app-tool exports, and durable event
subscriptions are outside this mode. Durable event scopes are intentionally
excluded because their current local subscription storage is not encrypted.

Before production deployment, verify account linking, refresh, logout/revocation,
and device operations in ChatGPT using two accounts with different Cloud
permissions. Confirm that neither account can select the other's assets, that
the enrollment policy grants only its operator identity, and that offline or
removed inventory never establishes readiness or access. This work does not
change the packaged local MCP connection or submit a plugin.

## Local checks

```sh
go test -race ./go/internal/cli/cloudlink ./go/internal/cli/browserauth
go test ./go/internal/cli/mcp ./go/internal/cli/commands
```

The account-link tests cover consent CSRF, exact callbacks and resources, PKCE,
issuer mixups, code/callback replay, separate account credentials, encrypted
restart, concurrent accounts, scope narrowing, refresh replay revocation,
client-scoped revocation, token expiry, exclusive storage ownership, and
encrypted-state tampering and service binding. The browser-auth tests cover
JWT verification, DPoP, certificates, and Cloud device identity checks.
