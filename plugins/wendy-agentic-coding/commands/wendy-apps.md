---
description: List, start, stop, remove, or clean up Wendy apps and volumes.
argument-hint: [app id, hostname, action, cleanup mode]
---

Use the `wendy-app-lifecycle` skill. Manage app lifecycle for:

`$ARGUMENTS`

Use `wendy --json device apps list` for non-interactive listing, pass app names explicitly to `start`, `stop`, and `remove`, and use `--force` plus explicit cleanup flags for destructive operations only when intended. To start an existing app in the background, use `wendy device apps start <app-id> --detach`: it returns once the agent confirms the start and sets the `unless-stopped` restart policy, so the agent restarts the app whenever it exits until `wendy device apps stop`. Without `--detach`, `device apps start` attaches to the app's output.
