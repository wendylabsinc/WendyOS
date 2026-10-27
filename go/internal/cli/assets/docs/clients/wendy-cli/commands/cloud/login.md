# `wendy cloud login`

Sign in to Wendy Cloud to enroll devices, deploy remotely, and manage your
organization's devices.

```sh
wendy cloud login
```

Complete sign-in in the browser. Choose your organization if prompted, then
return to the terminal. Wendy saves your sign-in and uses it automatically for
later commands.

## Check or change your sign-in

```sh
wendy auth status
wendy auth use
```

Sign in again to add another organization without replacing your current one.
Use `wendy auth use` to choose between saved sign-ins. See
[sign-in contexts](../auth/login.md#auth-contexts) for details.

## Other sign-in options

```sh
wendy cloud login --email you@example.com
wendy cloud login --service-account ./wendy-service-account.json
wendy cloud login --legacy
```

`--email` supplies your email before opening the browser. A service-account key
supports unattended Cloud API access; it does not provide a personal device
certificate. See [service-account setup](../auth/login.md#service-account-login).

## Existing Cloud v1 devices

`wendy cloud login` signs in to Cloud v2. For devices still enrolled in Cloud
v1, `wendy cloud login --legacy` opens the legacy dashboard at `cloud.wendy.sh`.
The flag is hidden from ordinary CLI help but remains supported. It cannot be
combined with `--email`, `--issuer`, `--service-account`, or `--api-key`.

Signing in to v2 does not move v1 devices. See the [Cloud guide](/docs/cloud)
for the Cloud v2 and Legacy workflows, and [Moving to Cloud v2](/docs/cloud-migration)
before changing an existing device's enrollment.

## Advanced flags

Hosted Wendy Cloud uses production settings by default. Use overrides only
when your administrator supplies them.

| Flag | Description |
| --- | --- |
| `--development` | Use Wendy's development environment. |
| `--issuer` | Sign in through a particular organization sign-in URL. |
| `--service-account` | Service-account key file. `WENDY_SERVICE_ACCOUNT_KEY` can supply its contents instead. |
| `--cloud` | Override the dashboard URL, normally `https://cloud.wendy.dev`. |
| `--cloud-grpc` | Override the Cloud API endpoint, normally `api.wendy.dev:443`. |
| `--legacy` | Use the Cloud v1 dashboard sign-in flow for existing v1 devices. |

`wendy auth login` remains an equivalent command. See its
[reference](../auth/login.md) for additional options and saved sign-ins.
