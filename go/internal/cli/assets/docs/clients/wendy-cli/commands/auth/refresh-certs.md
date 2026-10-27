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

## Automatic renewal through your sign-in

The renewal margin is the smaller of 15 minutes and one third of the
certificate's signed lifetime, clamped to zero. A 10-minute certificate starts
renewing with less than 3 minutes 20 seconds left. Renewal using the held
certificate runs first and requires it to remain valid.

For an OIDC operator sign-in, if that renewal cannot replace the certificate,
Wendy silently obtains a fresh certificate through the current sign-in session.
This also works after the old certificate expires. It runs without confirmation,
including in scripts and JSON commands, at most once per invocation. The explicit
command remains available.

Automatic re-issuance only uses the current context. A saved selector for another
context quietly keeps using its still-valid certificate. Once it expires, switch
to that context explicitly. When a device does not reveal its organization and
several contexts hold certificates, Wendy lists them and refreshes none.

A replacement must match the tenant, operator and requested key and have more
than its own renewal margin remaining. These checks also apply to explicit
refreshes. Its issuance time may be up to and including 60 seconds ahead of this
computer's clock, without extending its expiry. A held certificate further in the
future reports a clock problem without spending a refresh token.

Rotated refresh tokens are saved immediately, before further validation or
issuance. A failed save stops the command.

## If renewal is refused

For an OIDC sign-in, certificate-service and network failures preserve a
still-valid certificate. Wendy asks you to sign in only when the Auth sign-in
service refuses the session, such as when a refresh token has expired. A Cloud
API `Unauthenticated` response alone does not offer sign-in for OIDC contexts.
When asked, run:

```sh
wendy auth login
```

For certificate-only sign-ins, an expired certificate, an older sign-in, or an
organization's renewal limits can require a new login. Interactive runs may offer
this directly. If a connection fails while you have several saved sign-ins,
follow the named-context instruction rather than renewing an unrelated
organization.

If fresh sign-in does not help, retry the failing command with
`WENDY_TLS_DEBUG=1` and share the diagnostics with your administrator.

## Advanced: self-hosted certificate renewal

Certificate-only sign-ins need a renewal service in your deployment. Wendy
normally derives it from the saved certificate service address. If your
deployment uses a different address, your administrator can provide
`WENDY_PKI_RENEW_ENDPOINT`, for example
`https://renew.example.com/v1/renew`. A Cloud endpoint alone does not identify a
certificate renewal service.

This renewal presents the held certificate over mTLS and signs a possession
proof over that certificate and a fresh request. The certificate service owns
the identity in the replacement. Renewal is limited to recorded certificate
lineages and their remaining budget; entitlement-bearing and over-duration
certificates require tenant opt-in. An expired certificate supplies no renewal
authority. OIDC re-issuance instead uses the valid sign-in session as its proof.

## Related

- [Sign in to Wendy Cloud](../cloud/login.md)
- [Choose a saved sign-in](./use.md)
