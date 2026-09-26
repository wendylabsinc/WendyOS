---
description: Create, validate, or repair a Wendy project scaffold and wendy.json.
argument-hint: [app id, language, template, entitlements, schema, or validation target]
---

Use the `wendy-project-setup` skill. For new app creation, prefer the `wendy-template-app` skill first and search local templates or `wendylabsinc/templates` before writing a scaffold from scratch. Set up or validate the Wendy project for:

`$ARGUMENTS`

Prefer explicit `wendy init` flags with `--assistant skip`, validate with `wendy project validate`, and use `wendy project add` / `edit` with explicit field flags for manifest changes. Use `--dry-run --json` to inspect changes without writing.
