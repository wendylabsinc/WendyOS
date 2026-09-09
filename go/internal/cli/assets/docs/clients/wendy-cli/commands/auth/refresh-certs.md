# `wendy auth refresh-certs`

Renews the mTLS certificate of every stored auth session directly against
[pki-core](../../../../pki/)'s renew frontend. Cloud is not in this path: it
neither mints the certificate nor relays the request.

A renewal is a re-issue, not a re-key of the same certificate: the CLI generates
a new key pair, builds a CSR for it, and posts the CSR to the renew frontend
over mTLS. Presenting the certificate being renewed *is* the proof of
possession — the request body carries nothing but the CSR.

The renewed certificate keeps the identity of the one it replaces, and the CLI
does not get to assert what that identity is. pki-core reads the principal off
the presented certificate and stamps it into the new leaf itself; any identity
the CSR asserts is replaced or refused.

## Usage

```sh
wendy auth refresh-certs
```

## The renew frontend

`WENDY_PKI_RENEW_ENDPOINT` names pki-core's renew frontend, e.g.
`https://renew.pki.example:8451/v1/renew`. Nothing is derived from the cloud
endpoint. When it is unset there is no renew frontend configured — a supported
state, in which the CLI never renews on its own and this command reports that
nothing is configured to renew against.

For Wendy's own environments the frontend is `https://renew.dev.pki.wendy.sh/v1/renew` on
dev, and the same host without the `dev.` label on production once that
environment is online.

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
