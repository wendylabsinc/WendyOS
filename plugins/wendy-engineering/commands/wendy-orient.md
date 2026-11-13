---
description: Map the current Wendy repo and identify the relevant CLI, agent, device, or docs code paths.
argument-hint: [task, error, command, or repo path]
---

Use the `wendy-codebase` skill. Start from this task surface:

`$ARGUMENTS`

Return a concise orientation with:

1. Current repo, branch, and dirty-state summary.
2. The likely code path from user-facing command or symptom to implementation files.
3. The first commands or tests you would run next.
4. Any risks from live device state, generated code, or unrelated worktree changes.
