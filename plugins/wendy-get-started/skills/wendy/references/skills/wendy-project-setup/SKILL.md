---
name: wendy-project-setup
description: Use when creating, validating, or repairing a Wendy project scaffold, adapting Wendy templates, `wendy.json`, JSON schema setup, or project entitlements with `wendy init`, `wendy json`, or `wendy project entitlements`.
---

# Wendy Project Setup Workflow

Use this before app lifecycle work when the repository does not yet have a reliable Wendy project shape.

For new app creation, prefer the `wendy-template-app` workflow first. Search the local `../templates` repo or `wendylabsinc/templates` for a close match before writing a scaffold from scratch.

## Non-interactive project creation

Prefer explicit flags so coding agents avoid template, entitlement, and assistant prompts.

Minimal WendyOS app:

```bash
wendy init --app-id <app-id> --target wendyos --language python --no-extra-entitlements --assistant skip
```

Template scaffold:

```bash
wendy init --app-id <app-id> --target wendyos --language rust --template simple-api --var PORT=8080 --assistant skip --git-init no
```

Template init can still prompt if the selected template has required variables that are not supplied with `--var`. Pass every required template variable, and set `--git-init yes` or `--git-init no`, when the goal is a fully non-interactive agent run.

If the user names an archetype such as `realsense-camera`, `camera-feed`, `camera-feed-yolo`, `voice-assistant`, `audio`, `fullstack`, or `simple-api`, inspect the matching template's `template.json`, `wendy.json`, `Dockerfile` or `Containerfile`, and source files before creating the destination app.

Project with entitlements:

```bash
wendy init \
  --app-id <app-id> \
  --target wendyos \
  --language python \
  --entitlement camera,audio,persist \
  --persist-name <app-id>-data \
  --persist-path /data \
  --assistant skip
```

Hardware app with GPIO and I2C:

```bash
wendy init \
  --app-id edge-sensors \
  --target wendyos \
  --language swift \
  --entitlement gpio,i2c \
  --gpio-pins 17,27,22 \
  --i2c-device i2c-1 \
  --assistant skip
```

Useful `wendy init` flags:

- `--app-id`: application ID written to `wendy.json`.
- `--here`: scaffold into the current directory instead of creating a subdirectory — use when you've already created/entered the project directory.
- `--target`: `wendyos` (writes `"platform": "linux"` to wendy.json), `wendy-lite`, or `darwin` (native macOS for Wendy Agent for Mac). Omit for default `wendyos` behavior.
- `--language`: `python`, `swift`, `rust`, `node`, or `cpp`.
- `--template`: template name; a bare `--template` opens a picker, so avoid bare usage in agent workflows.
- `--branch`: templates repo branch.
- `--var KEY=VALUE`: repeatable template variable override.
- `--entitlement`: repeatable or comma-separated entitlement list.
- `--all-entitlements`: enable every entitlement, with required field flags supplied.
- `--no-extra-entitlements`: skip extra entitlement prompts and keep defaults.
- `--gpio-pins`, `--i2c-device`, `--persist-name`, `--persist-path`: required field values for those entitlements. Use `i2c-1`, not `/dev/i2c-1`, for `--i2c-device`.
- `--assistant`: `claude`, `codex`, or `skip`; use `skip` unless the user explicitly wants the assistant launched.
- `--install-claude-skills`: only use with `--assistant claude` when requested.
- `--git-init`: `yes` or `no`.

## Validate `wendy.json`

Validate the current directory:

```bash
wendy json validate
```

Validate a specific file or directory:

```bash
wendy json validate ./wendy.json
wendy json validate ./path/to/app
```

Print the schema for editor setup:

```bash
wendy json schema > wendy.schema.json
```

`wendy json validate` reports deprecated or suspicious config as warnings after structural validation. Treat warnings as actionable, especially `video` entitlement warnings where new configs should use `camera`.

## Project entitlement commands

List configured entitlements:

```bash
wendy --json project entitlements list
```

List available entitlement types:

```bash
wendy --json project entitlements list --show-all
```

Add or remove a simple entitlement by name:

```bash
wendy project entitlements add camera
wendy project entitlements remove camera
```

Important: `project entitlements add` prompts for required fields for `persist`, `i2c`, and `gpio`. In an agent workflow, prefer editing `wendy.json` directly for those entitlements, then run `wendy json validate`.

For I2C, use the device name without `/dev/`, for example `i2c-1`. The runtime entitlement path prefixes `/dev/` when mounting the device.

## Setup checklist

- Prefer an existing Wendy template before creating an app from scratch.
- Use `wendy init` for a new project scaffold.
- Use the `wendy-entitlements` skill when choosing capability details.
- Keep `appId` stable once a device has deployed the app.
- Keep JSON examples valid; `wendy.json` cannot contain comments.
- Validate with `wendy json validate` before `wendy build` or `wendy run`.
