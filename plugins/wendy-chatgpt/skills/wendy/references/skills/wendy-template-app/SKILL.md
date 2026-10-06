---
name: wendy-template-app
description: Use when creating a Wendy app from an existing Wendy template or sample, especially when the user names an app archetype such as realsense-camera, camera-feed, camera-feed-yolo, voice-assistant, audio, fullstack, simple-api, or asks to create a Wendy app in a new directory.
---

# Wendy Template-First App Workflow

Prefer adapting known Wendy templates before hand-writing a new scaffold. If the requested app sounds like an existing Wendy archetype, inspect templates first, then build from the closest match.

## Find templates

Search in this order:

1. Local sibling repo: `../templates` from a Wendy workspace.
2. A `templates/` directory in the current workspace.
3. GitHub repo `wendylabsinc/templates` if local source is missing or stale.

Useful searches:

```bash
rg -n "realsense|camera|yolo|voice|audio|fullstack|simple-api" ../templates
rg --files ../templates -g template.json -g wendy.json -g Dockerfile -g Containerfile
```

Read the candidate template's `template.json`, `wendy.json`, `Dockerfile` or `Containerfile`, and main source files before copying or rewriting code. Use `meta.json` to discover template names and descriptions when present.

If no local templates repo is available, clone or inspect the GitHub source in a temporary location before building:

```bash
git clone --depth 1 https://github.com/wendylabsinc/templates /tmp/wendy-templates
```

## Create from a template

When the template is available through `wendy init`, prefer explicit non-interactive flags:

```bash
mkdir -p <destination>
cd <destination>
wendy init \
  --here \
  --app-id <app-id> \
  --target wendyos \
  --language <language> \
  --template <template-name> \
  --var APP_ID=<app-id> \
  --var PORT=<port> \
  --assistant skip \
  --git-init no
```

`--here` keeps the scaffold in the directory just created; without it, `--app-id` creates another subdirectory. Pass every required `template.json` variable with `--var`; otherwise the command may prompt. If `wendy init` cannot express the needed customization, copy the closest template into the requested destination and adapt it directly. Verify that `<destination>/wendy.json` exists before building.

## Adapt safely

- Preserve the template's proven Dockerfile/Containerfile, entrypoint, readiness probe, hooks, and entitlements unless the requested app needs a real change.
- Validate `wendy.json` after edits.
- If hardware is involved, use `wendy-device-ops` to inspect device capabilities before changing entitlements.
- If source behavior is unclear, inspect `wendylabsinc/templates` before inventing a replacement.
- Keep template-derived app IDs, ports, README text, and paths consistent with the user's requested directory.

## RealSense note

For a prompt like "make me a Wendy app with the realsense-camera app in ./foo-bar", first look for `python/realsense-camera` in `wendylabsinc/templates`. Its template declares `APP_ID` and `PORT` variables and uses USB plus host networking for the RealSense multi-stream viewer.
