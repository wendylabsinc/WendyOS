# Wendy Client

A browser workspace backed by Wendy's Go client compiled to WASM. It starts with
an empty workspace. Device metrics, applications, and OpenTelemetry data come
from connected agents; there is no demo mode.

## Run locally

Install Node.js 22.13 or newer and Go 1.27 or newer, then run from `web-client`
inside the Wendy repository:

```sh
npm i && npm run dev
```

This builds the Go WASM client and matching JavaScript runtime, builds and starts
the Cloud API and broker relay on `127.0.0.1:8788`, then starts the web app on port
5173. The relay stops with the dev server when you press Ctrl-C. No separate
relay terminal or hosted Site configuration is needed. The first run may download
Go modules. Restart `npm run dev` after changing Go code to rebuild it.

Both ports must be free. Stop any manually started relay or previous dev server
before running this command. The app fails if port 5173 is occupied, since signing
in requires the registered OAuth callback on that port.

If Cloud discovery cannot connect, check the `npm run dev` terminal for relay
upstream errors. The relay verifies TLS and HTTP/2 before accepting the browser
connection, and logs connection failures there.

Open **http://localhost:5173**. Use this exact hostname and port: the existing
`cloud-login` OAuth client registers `http://localhost:5173/auth/callback`.
Enter your email and choose **Sign in with Wendy**. Authentication opens at
`auth.dev.wendy.sh`; your password never passes through this app.

The WASM worker follows the CLI's flow:

- Email-to-realm lookup, OIDC discovery, authorization code and S256 PKCE.
- ML-DSA-65 operator key generation, per-request DPoP proofs and nonce retry.
- Verification of access-token signatures, issuer, audience, expiry and key binding.
- A CSR and operator certificate from `identity.dev.pki.wendy.sh`.
- Refresh-token rotation to the Cloud API resource, then paginated v2 device discovery.

The worker saves the private key, certificates, profile, and refresh token in
IndexedDB for this browser origin. Reloading or reopening the browser restores
the same certificate, and expired access tokens are refreshed. These credentials
are not sent to the UI thread or stored in localStorage. The
same-origin `/api/wendy-auth` proxy forwards only allowlisted Wendy auth/PKI
requests and Cloud grant signing keys; tokens and public CSRs pass through it, but private keys do not.
The local `/cloud` WebSocket relay terminates verified public TLS to the fixed
`api.dev.wendy.sh:443` upstream. DPoP proofs are generated inside the worker.
The relay binds only to a loopback IP and accepts direct loopback connections
with the configured browser origin. Origin is a browser CSRF check, not relay
authentication. Remote listeners and reverse-proxy forwarding are unsupported;
do not expose this development relay through a proxy or port forward. A hosted
relay requires a separate authenticated service. The relay does not accept an
arbitrary Cloud hostname.

Sign-out deletes the saved credentials and clears other open Wendy Client tabs.
Transient connection failures preserve the saved session for retry. An expired
operator certificate requires signing in again. Browser storage is specific to
the browser profile and origin, so localhost and the hosted Site have separate
sessions. Clearing site data also clears the saved login.
Sign-out is local; it does not log you out of other Wendy applications.

## Cloud workspace

The sidebar shows the signed-in email from OIDC UserInfo and the organization
name from Cloud's v2 OrganizationService. UserInfo must match the authenticated
subject, and organization responses must match the authenticated tenant UUID.
Profile lookups that fail leave a compact expandable message in the sidebar.

Select an online cloud device to open a live `wendy device top` dashboard with
CPU, memory, container storage, temperatures, GPUs, battery, and per-application
usage. CPU uses counter deltas and normalizes application usage across all cores.
Missing metrics remain unavailable. Applications can be started, stopped, and
restarted. Selecting an application opens its logs.

Device tabs stream OTLP logs, metrics, and traces from the selected cloud device.
Streams support application filters, search, and expandable attributes. The UI
retains at most 500 entries and shows stream failures. Applications must emit
OTel data for it to appear. There is no browser shell; shell RPCs are disabled.

Cloud connections use the CLI's signed tunnel authorization, verified Cloud
grants, broker challenge proofs, and end-to-end agent mTLS pinned to the selected
tenant/device principal. The worker reads the selected asset's `pki_device_name`
enrollment binding from the authenticated Cloud API. It does not assume the
inventory UUID is the certificate identity or learn that identity from the peer.
The local `/broker` relay accepts only `relay.dev.wendy.sh`, `relay.wendy.sh`,
`eu.relay.wendy.sh`, and the exact Wendy development Cloud Run broker on port 443, and verifies
their public TLS certificates. The worker selects
the endpoint from the verified grant. Operator private keys stay in the browser.
Both inventory and device sessions currently require the local relay. Native
flashing, local builds, and LAN scanning still require the CLI.

## Hosted version

The existing private Site is also updated. Wendy auth currently rejects its
callback URL. To enable hosted sign-in, register this exact redirect URI on the
`cloud-login` public client and enable it in the login component:

```
https://wendy-client.joannis690160.chatgpt.site/auth/callback
```

Hosted Cloud discovery additionally needs a trusted WSS Cloud relay and its
configuration in the worker. Until these exist, the hosted UI explains the local
sign-in requirement. It does not pretend to be signed in or substitute fake data.

## Validation

```sh
# From the repository root:
go test -race ./go/internal/cli/browserauth
node go/experiments/wasmgrpc/auth-worker.mjs
node web-client/tests/credential-store.test.mjs
node --experimental-strip-types web-client/tests/telemetry.test.mjs
node --experimental-strip-types --test web-client/tests/auth-proxy.test.mjs

# From web-client:
npm run build:wasm
npx tsc --noEmit
npm run build
```

The auth worker integration test uses the running local app and live auth
metadata/authorization endpoints without signing in a user. Unit tests cover a
complete PKCE/DPoP/certificate flow and reject callback replay, state mismatch,
expired callbacks, untrusted issuers, and invalid token claims. Interactive
account login and account-specific discovery require the user's authentication.
The agent worker integration fixture lives in `go/experiments/wasmgrpc`.

Browser UI automation has not been run. Optional WebMCP read-only workspace and
navigation tools are feature-detected; credentials are never exposed to them.
