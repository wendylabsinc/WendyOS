# `wendy init`

Creates a new Wendy project: a [`wendy.json`](../../../apps/wendy.json.md), scaffolded source files, and optionally an AI assistant session.

Run it with no arguments for an interactive wizard, or pass flags to script it end to end. Every prompt has a matching flag, so any wizard answer can be supplied up front.

## Usage

```sh
# Interactive wizard
wendy init

# Create ./my-app and scaffold into it
wendy init my-app

# Already inside an empty my-app/ — scaffold here instead of nesting ./my-app/my-app
wendy init --here my-app

# Fully non-interactive
wendy init \
  --app-id my-app \
  --target wendyos \
  --language swift \
  --no-extra-entitlements \
  --assistant skip
```

An `[app-id]` argument (or `--app-id`) always creates a new subdirectory of that name. With neither, the wizard offers to scaffold into the current directory instead. Pass `--here` to scaffold into the current directory directly instead of nesting a subdirectory — with an app ID it still writes that as `wendy.json`'s app ID; without one, it infers the app ID from the current directory's name.

## What it produces

1. Picks the **target**, so template and language choices can be filtered to what that target supports. A concrete `--template <name>` resolves first instead: when the template supports exactly one target, the question is skipped and the target is inferred with a notice; when it supports several, the picker is narrowed to those.
2. Picks a **template** (`--template`), or continues with the plain wizard.
3. Picks the **language**. Skipped with a notice when the chosen template offers exactly one language.
4. Collects **entitlements** — the capabilities the app needs. See [Entitlements](../../../apps/wendy.json.md#entitlements-1).
5. Collects **framework** configuration — currently ROS 2. See [Frameworks](#frameworks).
6. Writes `wendy.json` and scaffolds the project files.
7. Optionally launches an AI assistant (`--assistant`).

## Targets

| `--target` | `platform` written to `wendy.json` | Languages |
|---|---|---|
| `wendyos` | `linux` | `swift`, `python` |
| `darwin` (aliases: `mac`, `macos`) | `darwin` | `swift` (plain wizard), `mojo` (via templates) |
| `wendy-lite` | `wendy-lite` | `swift` only (optional WASM scaffold) |

Templates may offer additional languages (for example `rust`, `node`, or `cpp`) on a `wendyos` target; the plain wizard writes `swift` or `python`. On a `darwin` target the plain wizard writes `swift`; `mojo` is accepted when a catalogued Mojo/MAX template such as `mac-llm` provides it.

**Language resolution.** `wendy-lite` always uses Swift and rejects any other language. `darwin` accepts `swift` or `mojo`; with a template, the language must be one the template offers, and an unsupported choice is rejected with the languages it does offer.

For ESP32, regular native ESP-IDF projects are recommended. Create them with the standard ESP-IDF tooling and add a `wendy.json` whose platform is `wendy-lite`; you do not need `wendy init`. The `wendy-lite` wizard target remains available for scaffolding an optional Swift/WASM guest.

## Flags

### Project flags

| Flag | Description |
|---|---|
| `--app-id <id>` | Application ID written to `wendy.json`. Creates a subdirectory of that name. |
| `--here` | Scaffold into the current directory instead of creating a subdirectory. With no `[app-id]`/`--app-id`, infers the app ID from the current directory's name. |
| `--target <name>` | Target platform: `wendyos`, `darwin`, or `wendy-lite`. Required when running non-interactively. |
| `--language <name>` | Project language. `swift` or `python` for the plain wizard (`swift` on `darwin`); templates may offer more, including `mojo` on `darwin`. |
| `--git-init <yes\|no>` | Initialize a git repo in the project directory. |

### Template flags

| Flag | Description |
|---|---|
| `--template [<name>]` | Scaffold from a template. Passing it **without** a value opens a picker filtered by target, auto-selecting when only one template exists for the target. With a value, `--target` becomes optional: it is inferred when the template's metadata maps to exactly one target. |
| `--branch <name>` | Branch of the templates repo to read templates from. |
| `--var KEY=VALUE` | Override a template variable. Repeatable. |

### Entitlement flags

| Flag | Description |
|---|---|
| `--entitlement <name>` | Entitlement to enable. Repeatable or comma-separated. |
| `--all-entitlements` | Enable every entitlement. Requires the field flags below for `gpio`, `i2c`, and `persist`. |
| `--no-extra-entitlements` | Skip the entitlement prompts and write only the default `network` entitlement. |
| `--gpio-pins 17,27,22` | Pins for the `gpio` entitlement. |
| `--i2c-device /dev/i2c-1` | Device path for the `i2c` entitlement. |
| `--persist-name <id>` | Storage namespace for the `persist` entitlement. |
| `--persist-path /data` | Mount path for the `persist` entitlement. |

Passing any entitlement flag suppresses the interactive entitlement prompts entirely — the flags become the complete answer.

### Framework flags

| Flag | Description |
|---|---|
| `--framework <name>` | Framework to enable under `wendy.json`'s top-level `frameworks` key. Repeatable or comma-separated. Currently accepts `ros2`. |
| `--ros2-domain-id <n>` | ROS 2 domain ID, `0`–`232`. Default: derived as a stable hash of the app ID. |
| `--ros2-rmw <impl>` | ROS 2 middleware: `cyclonedds` (default), `fastrtps`, `connextdds`, or `gurumdds`. |
| `--ros2-distro <name>` | ROS 2 distribution, e.g. `humble` (default) or `jazzy`. Lowercase letters and digits, starting with a letter. |
| `--ros2-discovery-scope <scope>` | `app` (default, isolated to this app's containers) or `host` (discoverable across the device's host network). |

See [Frameworks](#frameworks) below.

### AI assistant flags

| Flag | Description |
|---|---|
| `--assistant <claude\|codex\|skip>` | Launch an assistant in the new project after scaffolding. `claude` and `codex` must be on `PATH`. |
| `--install-claude-skills` | Install the Wendy Claude skills before launching Claude. Requires `--assistant=claude`. |

## Frameworks

`wendy.json` has a top-level [`frameworks`](../../../apps/wendy.json.md#frameworks) key, separate from `entitlements`, for framework-level configuration. ROS 2 is the only framework today.

`wendy init` fills it in three ways:

**Flags.** Pass `--framework ros2`, or any `--ros2-*` flag on its own (which implies `--framework ros2`):

```sh
wendy init \
  --app-id go2-network-bridge \
  --target wendyos \
  --language swift \
  --framework ros2 \
  --ros2-rmw cyclonedds \
  --ros2-discovery-scope host \
  --assistant skip
```

This writes:

```json
{
  "frameworks": {
    "ros2": {
      "rmw": "cyclonedds",
      "discoveryScope": "host"
    }
  }
}
```

Values are validated against the same rules `wendy.json` itself enforces, so a typo fails immediately at `init` rather than surfacing later from `wendy run` or `wendy device ros2`.

**Interactively.** On a `wendyos` target, when no entitlement flags and no framework flags were passed, the wizard asks whether the app uses ROS 2 and then walks through middleware, discovery scope, and domain ID. The distro is left at its default; edit `frameworks.ros2.distro` in `wendy.json` to change it.

**By hand.** Add the `frameworks` key to `wendy.json` yourself at any point; `wendy run` picks it up on the next deploy.

Frameworks are WendyOS-only. `--framework ros2` with `--target darwin` or `--target wendy-lite` is rejected — neither target runs the ROS 2 container image.

### With `--template`

Entitlement and framework flags are merged into the template's own `wendy.json` after it is scaffolded, and in both cases **the template's configuration wins where it is more specific**:

- `--entitlement` values already covered by the template are skipped; the rest are appended.
- `--framework`/`--ros2-*` are written only if the template does not already configure `frameworks`. A template that ships its own `frameworks` block keeps it, and the flags are dropped. A `frameworks` key that configures nothing (`null`, `{}`, or an object whose members are all `null`) does not count as configured, so the flags still apply.

`wendy init` reports what it merged in its summary (`Frameworks added from flags: ros2`).

## Non-interactive and headless use

`wendy init` detects whether a real terminal is attached. In CI, in scripts, and under AI agents there is none, so the interactive pickers are skipped rather than failing on a TTY that cannot be opened. Instead the CLI prints the choices as plain text and exits with an error naming the flag to pass:

- **No `--target`** — prints the available targets, then errors with `--target is required when running non-interactively`. With `--template <name>`, the target is inferred when the template's metadata maps to exactly one target; otherwise the error lists only that template's targets.
- **Bare `--template` with no value** — prints the templates available for the selected target, then errors with `--template requires a value when running non-interactively`. If the target has exactly one template it is auto-selected instead; if it has none, the error is `no templates available for <target>`.
- **Template with several languages and no `--language`** — prints the languages available for the template, then errors with `--language is required when running non-interactively`. A single-language template is auto-selected with a notice.
- **No app ID** — errors with `an app ID is required when running non-interactively`; pass `--app-id` or the `[app-id]` argument.

So discovering the valid values is a matter of running the command and reading the list:

```sh
$ wendy init
Available targets:
  WendyOS - Full Linux-based edge device (Jetson, Raspberry Pi, ...)
  macOS - Native macOS app deployed to Wendy Agent for Mac
  Wendy Lite - Microcontroller running WASM (ESP32)
Error: --target is required when running non-interactively (valid: wendyos, darwin, wendy-lite)

$ wendy init --app-id my-app --target wendyos --template
Available templates for wendyos:
  simple-api - Minimal HTTP API
  fullstack - Web UI plus API
Error: --template requires a value when running non-interactively; pass --template=<name> using one of the templates listed above
```

For a fully scripted run, supply every choice as a flag. `--no-extra-entitlements` and `--assistant skip` are what suppress the remaining prompts:

```sh
wendy init \
  --app-id my-app \
  --target wendyos \
  --language swift \
  --no-extra-entitlements \
  --assistant skip
```

## Examples

```sh
# Scaffold from a template, picking the language interactively
wendy init --template simple-api

# Target and language inferred from the template's metadata
# (go2-rc supports only WendyOS + Python, so neither is asked)
wendy init go2-app --template go2-rc

# Non-interactive template scaffold with a variable override
wendy init --app-id my-api --template simple-api --language rust --var PORT=8080

# A template from a branch of the templates repo
wendy init --template simple-api --branch feature/new-template

# WendyOS Python app with persistent storage
wendy init \
  --app-id demo-app \
  --target wendyos \
  --language python \
  --entitlement gpu,usb,persist \
  --persist-name demo-data \
  --persist-path /data \
  --assistant skip

# WendyOS app with GPIO and I2C
wendy init \
  --app-id edge-sensors \
  --target wendyos \
  --language swift \
  --entitlement gpio,i2c \
  --gpio-pins 17,27,22 \
  --i2c-device /dev/i2c-1 \
  --assistant skip

# Optional Wendy Lite Swift/WASM scaffold
wendy init \
  --app-id lite-app \
  --target wendy-lite \
  --no-extra-entitlements \
  --assistant skip

# Native macOS app for Wendy Agent for Mac
wendy init \
  --app-id mac-llm \
  --target darwin \
  --language swift \
  --template mac-llm \
  --assistant skip

# The same template as a native Mojo/MAX app (run.command on the Mac agent)
wendy init \
  --app-id mac-llm \
  --target darwin \
  --language mojo \
  --template mac-llm \
  --assistant skip

# ROS 2 app on a shared host DDS domain
wendy init \
  --app-id go2-network-bridge \
  --target wendyos \
  --language swift \
  --framework ros2 \
  --ros2-rmw cyclonedds \
  --ros2-discovery-scope host \
  --assistant skip

# Start Claude in the new project with the Wendy skills installed
wendy init \
  --app-id ai-app \
  --target wendyos \
  --language python \
  --entitlement gpu,audio \
  --assistant claude \
  --install-claude-skills
```

## Related

- [`wendy.json`](../../../apps/wendy.json.md) — every field `wendy init` can write.
- [`wendy project entitlements add`](project/entitlements/add.md) — add entitlements after init.
- [Wendy for ROS 2](/docs/integrations/ros2) — inspect and debug the ROS 2 app you just scaffolded.
- [`wendy run`](run.md) — build and deploy the new project to a device.
