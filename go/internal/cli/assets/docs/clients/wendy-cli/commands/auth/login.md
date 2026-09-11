> **Tip:** [`wendy cloud login`](../cloud/login.md) is the recommended entry
> point for authenticating with Wendy Cloud. This page documents
> `wendy auth login`, which behaves identically and is kept for backward
> compatibility but is no longer listed in the top-level help. The advanced
> session commands (`use`, `default`, `refresh-certs`) remain under
> `wendy auth`.

For the new Cloud API authentication flow, provide your email address:

```bash
wendy cloud login --email you@example.com
```

The CLI asks `auth.dev.wendy.sh` for the email's home realm, opens that realm's authorization page, and completes authorization code + PKCE through a loopback callback. It first requests the `https://pki.wendy.sh/identity` audience, creates a PKCS#10 CSR with the same key bound to the token and DPoP proof, and sends it directly to `https://identity.dev.pki.wendy.sh/v1/identity/certificate`. It then rotates the refresh-token family to the `https://cloud.dev.wendy.sh/api` audience and stores the resulting mTLS certificate alongside the Cloud access token, rotating refresh token, and DPoP key using the platform credential store. Cloud is not involved in certificate issuance.

The OAuth client is managed through the wendy-auth dashboard like any other interactive client; the auth service has no CLI-specific client configuration. Register a public, DPoP-bound client (the default client ID is `wendy-cli`) and allow the CLI's loopback redirect URIs. Use `--client-id` when the registered client has another ID.

Use `--auth`, `--cloud`, `--cloud-grpc`, and `--resource` to target another environment. `--pki-identity-endpoint` and `--pki-resource` override pki-core's exact public CSR endpoint and audience. `--issuer` accepts a complete realm issuer and skips email-based realm discovery.

The stored operator certificate also signs privileged Cloud mutations. For each such RPC, the CLI creates a fresh JCS request descriptor, signs it with the CSR key, and sends the resulting ES256 JWS in `x-wendy-request-signature`; the private key never leaves the machine. The certificate also authorizes broker and direct-device operations. Without `--email` or `--issuer`, the command continues to use the legacy cloud-dashboard enrollment callback.

The legacy dashboard callback also prints a QR code. You can scan it with the **Wendy iOS app** to authenticate on your phone instead of the local browser.

## Multiple auth sessions

When more than one Wendy Cloud session is stored in `~/.wendy/config.json`, every cloud command resolves which session to use in the following order:

1. **`--cloud-grpc` flag** — always wins when supplied.
2. **Single stored session** — used automatically when only one session exists.
3. **Persisted default** — the session set with [`wendy auth use`](./use.md) is used when present and valid.
4. **Interactive picker** — shown in an interactive terminal when no default is set.
5. **Error** — in non-interactive environments (pipes, CI, MCP) with no default set, the command exits with an error directing you to pass `--cloud-grpc` or run `wendy auth use`.

A stale default (the named session was removed) is never silently used: the picker warns, `wendy auth default` self-clears, and non-interactive callers receive an error.
