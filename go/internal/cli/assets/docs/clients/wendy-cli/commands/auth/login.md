> **Tip:** [`wendy cloud login`](../cloud/login.md) is the recommended entry
> point for authenticating with Wendy Cloud. This page documents
> `wendy auth login`, which behaves identically and is kept for backward
> compatibility but is no longer listed in the top-level help. The advanced
> context commands (`use`, `rename`, `default`, `refresh-certs`) remain under
> `wendy auth`.

Signing in uses Wendy Cloud v2 at `cloud.wendy.dev` and OIDC authentication at `auth.wendy.dev` by default:

```bash
wendy cloud login
```

A bare `wendy cloud login` prompts for your email address in the terminal, discovers your home realm, then opens its sign-in page in the browser. Pass `--email you@example.com` to skip the prompt, or `--issuer` to name the realm and skip email discovery. Without an interactive terminal, provide `--email` or `--issuer`. `--api-key` selects local authentication.

The CLI asks `auth.wendy.dev` for the email's home realm unless `--issuer` is supplied, opens the authorization page, and completes authorization code + PKCE through a loopback callback. It first requests the `https://pki.wendy.sh/identity` audience, creates a PKCS#10 CSR with the same key bound to the token and DPoP proof, and sends it directly to `https://identity.pki.wendy.dev/v1/identity/certificate`. It then rotates the refresh-token family to the `https://cloud.wendy.dev/api` audience and stores the resulting mTLS certificate alongside the Cloud access token, rotating refresh token, and DPoP key using the platform credential store. Cloud is not involved in certificate issuance. The Cloud gRPC endpoint is `api.wendy.dev:443`.

The OAuth client is managed through the wendy-auth dashboard like any other interactive client. Register a public, DPoP-bound global client in the system realm with the default client ID `wendy-cli`, and allow the CLI's loopback redirect URIs. Use `--client-id` when the registered client has another ID.

Use `--auth`, `--cloud`, `--cloud-grpc`, and `--resource` to target another environment. `--pki-identity-endpoint` and `--pki-resource` override pki-core's exact public CSR endpoint and audience. `--issuer` accepts a complete realm issuer and skips email-based realm discovery.

The stored operator certificate also signs privileged Cloud mutations. For each such RPC, the CLI creates a fresh JCS request descriptor, signs it with the CSR key, and sends the resulting ML-DSA-65 JWS in `x-wendy-request-signature`; the private key never leaves the machine. The certificate also authorizes broker and direct-device operations.

Pass `--legacy` to use the old Wendy Cloud dashboard enrollment callback (`cloud.wendy.sh`) instead of the OIDC flow. `--legacy` cannot be combined with `--api-key`, `--issuer`, or `--email`. This path is kept only for the previous cloud and will be removed once the v1 cutover lands.

The legacy dashboard callback also prints a QR code. You can scan it with the **Wendy iOS app** to authenticate on your phone instead of the local browser.

## Auth contexts

Every login is stored as a named **context**. The first login is always named
`default` — you are never prompted — so single-org users never deal with names.
Additional logins get a derived name (the realm, or `org-<id>`), renamable with
[`wendy auth rename`](./rename.md). Each context owns its own operator
certificate. Switch the active context with [`wendy auth use <context>`](./use.md);
see all contexts (and which is current) with `wendy auth status`.

When your account belongs to more than one organization, the browser sign-in
prompts you to pick one; the CLI stores whichever organization issued the login
as the context (its identity comes from the issued token, not from a flag). Sign
in again and pick a different organization to add a second context — your current
context is left unchanged.

When more than one context is stored in `~/.wendy/config.json`, every cloud
command resolves which one to use in the following order:

1. **`--cloud-grpc` flag** — selects by endpoint; the current context wins when it lives on that endpoint.
2. **Single stored context** — used automatically when only one exists.
3. **Current context** — the context set with [`wendy auth use`](./use.md), when present and valid.
4. **Interactive picker** — shown in an interactive terminal when no current context is set.
5. **Error** — in non-interactive environments (pipes, CI, MCP) with no current context, the command exits with an error directing you to pass `--cloud-grpc` or run `wendy auth use`.

A stale current context (its session was removed) is never silently replaced:
the picker warns, `wendy auth default` self-clears, and non-interactive callers
receive an error.

Configs from an earlier CLI are migrated automatically on first use — existing
sessions become named contexts (the previous default becomes the current
context) with no re-login.
