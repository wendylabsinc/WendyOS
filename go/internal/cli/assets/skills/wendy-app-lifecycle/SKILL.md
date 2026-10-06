---
name: wendy-app-lifecycle
description: Use when a developer wants to build, deploy, run, detach, stream logs, start, stop, list, remove, or clean up Wendy apps with `wendy build`, `wendy run`, `wendy device apps list`, `wendy device logs`, telemetry streams, or volumes.
---

# Wendy App Lifecycle Workflow

Use this for normal operator/developer workflows around Wendy apps, not only debugging. Prefer explicit, reproducible CLI commands that work in agent/non-interactive contexts.

## ChatGPT plugin development loop

When using the ChatGPT gateway, work through its tools without assuming a local
terminal. The gateway exposes different tools from `wendy mcp serve`.

1. For a simulator, use `simulator_list`, create only if needed, then
   `simulator_start`. Refresh `list_robots` to resolve its exact `robot_id`.
   Use `simulator_viewer` for a robot profile and inspect camera output when
   available. A `vm:<name>` selector is not a gateway `robot_id`.
2. Call `list_workspaces` and choose an approved workspace whose `robot_ids`
   includes the selected target. `can_read_files` and `can_write_files` report
   local file permissions. Missing permissions require operator configuration;
   do not substitute an arbitrary path or another target.
3. Use `list_workspace_files` and `read_workspace_file` to inspect the project.
   Author or edit code with `write_workspace_file`, using the read's SHA-256 as
   `expected_sha256`, or `"missing"` for a new file. Re-read on a revision conflict.
   Create app configuration and build inputs along with source. Each file must
   be UTF-8 text of at most 256 KiB within the approved directory.
4. Validate with `validate_device_project(workspace_id, robot_id)`. Resolve its
   findings before building. Compatibility reported as unknown stays unknown.
5. Call `start_device_deployment` with those exact IDs and a fresh stable
   `request_id`. Keep the returned job ID and poll `get_deployment_job`. Reuse the
   request ID after an uncertain response, not for a new build of edited code.
   `cancel_deployment_job` cancels the build/deploy command; it may leave an app
   already running on the target.
6. Check `inspect_robot`, `read_device_logs`, and actual app or robot output.
   `open_robot_app` opens a running app's declared HTTP UI. The gateway's
   `completed` deployment status and `RUNNING` app state do not prove readiness.

Local file tools require an approved workspace, local stdio, and `projects:read`
or `projects:write`. Deployment independently needs `apps:deploy`. A workspace's
`allow_simulators` opt-in permits its use on running local simulators; configured
physical or VM targets use its explicit `robots` list and subject grants. A
remote HTTP connection cannot edit unshared laptop files. Explain a missing
local connection or workspace grant when the tools cannot perform the task.

## Core rule for coding agents

Avoid Bubble Tea pickers and dashboards unless the user explicitly wants an interactive terminal UI. Coding agents should generally use:

- An explicit `--device <selector>` so a stale default cannot redirect deployment.
- `--json` for list/status/log records when supported.
- `--yes` for `wendy run` when it may need to create `wendy.json` or accept prompts.
- Explicit app names for `device apps start|stop|remove`.
- Explicit cleanup flags for destructive commands.
- `device logs --no-follow --tail <N>` for a finite diagnostic sample; a timeout or background process only when intentionally following live logs.

`--json` is a global flag. Prefer putting it near the root command:

```bash
wendy --json device apps list --device <hostname>
```

## Build

Use `wendy build` when the developer wants to compile/build without deploying or starting the app.

```bash
wendy build --device <hostname>
```

If multiple project markers exist, avoid interactive build pickers:

```bash
wendy build --build-type docker --device <hostname>
wendy build --build-type compose --device <hostname>
wendy build --build-type swift --device <hostname>
wendy build --build-type python --device <hostname>
```

Accepted build types are `docker`, `compose`, `swift`, and `python`. Current CLI help text may lag and omit `compose`, but the build/run code accepts it when a Compose file is present.

Important: `wendy build` has a global `--json` flag available from the root command, but the build command does not currently produce structured JSON output. Do not rely on JSON parsing for build success. Use exit status and the text output.

## Run and deploy

Attached run, for foreground development and live stdout/stderr:

```bash
wendy run --yes --device <hostname>
```

