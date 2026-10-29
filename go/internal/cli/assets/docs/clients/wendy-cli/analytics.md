# Usage analytics

Control whether Wendy sends anonymous usage analytics:

```sh
wendy analytics status
wendy analytics disable
```

Run `wendy analytics enable` to opt in again. `WENDY_ANALYTICS=false` and CI also
turn collection off; `status` shows the effective setting. Analytics failures do
not change a command's result. Diagnostic reports are not uploaded automatically.

## Event reference

The details below describe the event fields for administrators reviewing data
collection. Normal use does not require configuring these events.

## How it works

When analytics are enabled, each tracked event is serialised to JSON and sent via an HTTP POST request. Delivery is best-effort: a network failure, timeout, or non-2xx response never changes the command's result.

The CLI sends `cli_seen` before command execution or argument validation, including help, version, unknown commands, and invalid flags. Unknown commands use the last recognized command path; the unknown text is never sent. `command_executed` records the outcome before optional update or completion prompts. Help and version invocations do not count as successful product operations. Pending events are flushed before sudo process replacement or a Windows elevation launch.

MCP tool calls emit `command_executed` events as each call finishes, using a command name such as `wendy mcp device_info` and `command_root` of `mcp`. The duration and success flag describe the individual call, including tool results that report an error and panics recovered by the MCP server. Calls to app-provided tools use the fixed name `wendy mcp container_tool` so app and tool names stay private. Tool arguments and result content are never sent. The same analytics opt-out and CI settings apply to MCP calls.

`wendy doctor` and `wendy update --check` skip analytics entirely. These read-only
invocations also skip update and syntax-error cache writes and routine sudo
ownership cleanup.
Installation plans and privileged internal helpers also skip analytics.
`wendy analytics disable` does not send an event about disabling analytics.
An unreadable or malformed config prevents telemetry initialization, so a stored
opt-out cannot be accidentally ignored.

`wendy analytics status` reports the effective state, including CI detection,
the environment opt-out, and an analytics initialization failure. A saved enabled
preference does not override these conditions.

## Endpoint

Events are posted to:

```
https://wendy-cli-telemetry-114319063177.us-central1.run.app/v1/telemetry/events
```

The anonymous ID always lives in `~/.wendy/analytics_id`. `WENDY_CONFIG_DIR`
continues to select device settings, analytics preferences, and milestone state,
but does not select a separate identity. If the shared ID does not exist yet,
the CLI preserves an existing ID from the active config directory. Once a shared
ID exists, it takes precedence over IDs in other config directories.

## Event payload

Every event is an anonymous JSON object. The fields sent are:

