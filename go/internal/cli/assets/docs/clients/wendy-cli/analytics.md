We track analytics for our CLI's usage through a dedicated self-hosted telemetry service. This is [opt-out](./commands/analytics/disable.md), or through `WENDY_ANALYTICS=false` in your environment.

## How it works

When analytics are enabled, each tracked event is serialised to JSON and sent via an HTTP POST request. Delivery is best-effort: a network failure, timeout, or non-2xx response never changes the command's result.

MCP tool calls emit `command_executed` events as each call finishes, using a command name such as `wendy mcp device_info` and `command_root` of `mcp`. The duration and success flag describe the individual call, including tool results that report an error. Calls to app-provided tools use the fixed name `wendy mcp container_tool` so app and tool names stay private. Tool arguments and result content are never sent. The same analytics opt-out and CI settings apply to MCP calls.

## Endpoint

Events are posted to:

```
https://wendy-cli-telemetry-114319063177.us-central1.run.app/v1/telemetry/events
```

## Event payload

Every event is an anonymous JSON object. The fields sent are:

| Field | Type | Description |
|-------|------|-------------|
| `anonymous_id` | string | Stable random UUID stored in `~/.wendy/` — never tied to a real identity |
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

> **Privacy note:** Flag values, positional arguments, file paths, hostnames, and error message text are never included in telemetry payloads. Only the fields listed above are sent.

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

## Opting out

Analytics can be disabled in several ways:

1. **Environment variable** — takes precedence over everything else:
   ```sh
   WENDY_ANALYTICS=false wendy <command>
   ```
2. **CLI command:**
   ```sh
   wendy analytics disable
   ```
3. **CI environments** — analytics are hard-disabled automatically when any standard CI environment variable is detected (e.g. `CI=true`). There is no opt-in escape hatch in CI.
