# Global Flags

These flags are available on every `wendy` command.

## `--json`

Outputs command results as JSON instead of the default interactive TUI or table format.

```sh
wendy device list --json
```

When stdout is not a TTY (for example, when piping output, running in CI, or executing from a script), `--json` is automatically enabled. An explicit `--json` or `--json=false` always takes precedence over the automatic detection.

```sh
# JSON output without passing --json explicitly
wendy device list | cat

# Suppress JSON even in a non-TTY context
wendy device list --json=false | cat
```

In JSON mode stdout carries only JSON. Notices that change what a result means are written to stderr as single plain lines instead of being dropped: the device a command picked for you (`Using default device wendyos-abc.local.`) and certificate warnings. Onboarding hints such as "Next: run `wendy run` …" are not shown in JSON mode.

> **Note:** For live, full-screen TUI commands such as [`wendy device top`](./commands/device/top.md), `--json` does not stream the interface — it switches the command to a one-shot **snapshot** mode that prints a single JSON object and exits, instead of rendering the interactive dashboard.

### Errors in JSON mode

When a command fails in JSON mode, it writes nothing more to stdout, and the last line on stderr is one JSON object:

```json
{"error":{"code":"device_unreachable","exit":5,"message":"Could not connect to device at 192.168.1.42:50051. Is it powered on and connected to the network?","retryable":true,"next_steps":[]}}
```

