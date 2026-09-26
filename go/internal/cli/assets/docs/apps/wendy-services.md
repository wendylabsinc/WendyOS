# Multi-Service Apps with `wendy.json`

When your project needs more than one container managed through a `wendy.json` file (rather than a `docker-compose.yml`), declare a `services` map in `wendy.json`. `wendy run` detects the map and automatically orchestrates a parallel multi-service build and deployment. `wendy build` detects the same map and builds the images locally without deploying — see [Building without deploying](#building-without-deploying).

## `wendy.json` structure

```json
{
  "appId": "com.example.myapp",
  "platform": "linux",
  "services": {
    "db": {
      "context": "db"
    },
    "api": {
      "context": "api",
      "dependsOn": ["db"],
      "entitlements": [
        { "type": "network", "mode": "host" }
      ]
    },
    "frontend": {
      "context": "frontend",
      "dependsOn": ["api"]
    }
  }
}
```

### `services` map

Each key is a service name. Each value is a `ServiceConfig` object:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `context` | string | **yes** | Build context directory, relative to `wendy.json`. Must be a relative path and must not contain `..` components. |
| `entitlements` | array | no | [Entitlements](../device/entitlements.md) to apply to this service's container. Same schema as the top-level `entitlements` field. |
| `dependsOn` | array of strings | no | Names of other services in this `services` map that must be created before this one. All referenced names must exist in the same map. |
| `env` | object | no | Environment variables for this service's container. Overrides the top-level [`env`](./wendy.json.md#env) per key, so a service can change one variable without dropping the rest. |
| `readiness` | object | no | Readiness probe for this service. Same schema as the top-level `readiness` field; see [Readiness and lifecycle hooks](#readiness-and-lifecycle-hooks). |
| `hooks` | object | no | Lifecycle hooks for this service. Same schema as the top-level `hooks` field. |
| `resources` | object | no | CPU/memory/PID ceilings for this service. Overrides the top-level [`resources`](./wendy.json.md#resources) per field. |
| `frameworks` | object | no | Framework configuration (e.g. ROS 2) for this service, replacing the top-level `frameworks`. |

### Validation rules

`wendy.json` validation rejects the following:
- A service with an empty or missing `context`.
- A `context` that is an absolute path.
- A `context` that contains `..` path components.
- A `dependsOn` entry that references a service name not present in the `services` map.
- A service `entitlements` entry with an unknown or missing type.
- A service `persist` entitlement missing `name` or `path`, or with a non-absolute or `..`-containing `path`.
- A service `network` entitlement with a `mode` other than `"host"`, `"host-admin"`, `"none"`, `"bridge"`, or `"mesh"` (the same set the top-level entitlement accepts).
- A service `i2c` entitlement with a device not in `i2c-N` format.
- A service `serial` entitlement with a device not matching the USB-only `ttyACM0` / `ttyUSB0` (`tty*N`) pattern.
- A service `mcp` entitlement with a port outside the range 1–65535.
- More than one `mcp` entitlement within a single service's `entitlements` array.

`ValidateJSON` additionally warns on deprecated entitlement types and unknown entitlement keys within service-level `entitlements` arrays, using the same rules applied to the top-level `entitlements` field.

## Readiness and lifecycle hooks

Any service in the `services` map may declare its own `readiness` probe and `hooks.postStart`, using the same schema as the top-level `readiness`/`hooks` fields:

```json
{
  "appId": "com.example.stack",
  "services": {
    "db": { "context": "db" },
    "cache": { "context": "cache" },
    "api": { "context": "api", "dependsOn": ["db", "cache"] },
    "frontend": {
      "context": "frontend",
      "dependsOn": ["api"],
      "readiness": {
        "tcpSocket": { "port": 3000 },
        "timeoutSeconds": 30
      },
      "hooks": {
        "postStart": {
          "openURL": "http://${WENDY_HOSTNAME}:3000"
        }
      }
    }
  }
}
```

Only a service that declares `readiness`, `hooks`, or a service-level `http` entitlement runs the readiness→postStart sequence; `db`, `cache`, and `api` above are unaffected.

### Scoping

- `readiness` gates only the declaring service's own `postStart` hook — it never delays other services' startup order (`dependsOn` ordering is a separate mechanism).
- A service-level `http` entitlement waits, announces, and opens only for that service. A top-level `http` entitlement remains in every container's create configuration but runs as an app-level action once, after every service starts. If both scopes declare HTTP, both actions run at their respective scopes.
- An explicit `readiness.tcpSocket.port` remains the probe target even when the same scope declares HTTP. The displayed/opened URL prefers an explicit hostname-templated `openURL`, then the HTTP port, then the readiness port; probing `9000` while opening `8080` is supported.
- `hooks.postStart.agent` runs directly on the device host and is delivered only with the declaring service's own container start call; starting another service does not trigger it.
- Hook commands may reference `${WENDY_HOSTNAME}` (the device host), `${WENDY_APP_ID}`, and `${WENDY_SERVICE_NAME}` — the declaring service's name, empty for single-container apps and for the app-level fallback below. Windows-style `%VAR%` forms are accepted too.

### App-level fallback

A top-level `readiness`/`hooks` or `http` entitlement in `wendy.json` acts as an app-level fallback: it fires once after every service has started, rather than gating any single service. Both the fallback and a service's own lifecycle configuration fire if both are declared. Two exceptions:

- A top-level `hooks.postStart.agent` is ignored for multi-service apps — there is no single app-level container start to trigger it. `wendy run` warns about this when it loads `wendy.json`; declare it under `services.<name>.hooks` instead.
- The fallback is skipped when `wendy run --service` selects a subset of services, since "every service has started" can't be guaranteed on a partial run.

### Attached vs. detached

In attached mode, each service's readiness→postStart sequence fires asynchronously right after that service's start is acknowledged, so a slow or failing probe never delays starting the next service. Ctrl-C cancels any in-flight readiness wait and kills `cli` hook child processes. If the run ends on its own — every service's log stream closes — while a hook (per-service or the app-level fallback) is still waiting on readiness, that hook is suppressed rather than fired, so `wendy run` never opens a browser onto a stack that has already exited. In detached mode none of this runs: no readiness wait, no `App reachable` line, and no host-side `postStart` action. Only the agent-side `postStart.agent` hooks, carried on each start RPC, still run on the device. With `wendy run --watch`, each service's `openURL` and `cli` actions run once per session after its first successful readiness check; later saves do not repeat them, while a failed or canceled check may retry after a later deploy. `--watch --detach` skips these actions. A non-cancellation readiness timeout in attached mode warns but does not fail the command: explicitly configured multi-service `postStart` hooks still run, while `App reachable` and any HTTP-entitlement-synthesized browser open are suppressed. Cancellation suppresses the warning, announcement, and hook.

## How `wendy run` handles multi-service projects

When `wendy.json` defines a `services` map, `wendy run` routes to the multi-service pipeline:

1. **Parallel build** — all service images are built and pushed concurrently, with up to 4 simultaneous builds by default. Override with `--max-concurrency`. In interactive terminals a per-service spinner displays each service's status (`waiting` → `building…` → `built (Xs)` / `failed`). While a service builds, its row names the Dockerfile step currently running and, where that step's output exposes it, appends live progress after a `·` separator:

    ⠹ api         [4/9] RUN pip install -r requirements.txt · 61%  128.0MB/797.3MB  95.2MB/s

In non-interactive terminals plain log lines are printed instead.
2. **Ordered container creation** — containers are created one at a time in topological dependency order. A service listed in another service's `dependsOn` is created first.
3. **Start and stream** — all containers are started and their combined stdout/stderr is multiplexed to the terminal. Each line is prefixed with `[serviceName]`.

Press **Ctrl-C** to stop all services. The CLI cancels all streams and stops each service concurrently before exiting.

### Container naming

Each service container ID follows the `{appId}_{serviceName}` convention (`_`
is the separator because `/` is not permitted in containerd container IDs). For
example, with `appId: "com.example.myapp"` and service `"api"`, the containerd
container ID is `com.example.myapp_api`.

> **Note:** Single-container apps (no `serviceName` in the top-level
> `wendy.json`) are unaffected — their container ID remains the bare `appId`.

## Building without deploying

`wendy build` detects the same `services` map and builds every service (or a `--service` closure) into the local image store, tagged `<appid>-<service>:latest`, without deploying: nothing is pushed to a registry and nothing is created or started on a device. Useful for CI and pre-flight checks — confirm every service builds before running `wendy run`:

```sh
wendy build
wendy build --service api
```

See [`wendy build`](../clients/wendy-cli/commands/build.md#multi-service-manifests) for the full flag reference (`--service`, `--max-concurrency`, `--builder`, `--gpu-arch`, `--debug`).

## Filtering with `--service`

To build and run only a specific service (and its transitive dependencies):

```sh
wendy run --service api
```

This resolves `api` and all services reachable through its `dependsOn` graph. Services outside this subset are not built or started. Passing an unknown service name returns an error immediately.

## Flags

All standard `wendy run` flags apply. The following are particularly relevant for multi-service projects:

| Flag | Description |
|------|-------------|
| `--service <name>` | Build and run only the named service and its transitive `dependsOn` dependencies. |
| `--build-host <device>` | Build the selected services on another device, which delivers each image to the target. See [Remote build host](#remote-build-host). |
| `--deploy` | Build and create all containers but do not start them. |
| `--detach` | Start all containers but do not stream logs. See [Attached vs. detached](#attached-vs-detached). |
| `--keep-going` | Deploy services that build successfully instead of aborting the whole group on the first build/push failure. |
| `--max-concurrency <n>` | Max service images to build+push at once. 0 = default limit of 4. |
| `--watch` | Redeploy changed services while continuing to show logs from the whole app. Use `--watch --detach` to skip logs and `openURL`/`cli` actions. |

## Example layout

```
my-project/
  wendy.json
  db/
    Dockerfile
  api/
    Dockerfile
  frontend/
    Dockerfile
```

```sh
wendy run            # builds and starts all three services
wendy run --service api   # builds db and api only (frontend excluded)
```

## Crash-looping services

When a service within a group crashes and the agent's restart policy is
automatically restarting it, that service's individual entry in
`wendy device apps list` shows a red `↻` **crash-looping** state (nested under
the group header). The top-level app entry stays `Running` as long as at least
one service is up; it flips to `Crash-looping` only when every service is down
and the restart policy is still restarting at least one of them.

`wendy device logs --app <appId>` surfaces crash output from all service
members of the group, so a crash-looping service's logs are reachable without
naming the individual service.

## Remote build host

`wendy run --build-host <device>` builds the selected services on another device instead of on your machine. Each service goes to the build host as its own build context, and the host delivers each image straight to the target as `<appid>-<service>:latest`, the same image a local build produces. `--service`, `--max-concurrency`, and `--keep-going` behave as they do for a local build. See [Remote build host](../clients/wendy-cli/commands/run.md#remote-build-host) for setup and requirements.

Two things differ from a local build:

- **One target per run.** `--device a,b,c` is refused for a multi-service project, because its services are created and started as a group against one device. Run `wendy run` once per device instead.
- **No push-skip.** A local build skips a service whose inputs are unchanged and whose layers the device already holds. A remote build cannot prove that, so every selected service is sent to the build host on each run; the host's BuildKit cache and chunked delivery keep an unchanged service fast. Under `--watch`, a service already confirmed unchanged and running in the session is still left alone.

## Limitations

- Log output is multiplexed with a `[serviceName]` prefix on each line. Per-service log stream routing is not yet available.
- Containers are created one at a time in dependency order, not atomically as a group. Grouped atomic creation is planned as a follow-up.
- Headless Mac is not supported. `wendy run` rejects multi-service `wendy.json` projects when the selected target is Headless Mac, before any build or registry operation. Target a Linux/WendyOS device for multi-service workloads.
