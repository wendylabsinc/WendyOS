# `wendy auth refresh-certs`

Renew the certificate Wendy uses to connect to your organization's devices.
Wendy normally renews it automatically near expiry; use this command when a
connection reports an expired or incompatible certificate.

## Renew and retry

```sh
wendy auth status
wendy auth refresh-certs
```

Then retry the command that failed. Renewal applies only to your current saved
sign-in. To use another one, select it first:

```sh
wendy auth use <context>
wendy auth refresh-certs
```

To renew every saved sign-in explicitly:

```sh
wendy auth refresh-certs --all
```

With `--all`, each result is reported separately. The command exits with an
error if any renewal fails, even when others succeed.

## If renewal is refused

Sign in again to obtain a fresh certificate:

```sh
wendy cloud login
```

An expired certificate, an older sign-in, or an organization's renewal limits
can require a new login. Interactive runs may offer this directly. If a
connection fails while you have several saved sign-ins, follow the named-context
instruction rather than renewing an unrelated organization.

If fresh sign-in does not help, retry the failing command with
`WENDY_TLS_DEBUG=1` and share the diagnostics with your administrator.

## Advanced: self-hosted certificate renewal

Certificate-only sign-ins need a renewal service in your deployment. Wendy
normally derives it from the saved certificate service address. If your
deployment uses a different address, your administrator can provide
`WENDY_PKI_RENEW_ENDPOINT`, for example
`https://renew.example.com/v1/renew`. A Cloud endpoint alone does not identify a
certificate renewal service.

## Related

- [Sign in to Wendy Cloud](../cloud/login.md)
- [Choose a saved sign-in](./use.md)
