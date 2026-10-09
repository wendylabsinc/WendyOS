# Project manifest editor

Run `wendy project` inside an app directory to inspect and edit `wendy.json`.
The terminal menu lets you add capabilities and integrations, edit existing
settings, switch between app and service scope, and review changes before saving.
You can make several changes before choosing **Review and save**. Exiting or
cancelling the menu discards unsaved changes.

If the directory has no manifest, the menu offers to create one for the existing
project. It does not scaffold source files. Compose projects can run without a
manifest; creating one adds optional Wendy-specific settings.

## Direct commands

```sh
# Inspect the manifest and its services.
wendy project show
wendy project show --json

# Add capabilities or integrations without choosing a manifest section.
wendy project add camera
wendy project add http --port 8080
wendy project add persist --name recordings --path /data
wendy project add i2c --bus i2c-1
wendy project add serial --serial-device ttyACM0
wendy project add ros2 --domain-id 42

# Change settings without removing and re-adding an entry.
wendy project edit ros2 --domain-id 43
wendy project edit persist --path /recordings
wendy project edit app --version 0.2.0
wendy project remove camera

# Validate without building or connecting to a device.
wendy project validate
```

`add` and `edit` show forms when run in a terminal without field flags. Forms
prefill existing values. When flags are provided, only missing required values
prompt. Commands that collect interactive input show a preview and ask before
saving. Fully specified commands apply immediately.

Omit the name from `add`, `edit`, or `remove` to use a searchable picker. Names
include `camera`, `audio`, `gpu`, `npu`, `network`, `persist`, `i2c`, `gpio`, `spi`,
`serial`, `usb`, `input`, `display`, `http`, `mcp`, and `ros2`. `storage` is an alias
for `persist`. The picker explains the remaining capabilities. Deprecated
`video` entries remain editable, but new projects should use `camera`.

New network entries default to explicit `bridge` mode, which supplies outbound
internet access. Use `--mode host` when the app needs the device's host network.
Mesh networking also requires `--service-cidr`. Changing away from mesh removes
the mesh-only service CIDR.

## Service scope

```sh
wendy project add camera --service vision
wendy project edit ros2 --service navigation --domain-id 42
```

These commands edit only the named service. The overview separates app defaults
from local service entries. To change or remove an inherited app-level entry,
edit the app scope without `--service`. For a Compose service, Wendy creates its
companion manifest section when needed. The Compose file itself is unchanged.

Multiple storage, I2C, and serial entries are supported. If a type appears more
than once, the menu lets you choose an entry. In scripts, use the zero-based
index printed by `show`, for example `wendy project edit persist --entry 1 --path
/cache`. Indices refer to the entitlement array within the selected scope and
can change after removing an entry.

## Preview, scripting, and other files

```sh
wendy project add http --port 8080 --dry-run
wendy project edit ros2 --domain-id 42 --dry-run --json
wendy project --file ./apps/robot/wendy.json
wendy project validate ./apps/robot
```

`--file` accepts a manifest file or project directory. `--dry-run` validates and
prints changes without writing. Without a terminal, or with `--json`, commands
never prompt. Missing names and required fields produce errors with the relevant
flags. Plain `wendy project` prints the overview in a noninteractive session.

`show --json` returns `path`, `exists`, `compose`, `manifest`, and `services`.
Mutation output includes `saved`, `dryRun`, `changes`, and `warnings`.
`validate --json` reports `valid`, warnings, and an error on failure, with a
nonzero exit status for invalid manifests. Preview paths use JSON Pointer syntax.
Previews redact password, token, secret, and environment values; `show --json`
returns the complete manifest for tooling.

## Edit the full manifest

```sh
EDITOR='code --wait' wendy project edit --raw
```

This opens a temporary copy in `$VISUAL` or `$EDITOR`. Use an editor command that
waits until editing is complete. After the editor exits, Wendy validates the
result and previews changes before replacing the manifest. This also works for
repairing malformed JSON. Use it for environment variables, readiness checks,
hooks, resource limits, and other fields without dedicated forms.

All saves validate the manifest, preserve unrelated and unknown JSON fields,
retain file permissions, and replace the file atomically. Saves stop if the
manifest changed after editing began. Saving formats JSON with two-space
indentation and sorted object keys; it does not preserve the original layout.

The older `wendy project entitlements` and `wendy project frameworks` commands
remain available and use the same editor and save behavior. `wendy json validate`
and `wendy json schema` also remain available.
