Tails the OTel logs from a wendy-agent, rendering them in the terminal. By default it shows all apps **and the agent**'s logs.

With `--app`, you can filter on a per-app basis. You can also set a minimum log level using, for example, `--level error`.

If you provide `--json`, the output will be JSONL, one line per log statement.

To inspect the device's kernel ring buffer (`dmesg`) instead of container/agent logs, use [`wendy device os-logs`](./os-logs).

## Flags

| Flag | Description |
|------|-------------|
| `--app <name>` | Only show logs from the named app. For multi-service apps this also includes each service container's logs, so crash output from a crash-looping service member is reachable with `--app <appId>`. |
| `--service <name>` | Only show logs from the named service. |
| `--level <level>` | Minimum log level: `trace`, `debug`, `info`, `warn`, `error`, or `fatal`. **Defaults to `info`** when neither `--level` nor `--min-severity` is given, so kernel `dmesg` debug/trace output stays hidden unless you ask for it. |
| `--min-severity <n>` | Minimum OTel severity number; a numeric alternative to `--level`. |
| `--tail <N>` | Request the last N stored log batches **matching the active filters** before following new output (default `0`). The window counts only batches that survive `--app`/`--service`/`--level`, so other apps logging at high volume on the same device cannot push the requested app's logs out of the requested window. |
| `--no-follow` | Print available history and exit successfully instead of waiting for live logs. With `--tail`, replay at most N matching stored batches. |