| Field | Type | Description |
|-------|------|-------------|
| `schema_version` | integer | `2` for the lifecycle-aware event contract |
| `event_id` | string | Random event UUID, allowing the receiver to discard duplicate delivery |
| `occurred_at` | string | UTC event time recorded by the CLI, independent of delivery time |
| `attempt_id` | string | Random UUID shared by one invocation or tour and its child operations |
| `anonymous_id` | string | Stable random UUID stored in `~/.wendy/analytics_id`, shared by CLI and MCP even with different `WENDY_CONFIG_DIR` settings |
| `event` | string | Event name, e.g. `"command_executed"` |
| `command_name` | string | Canonical command path, e.g. `"wendy device wifi connect"` |
| `command_root` | string | Top-level command token |
| `duration_ms` | integer | Command duration in milliseconds |
| `success` | boolean | Whether the command completed without error |
| `error_class` | string | Bounded enum describing the error category (never free-form error text) |
| `cli_version` | string | CLI version string |
| `os` | string | Operating system (`GOOS`) |
| `arch` | string | CPU architecture (`GOARCH`) |
| `is_dev_build` | boolean | `true` when `cli_version` is `"dev"` or has a `-dev` suffix |
| `properties` | object | Event-specific string values, sent only by events that define them (see [Deploy events](#deploy-events)). At most 32 allowlisted entries of up to 64 bytes, or 128 bytes for canned remediation; invalid values are discarded |

> **Privacy note:** Flag values, positional arguments, file paths, hostnames, image and app names, and error message text are never included in telemetry payloads. Only the fields in this table are sent, and `properties` holds only the bounded values documented below.

## Activation events

The installer emits `install_started`, `install_progress`, and `install_finished`. The outcome is `completed`, `failed`, or `cancelled`; declining an erase confirmation counts as cancellation. `image_written` records the completed media write, before optional provisioning. `device_boot_verified` requires the first-boot verifier to contact the expected agent and pass its requested version, board, identity and enrollment checks. The verifier does not check application readiness.

Runs emit `run_started`, `run_progress`, and `run_finished`. `app_started` is emitted immediately when the agent acknowledges container start, before attached logs end. `app_ready` is emitted only after the attached run's configured readiness check succeeds. A failed check emits `app_readiness_failed` with a canned diagnostic even when the attached command continues streaming logs. Detached runs do not claim readiness. Multi-service, Compose, fleet and provider paths do not yet all report these application milestones; command success must not substitute for a missing acknowledgement.

Lifecycle properties include the current `stage` and `step`, `outcome`, `entrypoint` (`cli`, `mcp`, or `tour`), interactive-terminal status, duration and a subprocess exit code when available. Run diagnostics retain `unknown` for checks not actually observed, including authentication and build prerequisites. Recognized project and selected/reachable target checks become true only after their corresponding operations succeed. Tour events may include the selected route (`physical`, `simulator`, or `existing_device`). Failures include a stable `error_code` and an `error_detail` selected from fixed remediation sentences. Unknown failures use `step_failed` and a diagnostic-report instruction; they do not assert a root cause. No arbitrary error text is sent.

MCP tools use an attempt ID per call. The run subprocess inherits that ID. Durable installation jobs preserve their attempt ID across worker processes and first-boot verification. IDs do not identify people or join browser sessions to CLI installations. Installation method is currently `unknown`; acquisition source and OS version are unavailable and are not inferred from shell environment variables.

Delivery remains best-effort, with no persistent offline queue. A start without a terminal event has an unknown outcome; it is not proof of abandonment. Analytics remain disabled by the same opt-out and CI settings for every event.

Legacy `first_run` means the first completed CLI invocation, and `install_completed` describes the Homebrew postinstall hook. These are not device setup or first deployment events. Version 2 marks delivered milestones as successful. Historical events keep their original semantics.

`first_deploy_success` records the first acknowledged application start,
rather than merely a successful CLI exit. Managed-robot
provisioning does not emit it, and a successful command exit alone is insufficient.

## Deploy events

`wendy run` and `wendy watch` send one `deploy_completed` event per deploy, in addition to the command's `command_executed` event. It is sent once a deploy path has been chosen; earlier failures, such as an invalid `wendy.json` or an unreachable device, are covered by `command_executed` alone. Managed-robot provisioning builds are never reported.

`duration_ms` and `success` describe the deploy up to the agent's confirmation that the container started, not the log session that follows. Paths without that confirmation (compose, multi-service, fleet builds, and fast-path runs that reuse a running container) cover the whole command.

Its `properties` hold only numbers and fixed values. A phase is reported only once it happened.

| Property | Description |
|----------|-------------|
| `deploy_transport` | How the image reached the device: `chunk`, `registry`, `fastpath`, `buildhost`, `multiservice`, `native`, `xcode`, `swiftpm`, `swift`, or `compose` |
| `deploy_started` | `true` when the agent confirmed the container started |
| `deploy_fallback` | Why a registry push was used: `unimplemented`, `transport`, or `other` after a failed chunk upload; `darwin_agent`, `deploy_only`, `chunking_off`, or `not_attempted` when no chunk upload was tried |
| `deploy_target_platform` | Target platform, such as `linux/arm64` or `linux/arm/v7`; unrecognized values are sent as `other` |
| `deploy_device_type` | Board type reported by the device's OS, such as `jetson-orin-nano` |
| `deploy_build_ms`, `deploy_push_ms`, `deploy_upload_ms`, `deploy_device_prepare_ms`, `deploy_start_ms` | Chunk deploys: build, whole push, upload, device-side image preparation, and container start times in milliseconds |
| `deploy_bytes_sent`, `deploy_chunks_sent`, `deploy_chunks_total`, `deploy_layers_total`, `deploy_layers_reused` | Chunk deploys: bytes and chunks uploaded, chunks and layers in the image, and layers the device already had |
| `deploy_compression` | Chunk deploys: `gzip` or `none` |
| `deploy_build_push_ms` | Registry deploys: build and push time in milliseconds |
| `deploy_chunk_attempt_ms` | Time spent on a chunk upload that fell back to a registry push |
| `deploy_image_bytes` | Image size in bytes |

## Error categories

Failed commands report a fixed `error_class`. Shared operations use the same
categories across commands, including `run`, `watch`, `build`, and device commands.

| Category | Scenario |
|----------|----------|
| `build_failed` | An image, Swift, Xcode, or provider build failed. Fused build-and-push commands also use this category when the subprocess cannot distinguish the two stages. |
| `builder_unavailable` | Docker, Apple Container, a Swift toolchain, or builder setup is unavailable or failed to start. |
| `tool_not_found` | A required executable could not be found. |
| `config_invalid` | Invalid run options, missing build files, unsupported project detection, or failed loading/validation of `wendy.json`. |
| `cli_usage` | Flag parsing rejected an unknown flag, missing value, invalid value, or invalid syntax. |
| `project_target_mismatch` | The project cannot run on the selected target, architecture, or host platform. |
| `no_device` | No target was selected or no matching/enrolled device was found. |
| `device_ambiguous` | Multiple devices match and an explicit name or ID is required. |
| `device_offline` | The selected cloud device, or all enrolled cloud devices, are reported offline. |
| `device_unreachable` | No agent is listening, no authenticated endpoint answered, or a selected device has no reachable IP agent. |
| `simulator_unavailable` | The selected simulator could not be made available. |
| `device_identity_mismatch` | The selected device's identity does not match its saved pin. |
| `device_org_mismatch` | The device belongs to an organization for which the CLI has no credentials. |
| `device_tls_rejected` | The device TLS handshake rejected the CLI's credentials. |
| `device_auth_required` | A provisioned agent requires authentication that the CLI could not satisfy. |
| `auth_required`, `auth_session_ambiguous` | Cloud login is missing, or multiple sessions require an explicit choice. |
| `auth_certificate_failed` | Cloud certificate issuance/renewal returned a structured failure. |
| `auth_login_expired` | The device refused this computer's login because it has expired, and it could not be renewed. |
| `registry_auth`, `registry_unavailable` | Required registry credentials/transport are unavailable, or the Mac agent has no reachable registry. |
| `transfer_failed` | Chunk upload, file sync, or a separate image push failed. |
| `container_start_failed` | The agent's create/start stream ended before confirming completion. |
| `readiness_timeout` | The application's TCP readiness probe did not succeed in time. |
| `user_cancelled`, `context_canceled`, `context_deadline` | A picker or operation was cancelled, or its context deadline expired. |
| `network_dns`, `network_timeout`, `network_refused`, `network_unreachable`, `connection_closed` | A typed network error identifies DNS, timeout, refused connection, unreachable network/host, or reset/broken pipe. |
| `permission_denied`, `file_not_found`, `disk_full` | An underlying OS error identifies a local resource failure. |
| `unexpected_eof`, `process_failed` | An unclassified operation ended with a truncated stream or a subprocess exit failure. |
| `other` | The error has no recognized category or typed cause. Its message is still never sent. |

gRPC errors use `grpc_unavailable`, `grpc_deadline`, `grpc_unimplemented`,
`grpc_invalid_argument`, `grpc_not_found`, `grpc_already_exists`,
`grpc_permission_denied`, `grpc_resource_exhausted`, `grpc_failed_precondition`,
`grpc_aborted`, `grpc_out_of_range`, `grpc_internal`, `grpc_data_loss`,
`grpc_unauthenticated`, or `grpc_unknown`. gRPC cancellation uses
`context_canceled`; unrecognized future status codes use `grpc_other`.

Cancellation and deadlines take precedence, followed by known command categories,
then gRPC and OS/network causes. Wrapped and joined errors retain their causes.
For a multi-service failure with several categories, the classifier reports the
first matching category in its fixed priority order, not a majority vote.

These categories apply to events sent by updated clients. Historical events that
only stored `other` or `grpc_other` cannot be reclassified without additional
failure data.

The CLI stores a private, timestamp-only `activation-state.json` in the active config directory after a successful install operation. A later `run_started` clears the waiting window. On a subsequent CLI launch, it emits `next_launch_no_run_24h` and `next_launch_no_run_7d` when each threshold has elapsed. Each threshold is acknowledged only after successful HTTP delivery, so failed sends can retry on later launches. These are observations of returning installations; they do not classify absent users as abandoned. The check follows analytics opt-out and creates no state when disabled. Delivery remains best-effort, and concurrent CLI processes can duplicate a threshold observation.

## Opting out

Analytics can be disabled in several ways:

1. Set the environment variable to override the saved preference:
   ```sh
   WENDY_ANALYTICS=false wendy <command>
   ```
2. Save the disabled preference with:
   ```sh
   wendy analytics disable
   ```
3. CI detection automatically disables analytics when a standard CI environment variable has a non-empty value. Even `CI=false` disables analytics. There is no opt-in escape hatch in CI.
