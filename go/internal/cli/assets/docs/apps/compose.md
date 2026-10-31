# Multi-Service Apps with Docker Compose

Wendy supports running multi-service applications defined in a standard `docker-compose.yml` (or `compose.yml`) file. This is the recommended approach when your app needs more than one container, for example an API service alongside a database, or a perception pipeline with several processing stages.

## How it works

When you run `wendy run` in a directory that contains a compose file, Wendy automatically detects it as a compose project. Each service is built, transferred to the device, and started in dependency order. No `wendy.json` is required; each service carries its own configuration, and an optional companion `wendy.json` in the same directory is merged in when present (see [Readiness probes and postStart hooks](#readiness-probes-and-poststart-hooks)).

Supported compose file names (checked in order, first found wins):
- `docker-compose.yml`
- `docker-compose.yaml`
- `compose.yml`
- `compose.yaml`

A compose file takes precedence over a `Dockerfile` in the same directory. To force the single-container Docker path instead, pass `--build-type docker` or `--dockerfile <path>`.

## Cloud v2 uploads

The Compose and multi-service `wendy.json` chunk-upload path requires the Docker
builder (`--builder docker`). If an upload drops over Cloud v2, the CLI reports
the connection loss and bytes sent; automatic registry fallback is off after
that failure. Check that the device is online with `wendy cloud discover`, then
retry, or select `--chunking=off` to upload through the registry with the usual
authorization.

In `auto`, old Agents missing the chunk RPCs keep the existing authorized
registry fallback. Apple Container keeps its direct registry route; existing
setups can use `--builder apple-container --chunking=off` explicitly. BuildKit
Compose registry uploads remain unsupported. Other image preparation failures
are reported separately.
See [Cloud v2 upload behavior](../clients/wendy-cli/commands/run.md#uploads-over-cloud-v2)
for exit status and diagnostics.

## Quickstart

```
my-project/
  docker-compose.yml
  api/
    Dockerfile
    main.py
  worker/
    Dockerfile
    worker.py
```

```yaml
# docker-compose.yml
services:
  api:
    build: ./api
    network_mode: host

  worker:
    build: ./worker
    network_mode: host
    depends_on:
      - api
```

```sh
wendy run
```

Wendy selects a device, builds each service image, pushes them to the device, and starts them concurrently with interleaved log output. Each service's lines are prefixed with a color-coded, column-aligned service name. Log lines are never interleaved mid-line; each line is written atomically:

```
[api]     Listening on :8080
[worker]  Connected to api
[worker]  Processing item 1
```

Service builds run in parallel, while the device-intensive image preparation
phase is limited to two services at a time so a high `--max-concurrency` value
does not overload device storage. A later run with unchanged source and runtime
configuration skips image building, transfer, and preparation when the service's
container and its content are still present on the device; missing or
unverifiable content fails closed and performs a normal preparation. Set
`WENDY_PUSH_SKIP=0` to disable this optimization while diagnosing builds.

Press **Ctrl-C** to stop all services. Services are stopped in reverse dependency order; the CLI prints `Stopping <name>...` for each service and then a final `Stopped N service(s).` summary.

## Detached mode hint

When all services start in detached mode (`--detach`), the CLI prints a hint showing how to stream logs:

```
Run 'wendy device logs' to stream logs.
```

## Service fields

Wendy honours the following compose fields. Fields not listed here are ignored.

| Field | Description |
|-------|-------------|
| `build` | Build context: a path string or a `{ context, dockerfile, args }` mapping. `dockerfile` may name a `Dockerfile`, `Containerfile`, a dot/hyphen variant of either, or a Stagefile (`prod.stagefile.yaml`), and must resolve inside the build context. See [Build files](#build-files) for how the default is chosen when `dockerfile` is omitted. Services without `build` use a pre-built image via `image`. |
| `image` | Pre-built image to pull and run on the device (e.g. `redis:7-alpine`). Public image names are normalised to their fully-qualified form automatically. |
| `command` | Override the container's default command. Accepts a string (shell-split) or a YAML sequence. |
| `environment` | Environment variables to inject. Parsed from key-value maps or `KEY=VALUE` lists. Applied in order: image env → compose env → Wendy system vars → framework vars (e.g., ROS2) → OTEL vars. OCI last-wins semantics apply. |
| `ports` | Records `host:container` mappings on a `network` entitlement. This does not implement bridge DNAT or publish an isolated container to the host browser. |
| `network_mode: host` | Adds a `host` network entitlement. |
| `volumes` | Named volumes are created as `persist` entitlements. Host bind mounts (paths starting with `.` or `/`) are silently skipped. |
| `depends_on` | Dependency order: list or condition-map form. Services are created in dependency order; detached starts follow the same order, but the condition-map's own health-check conditions (e.g. `condition: service_healthy`) are not evaluated; ordering is purely topological. To gate a service's own postStart hook on its readiness instead, see [Readiness probes and postStart hooks](#readiness-probes-and-poststart-hooks). |
| `restart` | Restart policy: `no`, `on-failure`, `always`, `unless-stopped`. Overridden by CLI flags if specified. |
| `x-wendy` | Wendy-specific per-service extensions: `readiness` (a readiness probe) and `hooks` (postStart hooks). Same camelCase schema as `wendy.json`'s top-level `readiness`/`hooks` fields. See [Readiness probes and postStart hooks](#readiness-probes-and-poststart-hooks) below. |

The following fields are recognized but intentionally ignored, each with a warning at deploy time: `devices`, `privileged`, `cap_add`, `security_opt`, `ipc`, `pid`, `shm_size`, `healthcheck`, `profiles`, `secrets`, and `extra_hosts`. Hardware access goes through Wendy entitlements in a companion `wendy.json` instead. Anything else (such as `networks`, `deploy`, `entrypoint`, `env_file`, or long-form volume syntax) is ignored silently.

## Build files

A service's `dockerfile` may name a Stagefile as well as a Dockerfile. Stagefiles are compiled to a Dockerfile before the image build, so nothing else about the service changes:

```yaml
services:
  api:
    build:
      context: ./api
      dockerfile: prod.stagefile.yaml
```

When `dockerfile` is omitted, Wendy resolves the default from the build context, in order:

1. `build.stagefile.yaml` — the canonical Stagefile.
2. Otherwise the first `<name>.stagefile.yaml` in the context, alphabetically. A context that holds only variants has no conventional default, and falling through to `Dockerfile` would silently ignore the Stagefiles plainly sitting there. Name one explicitly with `dockerfile:` to pick a different variant.
3. Otherwise `Dockerfile`.

Two services may share one build context and point at different build files. Each Stagefile compiles to its own artifact (`Dockerfile.generated` for the canonical one, `Dockerfile.generated.<variant>` otherwise), so concurrent service builds in a shared context never overwrite each other's compiled Dockerfile. See [Stagefile naming](../clients/wendy-cli/commands/build.md#stagefile-naming) for the filename rules.

`docker compose build` itself knows nothing about Stagefiles, so `wendy build` compiles them first and passes a generated override file pointing the affected services at their compiled Dockerfile. Running plain `docker compose build` on the same project — without Wendy — will fail on a `dockerfile:` that names a Stagefile.

## Networking

Set `network_mode: host` for services that must share host ports, use multicast, pass CLI readiness probes, or be reached from the developer's browser.

Omitting `network_mode` while declaring `ports` currently creates a legacy mode-less network entitlement, which maps to host networking with a deprecation warning; the mappings do not create an isolated bridge or DNAT rules. To request isolated outbound networking, declare `{ "type": "network", "mode": "bridge" }` in a companion `wendy.json`, without relying on Compose `ports:` for inbound access. Bridge mode supplies a private namespace, DNS, and outbound NAT only. `network.ports` mappings on mesh entitlements are for mesh-peer traffic, not host/LAN browser ingress.

## Volumes

Named volumes declared in `volumes:` become persistent storage on the device and survive container restarts and re-deployments. Two services sharing a volume name share the same storage.

```yaml
services:
  producer:
    build: ./producer
    volumes:
      - shared-data:/data/out

  consumer:
    build: ./consumer
    volumes:
      - shared-data:/data/in

# Declare named volumes at the top level so the file stays valid for
# plain `docker compose` too. Wendy itself only reads the `services:`
# block; the service-level references above are what create the storage.
volumes:
  shared-data:
```

> Host bind mounts (e.g. `./local-path:/container/path`) are not supported on device; they are skipped.

## Readiness probes and postStart hooks

A compose service can declare its own readiness probe and postStart hooks under `x-wendy`, using the same camelCase schema as `wendy.json`'s `readiness`/`hooks` fields:

```yaml
services:
  frontend:
    build: ./frontend
    ports:
      - "3000:3000"
    x-wendy:
      readiness:
        tcpSocket:
          port: 3000
        timeoutSeconds: 30
      hooks:
        postStart:
          openURL: "http://${WENDY_HOSTNAME}:3000"
```

`readiness` here gates only `frontend`'s own `postStart` hook; it never delays any other service's startup (`depends_on` ordering is unaffected). `hooks.postStart.agent` runs directly on the device host and is triggered only by the declaring service's own container start.

A companion `wendy.json` in the same directory can also declare `services.<name>.readiness` / `services.<name>.hooks` for a service. When both are present, the companion wins wholesale per field: its `readiness` and `hooks` each replace the `x-wendy` value entirely rather than merging with it.

A companion `wendy.json`'s *top-level* `readiness`/`hooks` or `http` entitlement act instead as an app-level fallback: they fire once after every service in the project has started, rather than gating any single service. Inherited top-level HTTP remains in every service's container create configuration, but it is not executed once per service. A companion `services.<name>.entitlements` HTTP declaration is service-scoped; if both scopes declare HTTP, both actions run at their respective scopes. `Examples/WendyMC` in the repository uses a top-level `{ "type": "http", "port": 8080 }` entitlement with `readiness.timeoutSeconds: 180`, so the CLI preserves that timeout and opens the UI once after both services start. A top-level `hooks.postStart.agent` is the one exception: a compose app has no single app-level container start to trigger it, so it is ignored (with a warning); declare an agent-side hook under a service's `x-wendy.hooks` or the companion's `services.<name>.hooks` instead.

In attached mode, each service's readiness→postStart sequence runs asynchronously right after that service's start is acknowledged, so a slow or failing probe never delays starting the next service; Ctrl-C cancels any in-flight readiness wait and kills `cli` hook child processes. In detached mode none of this runs: no readiness wait, no `App reachable` line, and no host-side `postStart` action — only the agent-side `postStart.agent` hooks run, on the device. With `wendy run --watch`, each container's `openURL` and `cli` actions run once per session after its first successful readiness check; later saves do not repeat them, while a failed or canceled attempt may retry after a later deploy. `--watch --detach` skips these actions. A non-cancellation timeout in attached mode warns without failing the command: explicitly configured multi-service hooks still run, while `App reachable` and any synthesized HTTP browser open are suppressed. Cancellation suppresses all three. An explicit readiness TCP port remains the probe target; presentation prefers a hostname-templated `openURL`, then the HTTP entitlement port, then the readiness port. See [Watch mode](../clients/wendy-cli/commands/run.md#watch-mode) for details.

Hook commands may reference `${WENDY_HOSTNAME}` (the device host), `${WENDY_APP_ID}`, and `${WENDY_SERVICE_NAME}` (the declaring service's name; empty for the app-level fallback). Windows-style `%VAR%` forms are accepted too.

For a compose file with more than one service, `WENDY_APP_ID` is the project directory name and `WENDY_SERVICE_NAME` is the service name. A **single-service** compose project without a companion `wendy.json` uses `<project>-<service>` as `WENDY_APP_ID` (for example, `myproj-web`) and leaves `WENDY_SERVICE_NAME` empty. When a companion file is present, its `appId` is used and grouped naming applies even when there is only one service.

> Readiness probes and browser URLs dial the device host, not the container directly, so browser-reachable HTTP currently requires `network_mode: host`. Bridge mode provides outbound NAT only, and mesh port mappings serve mesh peers rather than the developer's browser.

## Flags

All `wendy run` flags work with compose projects:

| Flag | Description |
|------|-------------|
| `--deploy` | Build and create all containers but do not start them. |
| `--detach` | Start all containers but do not stream logs. See [Readiness probes and postStart hooks](#readiness-probes-and-poststart-hooks). |
| `--restart-unless-stopped` | Set restart policy to `unless-stopped` for all services (overrides per-service setting). |
| `--restart-on-failure` | Set restart policy to `on-failure` for all services. |
| `--no-restart` | Disable restart for all services. |
| `--debug` | Enable debug logging during build and run. |
| `--yes` / `-y` | Accept all device-selection prompts automatically. |
| `--watch` | Redeploy on changes and stream app logs between deploys. Requires a Wendy agent target when attached; use `--watch --detach` for provider targets. |

## Limitations

- Headless Mac is not supported. `wendy run` rejects compose projects before any registry or Docker setup when the selected target is Headless Mac. Target a Linux/WendyOS device to use compose.
- Wendy-specific hardware access entitlements such as `gpu`, `display`, `camera`, `audio`, `bluetooth`, `usb`, `i2c`, `gpio`, `spi`, `input`, and `serial` are not inferred from compose fields.
- Host networking does not imply shared IPC or shared `/dev/shm`; ROS 2 shared-memory transport requires an app shape that can explicitly share namespaces.
- Linux containers on macOS require a target WendyOS device; local Docker Desktop compose is used as a fallback when no device is targeted.
- Compose `extends`, `profiles`, and `secrets` are not supported.
- `wendy fleet run` does not support compose projects yet; deploy to one device at a time.
- `wendy run --service` applies only to `wendy.json` `services` maps; the compose path always deploys every service.
- `wendy build` for a compose project shells out to `docker compose build`. The Apple Container builder is not supported for compose; use `--builder docker`.