Detached run, for long-running services where the agent should regain control:

```bash
wendy run --yes --detach --device <hostname>
```

Deploy only, create the container but do not start it:

```bash
wendy run --yes --deploy --device <hostname>
```

Force a build type:

```bash
wendy run --yes --build-type docker --device <hostname>
wendy run --yes --build-type compose --device <hostname>
wendy run --yes --build-type swift --device <hostname>
wendy run --yes --build-type python --device <hostname>
```

Other useful flags:

```bash
wendy run --yes --prefix ./path/to/app --device <hostname>
wendy run --yes --product MySwiftProduct --device <hostname>
wendy run --yes --user-args foo,bar --device <hostname>
wendy run --yes --user-args foo --user-args bar --device <hostname>
wendy run --yes --debug --device <hostname>
wendy run --yes --restart-unless-stopped --detach --device <hostname>
wendy run --yes --restart-on-failure --detach --device <hostname>
wendy run --yes --no-restart --device <hostname>
wendy run --yes --chunking off --device <hostname>     # registry push only, skip chunk-diff
wendy run --yes --chunking force --device <hostname>   # chunk-diff only, no fallback
```

Attached `wendy run` starts the container and streams output. Ctrl+C stops the container. Detached `wendy run --detach` returns after starting; it skips waiting for readiness and opening the app URL. Verify container state, startup logs and an application health check separately.

The MCP `run` tool uses an explicit `device` selector or the currently connected
target, including its cloud endpoint when applicable. Legacy `device_name` selects
a cloud device. Inspect the returned target and status. A created or started
container does not establish sensor, HTTP or robot behavior; use `wendy-robot-deploy`
for robot checks. If the installed server exposes only the older cloud-only run
tool, use the CLI with an explicit target or update and restart the MCP host.

`--deploy` creates the container but does not start it. To start that existing app later without attaching to its output, use `wendy device apps start <app-id> --detach --device <hostname>` (see Manage apps).

`--user-args` is repeatable and also accepts comma-separated values. Prefer repeated flags when values could contain commas. The values are appended to the image's own entrypoint/`CMD`, not substituted for it, so `--user-args --port,8080` runs `<image entrypoint> --port 8080`.

## Recover from port conflicts

Use startup logs and app inventory on the explicit target to identify the
occupied port and its owner. With host networking, apps on the device share
listening ports. A VM forwarding error can instead concern a port on the
developer's host. Preserve unrelated listeners when choosing another app port.

When moving an app's listener, keep its configuration consistent:

1. Change the actual server port in source, environment or startup arguments.
2. Update affected `http` entitlement ports, explicit port mappings,
   `hooks.postStart.openURL` ports and client URLs. Change
   `readiness.tcpSocket.port` when it probes the moved listener; keep a probe
   for a separate health port unchanged.
3. Run `wendy json validate`, redeploy to the same explicit device, and check
   app state and startup logs for a successful bind on the new port.
4. Fetch the new endpoint through a route reachable from the caller, verify
   application-specific response content, and confirm the original listener
   still works. For a user-networked VM, use its host loopback forward rather
   than treating `vm:<name>` as a DNS name.

A passing TCP readiness probe only proves that something accepts connections.
The existing port owner can satisfy it while the new app crashes; verify the
new app's state and response before claiming success.

## Stream logs

For a bounded, machine-readable diagnostic sample, use `--tail` with `--no-follow`:

```bash
wendy --json device logs --app <app-id> --tail 50 --no-follow --device <hostname>
wendy --json device logs --app <app-id> --service <service-name> --level warn --tail 50 --no-follow --device <hostname>
```

Use `--level` or `--min-severity` to narrow the sample. Only omit `--no-follow` when continuous live output is needed:

```bash
wendy --json device logs --app <app-id> --device <hostname>
```

Bound intentional live streams with the surrounding tool's timeout or a background process. On macOS, GNU `timeout` may not be installed.

For structured telemetry streams:

```bash
wendy device telemetry-stream --logs --app <app-id> --device <hostname>
wendy device telemetry-stream --logs --metrics --app <app-id> --device <hostname>
wendy device telemetry-stream --app <app-id> --service <service-name> --device <hostname>
```

`telemetry-stream` emits JSONL by design. It does not need `--json`. If none of `--logs`, `--metrics`, or `--traces` is set, the command enables all three streams.

