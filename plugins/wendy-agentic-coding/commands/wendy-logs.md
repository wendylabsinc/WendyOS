---
description: Stream Wendy app logs or telemetry in a bounded, agent-friendly way.
argument-hint: [app id, hostname, level, or telemetry stream]
---

Use the `wendy-app-lifecycle` skill. Stream logs for:

`$ARGUMENTS`

For a bounded machine-readable sample, prefer `wendy --json device logs --app <app-id> --tail 50 --no-follow --device <hostname>`. Use `--service`, `--level`, or `--min-severity` to narrow the sample. Omit `--no-follow` only when continuous live output is requested, and bound that stream with the surrounding tool's timeout or a background process. Use `wendy device telemetry-stream --logs --app <app-id>` when JSONL telemetry is needed.
