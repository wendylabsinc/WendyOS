# `wendy cloud login`

Authenticates the CLI with Wendy Cloud. This is the primary login entry point.

## Usage

```sh
wendy cloud login --email you@example.com
wendy cloud login --issuer https://auth.dev.wendy.sh/realms/<realm>
wendy cloud login --service-account ./wendy-service-account.json
wendy cloud login --legacy
```

## Description

`wendy cloud login` is identical to [`wendy auth login`](../auth/login.md) — it
reuses the same implementation. For now, it defaults to the legacy dashboard
flow at `cloud.wendy.sh`. With `--email` or `--issuer`, it runs the OIDC flow: it discovers
your realm from `--email` (or takes `--issuer` directly), completes authorization
code + PKCE through a loopback callback, obtains an operator certificate from
pki-core, and stores it with a refreshable Cloud API session. Subsequent commands
use the certificate automatically.

A bare `wendy cloud login` uses legacy login. `--api-key` continues to select
local authentication. `--service-account` signs in headlessly as a wendy-auth
service account (see [`wendy auth login`](../auth/login.md#service-account-login)).

Pass `--legacy` to use the old Wendy Cloud dashboard enrollment callback
(`cloud.wendy.sh`) instead. It is kept only for the previous cloud.

`wendy auth login` remains functional for backward compatibility but is no
longer listed in the top-level help. See [`wendy auth login`](../auth/login.md)
for the full flag reference and multi-session behaviour.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--email` | `""` | Email address used to discover your realm and sign in (OIDC flow). |
| `--issuer` | `""` | Complete realm issuer URL; skips email-based realm discovery. |
| `--service-account` | `""` | Service-account key file for headless login; `WENDY_SERVICE_ACCOUNT_KEY` may hold its contents instead. |
| `--legacy` | `false` | Use the old cloud-dashboard enrollment flow (`cloud.wendy.sh`). |
| `--cloud` | `""` | Dashboard URL of a non-default cloud instance. |
| `--cloud-grpc` | `""` | gRPC endpoint of a non-default cloud instance. |

## See also

- [`wendy cloud logout`](./logout.md)
- [`wendy cloud status`](./status.md)
- [`wendy auth use`](../auth/use.md) — advanced multi-session management
