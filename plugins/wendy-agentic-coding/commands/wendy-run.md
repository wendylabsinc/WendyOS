---
description: Build, deploy, run, or detach a Wendy app.
argument-hint: [app path, hostname, detach/deploy mode, or build type]
---

Use the `wendy-app-lifecycle` skill. Run or deploy the app for:

`$ARGUMENTS`

Prefer `wendy run --yes --detach --device <hostname>` for long-running apps that the coding agent should start and then inspect with logs. Use attached `wendy run --yes --device <hostname>` only when live stdout/stderr is the desired foreground interaction. Use `--build-type docker|compose|swift|python` to avoid build-type ambiguity.

For container builds on WendyOS devices, pass Dockerfile arguments with `--build-arg WENDY_VERSION=2026.11.03-074612`. Repeat `--build-arg KEY=VALUE` for additional arguments. Explicit values override device defaults and Compose build args for each built service; the last occurrence wins. Empty values are allowed, and commas stay in the value. This flag requires the Dockerfile Stagefile backend and is not supported for local provider targets or native builds.
