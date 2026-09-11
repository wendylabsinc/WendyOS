# ComposeEnv

Demonstrates that `environment:` values in `docker-compose.yml` are now
forwarded to each container by Wendy (WDY-1268, PR #897).

```
ComposeEnv/
├── docker-compose.yml   ← defines environment: vars for the server
├── wendy.json           ← companion: appId + port entitlement
├── server/              ← HTTP server that returns its own env vars as JSON
└── client/              ← fetches the server response and validates the vars
```

## What changed (WDY-1268)

Before PR #897, Wendy silently ignored `environment:` entries in compose files
— the `TODO` in the CLI read:
> *"compose `environment:` values aren't sent to the device yet"*

Now `wendy run` reads each service's `environment:` block and passes it
as the new `env` field on `CreateContainerRequest`. The agent applies them in
the order: **image built-in env → user compose env → Wendy system env →
framework env (e.g., ROS2) → OTEL env**. OCI last-wins semantics apply.

## Unsupported field warnings (WDY-1270)

If your compose file uses fields Wendy doesn't honour (`devices`, `privileged`,
`ipc`, `cap_add`, `cap_drop`, `sysctls`, `security_opt`, `cgroup`, `pid`), the
CLI now prints a warning per service rather than silently ignoring them:

```
warning: service "server" uses unsupported Compose fields (ignored by Wendy): privileged
```

The deploy still proceeds — the warning is informational.

## Run on a Wendy device

```sh
cd Examples/ComposeEnv
wendy run
```

Expected output:

```
[server] listening on :8080
[server] APP_MODE    = production
[server] GREETING    = Hello from Wendy!
[server] MAX_WORKERS = 4
[client] WENDY_HOSTNAME        = client.local
[client] WENDY_DEVICE_HOSTNAME = wendyos-<name>.local
[client] waiting for server at http://wendyos-<name>.local:8080 ...
[client] server response:
{
  "from_compose_environment": {
    "APP_MODE": "production",
    "GREETING": "Hello from Wendy!",
    "MAX_WORKERS": "4"
  },
  "from_wendy_agent": {
    "WENDY_APP_ID": "sh.wendy.examples.composeenv",
    "WENDY_HOSTNAME": "server.local",
    ...
  }
}
[client] ✓ all compose environment: vars reached the server container
```

## Run locally with Docker Desktop

To verify global CLI overrides on both services, including repeated keys and an
explicit empty value (WDY-2907), deploy with:

```sh
wendy run --env CHECK_CLI_OVERRIDES=1 --env APP_MODE=first --env APP_MODE=cli --env GREETING= --env MAX_WORKERS=7
```

The client checks its own environment and the server response. It exits with
an error if either service missed an override. The same resolved environment
is used for build fingerprints and watch change detection.

`docker-compose.yml` is unmodified, so it works as-is with Docker Desktop
(environment vars are native Docker Compose behaviour):

```sh
docker compose up
```

The client falls back to `http://server:8080` when `WENDY_DEVICE_HOSTNAME` is
absent.

## See also

- [HelloCompose](../HelloCompose) — companion wendy.json pattern, GPU entitlements
