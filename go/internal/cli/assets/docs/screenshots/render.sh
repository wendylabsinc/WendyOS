#!/usr/bin/env bash
#
# render.sh — regenerate all Wendy CLI screenshots + animated clips, in both
# the light and dark docs themes, with one command.
#
# It drives the shipping `wendy` binary inside VHS's headless terminal. Because
# the captures come from a scripted REAL session, a WendyOS device must be
# ATTACHED (USB-C, or reachable on the same network) when you run this. This is
# a developer-machine step, NOT CI (see the design's Scope & non-goals).
#
# Usage:
#   ./render.sh                    # render every flow in tapes/, both themes
#   ./render.sh wifi-connect       # render only the named flow(s)
#   ./render.sh discover wifi-connect
#
# Output, for a flow <f> (steps <s>, themes <t> in {light,dark}):
#   ../images/docs/cli/<f>/<s>-<t>.png     one still per documented step
#   ../images/docs/cli/<f>/<f>-<t>.webp    animated clip of the whole flow
#
# That naming contract is the stable interface the <CliShot>/<CliClip> docs
# components depend on — keep it in sync with components/docs/cli-shot.tsx.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tapes_dir="$here/tapes"
themes_dir="$here/themes"
common_tape="$here/lib/common.tape"
dest_root="$(cd "$here/.." && pwd)/images/docs/cli"

themes=(light dark)

# Project directory the `run` flow builds/deploys from. Defaults to the repo's
# Examples/HelloPython (fast to build); override by exporting your own. Exported
# so it reaches the tape's shell inside VHS (which inherits the environment).
: "${WENDY_SHOTS_RUN_DIR:=$(cd "$here/../../../../../.." 2>/dev/null && pwd)/Examples/HelloPython}"
export WENDY_SHOTS_RUN_DIR

# MarginFill per theme == the docs page background, so the square-cornered
# window blends into the page. Keep these in sync with the "background" field
# of themes/wendy-<theme>.json.
declare -A margin_fill=(
  [light]="#f5f5f5"
  [dark]="#121212"
)

# --- preflight -------------------------------------------------------------

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: required tool '$1' not found — $2" >&2
    exit 1
  }
}

need vhs      "install with: brew install vhs"
need gif2webp "install with: brew install webp"
need wendy    "build/install the Wendy CLI and put it on your PATH"

echo "» Reminder: a WendyOS device must be attached — these are scripted real sessions." >&2

# Bring the target device's agent up to date BEFORE rendering. Otherwise, when
# the CLI is newer than the agent, plain commands (apps list, logs, run) block
# on an interactive "agent is behind the CLI — update now?" prompt that derails
# the scripted capture (the interactive TUIs dodge it, but the others don't).
#
# Run non-interactively (stdin from /dev/null) so it updates the agent and only
# *reports* any OS update rather than applying it — no surprise reflash/reboot
# mid-pipeline. Targets $WENDY_DEVICE, same as the tapes. Skip with
# WENDY_SHOTS_SKIP_UPDATE=1 when you already know the agent is current.
if [ "${WENDY_SHOTS_SKIP_UPDATE:-0}" != "1" ]; then
  echo "» Ensuring the target device's agent is up to date..." >&2
  if ! wendy device update </dev/null; then
    echo "warning: 'wendy device update' failed; captures may hit the update prompt" >&2
  fi
fi

# Flows to render: explicit args, otherwise every tape in tapes/.
flows=()
if [ "$#" -gt 0 ]; then
  flows=("$@")
else
  for tape in "$tapes_dir"/*.tape; do
    flows+=("$(basename "$tape" .tape)")
  done
fi

render_one() {
  local flow="$1" theme="$2"
  local flow_tape="$tapes_dir/$flow.tape"
  local theme_file="$themes_dir/wendy-$theme.json"

  [ -f "$flow_tape" ]  || { echo "error: no such tape: $flow_tape" >&2; return 1; }
  [ -f "$theme_file" ] || { echo "error: no such theme: $theme_file" >&2; return 1; }

  local dest="$dest_root/$flow"
  mkdir -p "$dest"

  # VHS `Set Theme` takes a theme NAME or inline JSON — not a file path — so we
  # flatten the theme file onto a single line and inline it into a driver tape.
  local theme_json
  theme_json="$(tr '\n' ' ' < "$theme_file" | tr -s ' ')"

  # Render in a temp dir: the tape's Screenshot/Output paths are literal, so we
  # let them land here, then relocate to canonical names with the theme suffix.
  local work
  work="$(mktemp -d)"

  local driver="$work/driver.tape"
  {
    echo "Output \"clip.gif\""
    echo "Require wendy"
    echo "Set Theme $theme_json"
    echo "Set MarginFill \"${margin_fill[$theme]}\""
    echo "Source \"$common_tape\""
    echo "Source \"$flow_tape\""
  } >"$driver"

  echo "→ rendering $flow ($theme)"
  ( cd "$work" && vhs "$driver" )

  # Stills: <step>.png → <dest>/<step>-<theme>.png
  shopt -s nullglob
  local png step
  for png in "$work"/*.png; do
    step="$(basename "$png" .png)"
    cp "$png" "$dest/$step-$theme.png"
  done
  shopt -u nullglob

  # Animated clip: VHS GIF → animated WebP (smaller, loops forever).
  if [ -f "$work/clip.gif" ]; then
    gif2webp -q 80 -mixed "$work/clip.gif" -o "$dest/$flow-$theme.webp" >/dev/null
  fi

  rm -rf "$work"
}

for flow in "${flows[@]}"; do
  for theme in "${themes[@]}"; do
    render_one "$flow" "$theme"
  done
done

echo "✓ Done — captures written under $dest_root/<flow>/"