## Manage apps

List apps without opening the interactive dashboard:

```bash
wendy --json device apps list --device <hostname>
```

Start an existing app by name and return as soon as the agent confirms it started:

```bash
wendy device apps start <app-id> --detach --device <hostname>
```

`--detach` (`-d`) does not stream the app's output, and it starts the app with the `unless-stopped` restart policy: the agent restarts it whenever it exits until you run `wendy device apps stop`. Without `--detach`, `device apps start` attaches to the app's output until the container exits (a multi-service app returns right away), so run it only as a bounded or background task.

Stop an app:

```bash
wendy device apps stop <app-id> --device <hostname>
```

Remove an app without prompts:

```bash
wendy device apps remove <app-id> --force --device <hostname>
```

Remove an app and its image:

```bash
wendy device apps remove <app-id> --force --cleanup --device <hostname>
```

Remove an app, image, and persistent volumes:

```bash
wendy device apps remove <app-id> --force --cleanup --delete-volumes --device <hostname>
```

Do not delete volumes unless the user asked for data cleanup or the app data is disposable.

## Manage volumes

List volumes in JSON:

```bash
wendy --json device volumes list --device <hostname>
```

Remove a named volume without prompts:

```bash
wendy device volumes remove <volume-name> --force --device <hostname>
```

Volume removal is destructive. Confirm user intent unless the user already asked for cleanup.

## Interactive vs JSON behavior

Known non-interactive guidance:

- `wendy --json device apps list` avoids the Bubble Tea apps dashboard.
- `wendy --json device volumes list` prints JSON.
- `wendy --json device logs` prints JSON log records, but still streams.
- `wendy device telemetry-stream` prints JSONL without `--json`.
- `wendy build` can still use Bubble Tea spinners in a TTY and does not currently provide structured JSON output.
- Agent-backed `wendy --json run --detach` returns one JSON result with `status`, `app`, `device`, `readiness` and `endpoints`; `url` is the first available HTTP endpoint and is omitted when no host URL can be determined. Progress goes to stderr. Attached run and local container providers still use progress/log output.
- `wendy run --yes` avoids app-config creation prompts where possible.
- `--json` also prevents device picker fallback; if no device/default is configured, pass `--device` or set a default first.
- `device apps start|stop|remove` and `device volumes remove` can prompt for a name if omitted; pass the app or volume name explicitly in agent workflows.

## Operator flow

For a typical coding-agent loop:

1. Verify CLI and device:

```bash
wendy --version
wendy discover --json
```

2. Build/deploy detached:

```bash
wendy run --yes --detach --device <hostname>
```

3. Confirm app state:

```bash
wendy --json device apps list --device <hostname>
```

4. Stream a bounded log sample:

```bash
wendy --json device logs --app <app-id> --tail 50 --no-follow --device <hostname>
```

5. Stop or remove only when requested:

```bash
wendy device apps stop <app-id> --device <hostname>
wendy device apps remove <app-id> --force --device <hostname>
```

### VM selectors and HTTP verification

`vm:dev` is a Wendy device selector, not a DNS name. In default `--net user`
mode the guest's `10.0.2.15` is behind QEMU NAT. Wendy forwards declared app ports
to host loopback; the agent's forwarded gRPC port is separate from the app port.
Use an `http` entitlement for each web port (and host networking or the appropriate
container-to-guest port publication). For example, an app serving guest port 18880
can have a host URL of `http://127.0.0.1:18880`.

```bash
wendy --json --device vm:dev run --yes --detach > deploy.json
url=$(jq -er '.url' deploy.json)
curl --fail --retry 10 --retry-connrefused --retry-delay 1 --max-time 5 "$url"
```

Check `device` and `app` in the result, then check the HTTP response against the
requested behavior or a unique fixture marker. Detached output says
`readiness: "not_checked"`: it reports the configured endpoint after start was
acknowledged and does not wait for health or execute host postStart hooks. For
multi-service apps, inspect each entry in `endpoints`. A TCP readiness probe alone
does not establish an HTTP endpoint. If `url` is absent, inspect configuration and
logs rather than constructing a URL from the VM selector. A host port conflict
must be resolved by changing the app port or freeing your own listener; do not
stop unrelated services or assume a different VM owns the same localhost port.
