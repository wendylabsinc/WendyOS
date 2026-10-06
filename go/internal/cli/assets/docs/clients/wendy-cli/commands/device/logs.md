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
| `--no-follow` | Print the logs the device replays and exit instead of following new output. Combine with `--tail <N>` to get the last N matching batches, e.g. `wendy device logs --app <app> --tail 20 --no-follow`. The command ends when the device closes the stream after its replay (agents with finite log replay), when it switches from replayed to live output, or after a short pause once the replay stops (1.5 s, stretched on a slow link up to 10 s; the command notes on stderr when it ended this way with fewer batches than `--tail`, or than the 20 the device caches without it). Device agents released before 2026-08-19 replay history only with `--tail`, and those before 2026-05-22 not at all; when nothing is replayed, the command says so on stderr and exits. |
