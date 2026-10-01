# `wendy cloud login`

Authenticates the CLI with Wendy Cloud. This is the primary login entry point.

## Usage

```sh
wendy cloud login
wendy cloud login --email you@example.com
wendy cloud login --issuer https://auth.wendy.dev/realms/<realm>
wendy cloud login --legacy
```

## Description

`wendy cloud login` is identical to [`wendy auth login`](../auth/login.md).
It defaults to Wendy Cloud v2 at `cloud.wendy.dev`, authenticating through
`auth.wendy.dev`. In an interactive terminal, it prompts for your email address,
discovers your home realm, then opens that realm's sign-in page in the browser.
Pass `--email` to skip the prompt, or `--issuer` to name the realm and skip email
discovery. Without an interactive terminal, provide `--email` or `--issuer`.
It completes authorization code + PKCE through a loopback callback, obtains an
operator certificate from pki-core, and stores it with a refreshable Cloud API
session. Subsequent commands use the certificate automatically.

`--api-key` selects local authentication.

Pass `--legacy` to use the old Wendy Cloud dashboard enrollment callback
(`cloud.wendy.sh`) instead. It is kept only for the previous cloud.

`wendy auth login` remains functional for backward compatibility but is no
longer listed in the top-level help. See [`wendy auth login`](../auth/login.md)
for the full flag reference and multi-session behaviour.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--email` | `""` | Email address used to discover your realm and sign in; skips the terminal prompt. |
| `--issuer` | `""` | Complete realm issuer URL; skips the terminal prompt and email-based realm discovery. |
| `--legacy` | `false` | Use the old cloud-dashboard enrollment flow (`cloud.wendy.sh`). |
| `--cloud` | `""` | Dashboard URL of a non-default cloud instance. |
| `--cloud-grpc` | `""` | gRPC endpoint of a non-default cloud instance. |

## See also

- [`wendy cloud logout`](./logout.md)
- [`wendy cloud status`](./status.md)
- [`wendy auth use`](../auth/use.md) — advanced multi-session management
