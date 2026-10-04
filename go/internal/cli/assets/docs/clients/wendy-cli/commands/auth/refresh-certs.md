# `wendy auth refresh-certs`

Refreshes the mTLS certificate of every stored auth session directly against
[pki-core](../../../../pki/). OIDC sessions use their refresh token and PKI
identity endpoint to obtain a fresh operator certificate. Certificate-only
sessions use the renew frontend described below. Cloud does not relay either
request.

A renewal is a re-issue, not a re-key of the same certificate: the CLI generates
a new key pair of the same algorithm as the current one (an ML-DSA-65 session
stays ML-DSA-65), builds a CSR for it, and posts the CSR to the renew frontend
over mTLS, presenting the certificate being renewed. Next to the CSR the body
carries a `possession_proof`: a signature by the current certificate's key over
that certificate and the CSR, in the key's own algorithm, fresh for every
request. pki-core checks it itself rather than trusting the frontend's
handshake.

The renewed certificate keeps the identity of the one it replaces, and the CLI
does not get to assert what that identity is. pki-core reads the principal off
the presented certificate and stamps it into the new leaf itself; any identity
the CSR asserts is replaced or refused.

## When you see a TLS authentication failure

If a command reports `TLS authentication failed. Your certificates may be
outdated or incompatible with the device.`, run `wendy auth refresh-certs`,
then retry the original command. If it still fails, rerun the original command
with `WENDY_TLS_DEBUG=1` to see TLS diagnostics. See the renewal limits below
if refreshing the certificates is refused.

## Usage

```sh
wendy auth refresh-certs
```

## The renew frontend

`WENDY_PKI_RENEW_ENDPOINT` names pki-core's renew frontend, e.g.
`https://renew.pki.example:8451/v1/renew`, and overrides deployment defaults.
Without it, the renew frontend is derived from the pki-core identity endpoint
the session already holds, by replacing its leading `identity.` label:
`https://identity.<rest>/...` becomes `https://renew.<rest>/v1/renew`, port
included. The derivation therefore stays inside one PKI deployment and names no
environment.

A session whose identity endpoint is not an `https://identity.<rest>` URL
derives nothing and requires the override. That includes a session which knows
only its Cloud endpoint: which cloud answers says nothing about which PKI
mints, so no renew host is ever derived from one.

## It also runs by itself

The CLI renews ahead of expiry without being asked: when an auth session is
resolved and its certificate has less than 15 minutes of validity left, the
renewal happens before the connection is attempted. This command is the manual
trigger for the same path, and is useful when a certificate is close to expiry
and you would rather not discover it mid-command.

## What renewal will not do

- **No renewal of a certificate pki-core did not issue.** The renew frontend
  routes a request by the tenant identity on the presented certificate and
  renews only lineages it recorded at first mint. A session predating that —
  one whose certificate carries no pki-core tenant identity — has no renewal
  path at all; log in again to be issued one that does.
- **No roll-forward from an expired certificate.** Renewal requires a
  currently-valid certificate to present. Once yours has expired the renew
  frontend needs an approved grant, which the CLI cannot mint — log in again
  with [`wendy cloud login`](../cloud/login.md), which issues a fresh
  certificate outright.
- **Renewals are budgeted per lineage, and the budget is inherited rather than
  requested.** A plain operator certificate renews a fixed number of times and
  then has to be re-minted.
- **Entitlement-bearing and over-duration certificates are not renewable at
  all** unless their tenant has opted in.

Each of these arrives as an explicit refusal and is reported as itself, not
retried as a generic failure. In an interactive terminal the refusals that only
a new certificate can resolve offer to log you in again on the spot.

## Related

- [`wendy cloud login`](../cloud/login.md) — issue a fresh operator certificate.
- [`wendy auth use`](./use.md) — choose which stored session is the default.
- [PKI](../../../../pki/) — the three authorized certificate paths.
