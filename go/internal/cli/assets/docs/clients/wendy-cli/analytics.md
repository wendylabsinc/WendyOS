We track analytics for our CLI's usage through a dedicated self-hosted telemetry service. This is [opt-out](./commands/analytics/disable.md), or through `WENDY_ANALYTICS=false` in your environment.

## How it works

When analytics are enabled, each tracked event is serialised to JSON and sent via an HTTP POST request. Delivery is best-effort: a network failure, timeout, or non-2xx response never changes the command's result.

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
| `event` | string | Event name, e.g. `"command_run"` |
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