| Field | Meaning |
|---|---|
| `code` | The failure category, for example `cli_usage`, `auth_required` or `device_unreachable`; `error` when the failure has no category. |
| `exit` | The process exit status (see [Exit status](#exit-status)). |
| `message` | The error as plain text, without colour or the `✗` marker. It can span several lines. |
| `retryable` | `true` when running the same command again, unchanged, can succeed, for example once the device is back online. |
| `next_steps` | Recovery steps, in order. Always an array, often empty. |

Earlier stderr lines can hold progress output and notices, so read the last line. Outside JSON mode the same failure prints as styled text. So does a failure in automatic JSON mode when stderr is a terminal, as in `wendy device apps list | grep my-app`: stdout is piped, but the error is read by a person. Pass `--json` explicitly to get the JSON error there too.

## Exit status

`wendy` exits with the same status whether or not JSON mode is on.

| Status | Meaning | `code` values |
|---|---|---|
| 0 | Success | |
| 1 | Any failure not listed below | every other code, including `error` |
| 2 | The command line is wrong: unknown command or flag, wrong number of arguments, a missing required flag, an invalid or conflicting flag value, or a confirmation that needs `--force` because there is no terminal | `cli_usage` |
| 3 | Credentials are missing, expired or ambiguous | `auth_required`, `auth_session_ambiguous`, `auth_certificate_failed`, `device_auth_required`, `registry_auth`, `grpc_unauthenticated` |
| 4 | No usable target device: none given, several match, or the device cannot run this project | `no_device`, `device_ambiguous`, `project_target_mismatch` |
| 5 | The device could not be reached: it refused or did not answer the connection, its host name did not resolve, or it rejected the TLS handshake | `device_unreachable`, `device_offline`, `device_not_resolved`, `device_tls_rejected`, `simulator_unavailable` |
| 6 | The build failed, or a build tool is missing | `build_failed`, `builder_unavailable`, `tool_not_found` |
| 7 | The app was deployed but did not start or stay up | `container_start_failed` |
| 8 | The app started but did not become ready in time | `readiness_timeout` |
| 10 | A trust decision only a person can make: the device's identity or organization changed | `device_identity_mismatch`, `device_org_mismatch` |
| 70 | An internal error: a bug in `wendy` itself. The stack trace is printed on stderr (in JSON mode before the envelope line, otherwise after the message); include it when you report the bug. Only a panic in the command's own goroutine is caught: one in a background goroutine still crashes with Go's own output and exit status | `internal_error` |

`device_not_resolved` means DNS reported that the host name does not exist. That can also happen while this machine is offline, so check the network before the name. A device name resolved over mDNS (a `.local` name, or a bare name such as `wendyos-abc`) that does not resolve is `device_unreachable` instead, since it resolves only while the device is on the network.

`retryable` is `true` for `device_unreachable`, `device_offline`, `transfer_failed`, `registry_unavailable`, `readiness_timeout`, and the timeouts `grpc_deadline` and `network_timeout`.

## `--device`

Specifies a target device by IP address, hostname, provider key, or explicit `host:port`, bypassing [device selection](./device-selection.md).

```sh
wendy --device 192.168.1.42 device apps list
wendy --device my-mac.local:50051 device info --json
```

## Automatic update notifications

The Wendy CLI checks GitHub for a newer release in the background once every 24 hours. Because the HTTP call can take several seconds, the result is **persisted** to `~/.wendy/config.json` (field `availableCLIUpdate`) and displayed at the end of the **next** CLI command you run after the check completes.

- **Interactive terminal:** The CLI prompts `Update now?` (default yes). Answering yes runs the upgrade automatically. Either way, the stored tag is cleared so the prompt does not repeat until the next check finds another update.
- **Non-interactive / `--json` mode:** The notice is printed to stderr. No prompt is shown.
- **macOS:** The upgrade command is `brew update && brew install wendy`. If the tap is untrusted, the CLI also suggests `brew trust wendylabsinc/tap`.
- **Windows:** `winget upgrade WendyLabs.Wendy`.
- **Linux:** `curl -fsSL https://install.wendy.dev/cli.sh | bash`.

> **Note:** The 24-hour cooldown between update checks depends on `~/.wendy/config.json` being writable. If the file cannot be saved, the background check runs on every CLI invocation.

## Automatic shell-completion prompt

When shell completions aren't installed, the CLI offers — at most once per 24-hour window — to install them with an ambient `Install them now? [y/n]` prompt after a command finishes. It is never shown in non-interactive or `--json` contexts, on commands that handle completions themselves (`wendy completion …`, [`wendy tour`](./commands/tour.md)), or once completions are installed or the prompt is dismissed. See [`wendy completion`](./commands/completion.md#automatic-prompt-to-install-completions) for the full behavior.

Its state is persisted in `~/.wendy/config.json`:

| Field | Type | Meaning |
|---|---|---|
| `completionInstalled` | bool | Completions were installed through the CLI; permanently suppresses the prompt. |
| `completionPromptDismissed` | bool | The user answered `n` to the prompt; permanently suppresses it. |
| `lastCompletionPromptCheck` | RFC3339 timestamp | When the prompt was last shown; throttles it to once per 24-hour window. |

## Environment variables

| Variable | Description |
|----------|-------------|
| `GITHUB_TOKEN` | When set, the CLI uses it as a bearer token for GitHub API release checks and agent update lookups. When absent, those requests are made unauthenticated. |
| `WENDY_ANALYTICS` | Set to `false` to disable analytics. |
| `WENDY_APPSTORE_API` | Override the Wendy AppStore resolution API base URL used by `wendy app install` / `wendy device apps install`. Takes precedence over the built-in default; the `--api` flag takes precedence over this variable. |
| `WENDY_BUILD_PROGRESS` | Override the progress format requested from `docker buildx` during image builds. Accepts `plain` or `rawjson` (case-insensitive); any other value is ignored. When unset, the CLI probes the installed buildx version once per run and picks `rawjson` for buildx 0.13 or newer, `plain` otherwise. `rawjson` is what supplies per-step byte counters and download rates, so forcing `plain` reduces build output to step names only. |
| `WENDY_BUILDKIT_HOST` | Override the BuildKit endpoint used by `--builder buildkit`, for example `unix:///path/to/buildkitd.sock`. It takes precedence over `BUILDKIT_HOST`, Wendy's discovered `<Wendy cache>/runtime/buildkitd.sock`, and buildctl's normal local default. The daemon must use a containerd worker for `wendy build` image-store output. |
| `WENDY_STAGEFILE_BACKEND` | Select the Stagefile compiler backend when `--stagefile-backend` is not passed. Accepts `dockerfile` (default) or experimental direct `llb`; the command-line flag takes precedence. |
| `WENDY_AGENT_SIGNATURE_PATH` | Path to a detached ML-DSA65 signature file for the agent update binary. When set, `wendy device update` includes the signature in the `UpdateAgent` RPC. Has no effect until a verification key is embedded in the agent. |
| `WENDY_IMAGE_SIGNATURE_PATH` | Path to a detached ML-DSA65 signature file for the OCI image config. When set, `wendy run` includes the signature in `RunContainer` calls. Has no effect until a per-org publisher key is provisioned on the agent. |
