# CLI screenshot pipeline

Repeatable, uniform captures of the Wendy CLI's interactive TUI flows for the
docs site — in both **light** and **dark** themes — from one command.

Driven by [`charmbracelet/vhs`](https://github.com/charmbracelet/vhs): each flow
is a declarative `.tape` of keystrokes with a `Screenshot` at every documented
step, rendered through shared chrome (font, padding, macOS window bar, square
corners) so every capture comes out identical.

> **Why it's not in CI:** the captures are scripted **real** sessions against an
> attached device — the most authentic source, but it needs hardware. So this is
> a developer-machine step you run when the CLI's TUI changes, not on every
> build. See the design at
> `specs/2026-06-28-cli-screenshot-pipeline-design.md`.

## Prerequisites

- `vhs` — `brew install vhs` (pulls in `ttyd` + `ffmpeg`)
- `gif2webp` — `brew install webp` (converts the GIF clip to animated WebP)
- the `wendy` CLI on your `PATH` (the pipeline drives the shipping binary as-is)
- a **WendyOS device attached** (USB-C or same-network) — the flows run for real
  against the device named by `WENDY_DEVICE`, so export it before rendering

Type is rendered with the system's native monospace font (`Set FontFamily
"monospace"` in `lib/common.tape`), so there's nothing extra to install.

## Usage

```sh
./render.sh                 # every flow in tapes/, both themes
./render.sh wifi-connect    # just one flow (both themes)
```

Outputs land under `../images/docs/cli/<flow>/`. `images/docs/` is the tracked
source tree; `scripts/prepare-content.mjs` copies it into the gitignored
`public/` at build time, so nothing else needs to change.

Before rendering, `render.sh` runs `wendy device update` once so the target's
agent matches the CLI — otherwise plain commands (`apps list`, `logs`, `run`)
block on an interactive "agent is behind the CLI — update now?" prompt that
derails the capture. Skip it with `WENDY_SHOTS_SKIP_UPDATE=1` when you know the
agent is current. The `run` flow builds/deploys from `$WENDY_SHOTS_RUN_DIR`
(defaults to the repo's `Examples/HelloPython`).

## Flows

| flow | command | step(s) | clip |
| --- | --- | --- | --- |
| `discover` | `wendy discover` | `devices` | yes |
| `wifi-connect` | `wendy device wifi` | `networks` | yes |
| `apps-list` | `wendy device apps list` | `apps` | yes |
| `logs` | `wendy device logs --app …` | `logs` | yes |
| `init` | `wendy init` | `wizard` | yes |
| `run` | `wendy run` | `deploy` | yes |
| `os-install` | `wendy os install` | `select-device-type` | yes |
| `entitlements` | `wendy project entitlements add` | `add` | yes |

**Sanitised for public docs:** `wifi-connect` filters the table to `Wendy` (so
neighbouring SSIDs never appear, in the still *or* the clip — the unfiltered
launch is wrapped in `Hide`/`Show`); `logs` is scoped to one app to avoid the
agent's transport logs (IP/serial). `os-install` captures only the device-type
picker and quits before any image download or drive write. A `cloud-discover`
flow is intentionally absent: its table is real fleet device names, which can't
be auto-sanitised — add it back deliberately if that's acceptable.

## Layout

```
screenshots/
  tapes/<flow>.tape    keystrokes + Screenshot marks for one flow (no styling)
  lib/common.tape      shared chrome: font, size, padding, square corners, shell
  themes/wendy-*.json  light/dark palettes tuned to the fumadocs neutral theme
  render.sh            driver: each tape × {light,dark} → stills + clip
```

`render.sh` composes a throwaway driver tape per `(flow, theme)` that sets the
`Output` + inline theme JSON + per-theme `MarginFill`, then `Source`s
`lib/common.tape` and the flow tape. (VHS `Set Theme` takes a name or inline
JSON, not a file path; `Output`/`Screenshot` paths are literal — hence the
generated driver.)

## Naming contract

For a flow `run` with step `deploy`, you get under `images/docs/cli/run/`:

```
deploy-light.png      deploy-dark.png      # one still per documented step
run-light.webp        run-dark.webp        # the animated clip
```

This is the stable interface the docs depend on. The `<CliShot>` / `<CliClip>`
MDX components (`components/docs/cli-shot.tsx`) build their `src` paths from it.

## Adding a flow

1. Write `tapes/<flow>.tape` — just the keystrokes, `Sleep`s, and a
   `Screenshot <step>.png` per documented step. No theme/size/output.
2. Run `./render.sh <flow>` (device attached).
3. Reference it in MDX: `<CliShot flow="<flow>" step="<step>" alt="…" />` and
   optionally `<CliClip flow="<flow>" alt="…" />`.

No pipeline changes needed.
