# `wendy cloud status`

Shows the currently active Wendy Cloud auth session. Equivalent to `wendy auth status`.

## Usage

```sh
wendy cloud status
```

## Description

`wendy cloud status` reports the stored Wendy Cloud session — the dashboard URL,
gRPC endpoint, certificate state, and the session key algorithm with the login
path that minted it (`Key: ML-DSA-65 (OIDC login)` or `Key: ECDSA P-256 (legacy login)`). It reuses the same implementation as the
(now hidden) `wendy auth status` and is the surfaced way to check who you are
logged in as.

When more than one session is stored, use [`wendy auth use`](../auth/use.md) to
inspect and set the default.

## See also

- [`wendy cloud login`](./login.md)
- [`wendy cloud logout`](./logout.md)
