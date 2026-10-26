# `wendy auth login`

Sign in to Wendy Cloud and save access to your organization. This command is
equivalent to [`wendy cloud login`](../cloud/login.md), the recommended entry point.

```sh
wendy cloud login
```

Complete sign-in in your browser, select an organization if asked, and return to
the terminal. Wendy saves the credentials needed for later commands. You can
supply your email first with `--email you@example.com`.

## Auth contexts

A **context** is a saved sign-in for one organization. The first is named
`default`. Sign in again and choose another organization to add a second context;
your current context stays selected.

```sh
wendy auth status
wendy auth use
wendy auth use <context>
```

`status` lists your sign-ins and marks the current one. `use` opens a picker;
providing a name selects it directly. Use [`wendy auth rename`](./rename.md) to
make a saved context easier to recognize.

Commands use the current context, or the only saved context if there is just one.
With several contexts and none selected, interactive commands show a picker.
Scripts must select a context first or pass `--cloud-grpc` to select its endpoint.
If several contexts share an endpoint, the current matching context takes priority.

Wendy normally refreshes your session automatically. If a command asks you to
sign in again, rerun `wendy cloud login`; you do not need to log out first.

## Service-account login

Use a service account for unattended Cloud API calls in scripts and CI.
Ask your organization administrator to create one and provide a one-time
enrollment secret and its sign-in URL.

1. Register a key on the machine that will use it:

   ```sh
   wendy auth service-account enroll <one-time-secret> \
     --issuer https://auth.wendy.dev/realms/<realm> \
     --output wendy-service-account.json
   ```

2. Sign in using the saved key:

   ```sh
   wendy cloud login --service-account wendy-service-account.json
   ```

The secret is used once. The key file is private: keep it out of source control
and give it only to the intended CI job. You can instead store its contents in
`WENDY_SERVICE_ACCOUNT_KEY` and run `wendy cloud login`. Explicit personal-login
flags take precedence over that variable.

Service accounts obtain new access tokens automatically while their key remains
authorized. They do not receive the personal device certificate used by direct
device and Lite relay connections. Disabling an account prevents new tokens;
already issued tokens remain usable until expiry, normally up to one hour.

## Advanced: alternate environments

Production Wendy Cloud is the default. Your administrator may provide different
settings for development or a self-hosted deployment.

| Flag | Purpose |
| --- | --- |
| `--development` | Use Wendy's development environment. |
| `--issuer` | Use a particular organization sign-in URL. |
| `--client-id` | Use an administrator-registered login client. |
| `--auth` | Override the sign-in service address. |
| `--cloud`, `--cloud-grpc` | Override the dashboard and Cloud API endpoints. |
| `--resource` | Override the Cloud API authentication audience. |
| `--pki-identity-endpoint`, `--pki-resource` | Override the certificate service URL and audience. |
| `--api-key` | Use local authentication. |
| `--legacy` | Use the older dashboard sign-in flow. |

Current personal sign-ins support both device access and signed Cloud changes.
Legacy and local-API-key sign-ins cannot authorize all current Cloud operations;
sign in with the default browser flow if a command requires it.

See [certificate renewal](./refresh-certs.md) for connection troubleshooting.
