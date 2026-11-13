---
name: wendy-app-lifecycle
description: Use when a developer wants to build, deploy, run, detach, stream logs, start, stop, list, remove, or clean up Wendy apps with `wendy build`, `wendy run`, `wendy device apps list`, `wendy device logs`, telemetry streams, or volumes.
---

# Wendy App Lifecycle Workflow

Build, deploy, inspect and remove Wendy apps from an agent. Pass an explicit
`--device <selector>` (host[:port], `vm:<name>` or `docker`) so a stale default
cannot redirect the work, put the global `--json` flag first, and pass `--yes`
to `wendy run`. With the Wendy MCP server connected, use the tools in the
wendy skill's [MCP reference](../wendy/references/mcp.md) instead. Deploying an
app does not deploy Wendy Data campaigns; use `wendy-data` for those.

## Deploy

```bash
wendy --json run --device <selector> --detach --wait-ready --yes
```

The result is one JSON object. `status` is `ready` (probe passed), `running`
(no probe; the app stayed up), `crashed`, `not_ready` (probe timed out, exit
code 8) or `failed`. `readiness` is `passed`, `not_checked` or `failed`.
Failures add `exit_code`, `termination_reason` and `message`. The probe is the
`readiness.tcpSocket.port` in `wendy.json`, or else the `http` entitlement's port.

`--wait-ready` covers single-container projects on WendyOS devices and
simulators. For multi-service and Compose projects, `--device docker`, or when
the CLI rejects the flag, deploy with `--detach` alone. The result then reports
`readiness: "not_checked"`: check the app list and a log sample before testing
the app.

| Need | Add |
|---|---|
| One service of a multi-service `wendy.json`, with its dependencies | `--service <name>` |
| Project in another directory | `--prefix <dir>` |
| Several build files apply | `--build-type docker` (or `compose`, `swift`, `python`) |
| Create without starting | `--deploy`, instead of `--detach --wait-ready` |
| Restart whenever the app exits | `--restart-unless-stopped` |
| Arguments appended to the image entrypoint | `--user-args <arg>`, repeatable |

Without `--build-type`, `wendy run` uses the first of: a Compose file, a
Stagefile, a Dockerfile or Containerfile, a Swift package, a Python project.
`--yes` writes a minimal `wendy.json` when none exists, using the directory name
as `appId`. Only `appId` is required. A typical service adds environment
variables, its HTTP port (forwarded from simulators) and a readiness probe:

```json
{
  "appId": "my-app",
  "version": "1.0.0",
  "env": { "PORT": "8080" },
  "entitlements": [{ "type": "network", "mode": "host" }, { "type": "http", "port": 8080 }],
  "readiness": { "tcpSocket": { "port": 8080 }, "timeoutSeconds": 60 }
}
```

Choose other entitlements with `wendy-entitlements`, read the
[wendy.json reference](../wendy/references/wendy.json.md) only for fields not
shown here, and check edits with `wendy json validate`.

Do not run `wendy run` without `--detach` from an agent: it streams the app's
output until the app exits, and interrupting it stops the app.

`wendy build --device <selector>` builds without deploying. It prints text, not
JSON, so rely on its exit status.

## Verify the app

Readiness means that something accepts connections on the probed port, not that
the app works: an existing listener can pass the probe while the new app
crashes. Check the requested behavior once, such as one HTTP request to the
result's `url` that checks a field unique to this app.

On a simulator, `url` is a host loopback forward such as
`http://127.0.0.1:18880`. Do not use `vm:<name>` or the guest's `10.0.2.15` as a
hostname. An app port is forwarded only when `wendy.json` declares it in an
`http` entitlement. If `url` is missing, inspect the configuration and logs
rather than constructing one.

## Local Docker provider

`wendy run --device docker --detach --yes` builds with Docker and starts the
container on this computer. It prints Docker's build output and the container
ID, not the JSON result. It publishes no ports and sets no restart policy: for
host ports, a container name or a restart policy, write a `compose.yaml` and
add `--build-type compose`. `wendy device logs` and `--wait-ready` do not
support this provider, so use `docker logs --tail 50 <container>` and
`docker inspect`.

## Inspect and control apps

```bash
wendy --json device apps list --device <selector>
wendy --json device logs --app <app-id> --tail 50 --no-follow --device <selector>
wendy device apps start <app-id> --detach --device <selector>
wendy device apps stop <app-id> --device <selector>
wendy device apps remove <app-id> --force --device <selector>
```

- `--tail` counts stored log batches, which can include earlier runs of the
  app. Narrow the sample with `--service <name>` and `--level warn`. Omit
  `--no-follow` only to follow live output, bounded by a timeout or a
  background process.
- `apps start --detach` sets the `unless-stopped` restart policy. Without
  `--detach` it attaches until the app exits.
- `apps remove --cleanup` also deletes the image, and `--delete-volumes` deletes
  persistent data. Delete volumes only when the user asked for data cleanup.
  `wendy --json device volumes list` and
  `wendy device volumes remove <name> --force` manage volumes.

For a port already in use, a debugger (`--debug`) or OpenTelemetry export, read
[references/troubleshooting.md](references/troubleshooting.md).
