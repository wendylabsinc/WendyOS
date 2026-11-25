#!/usr/bin/env bash
# Build a Wendy Lite native simulator, create the blink-rgb-led template and
# run it on the simulator with the Wendy CLI, then check the app's output.
#
# Needs git and eim (ESP-IDF Installation Manager) on PATH; the CLI installs
# ESP-IDF and the ESP32 Emulator itself. Runs on Linux (x86_64, ARM64) and
# Apple Silicon macOS, in CI or locally:
#
#   make -C go build-cli
#   .github/scripts/test-lite-simulator-blink.sh --output /tmp/lite-sim
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WENDY="$ROOT/go/bin/wendy"
VERSION="gab/configurator"
TEMPLATES_BRANCH="main"
VM="lite-blink-ci"
OUT=""
APP="blink-ci"
LOG_WAIT_SECONDS=90

usage() {
  cat <<EOF
Usage: $0 --output DIR [--wendy PATH] [--version REF] [--templates-branch NAME] [--name VM]

  --output DIR             Folder for logs and the generated project (required)
  --wendy PATH             Wendy CLI binary (default: go/bin/wendy)
  --version REF            Wendy Lite version, branch or commit (default: $VERSION)
  --templates-branch NAME  Branch of the templates repo (default: $TEMPLATES_BRANCH)
  --name VM                Simulator name (default: $VM)
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --output) OUT="$2"; shift 2 ;;
    --wendy) WENDY="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --templates-branch) TEMPLATES_BRANCH="$2"; shift 2 ;;
    --name) VM="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done
if [[ -z "$OUT" ]]; then
  usage >&2
  exit 2
fi
if [[ ! -x "$WENDY" ]]; then
  echo "Wendy CLI not found at $WENDY; build it with 'make -C go build-cli'" >&2
  exit 2
fi
WENDY="$(cd "$(dirname "$WENDY")" && pwd)/$(basename "$WENDY")"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
PROJECT="$OUT/work/$APP"

# Keep the VM store and the emulator install away from the user's ~/.wendy.
# ESP-IDF (~/.espressif) and the Wendy Lite build cache stay shared.
export WENDY_CONFIG_DIR="$OUT/config"
export WENDY_ANALYTICS=false
export WENDY_NO_UPDATE_CHECK=1
export NO_COLOR=1
unset WENDY_DEVICE

# Without a terminal the CLI turns JSON output on by itself, and in JSON mode
# 'vm start' does not install the ESP32 Emulator even with --yes.
w() { "$WENDY" --json=false "$@"; }

STAGE="start"
LOGS_PID=""

group() {
  STAGE="$1"
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
    echo "::endgroup::"
    echo "::group::$1"
  else
    echo "==> $1"
  fi
}

finish() {
  local status=$?
  set +e
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
    echo "::endgroup::"
    echo "::group::Cleanup"
  fi
  if [[ -n "$LOGS_PID" ]]; then
    kill "$LOGS_PID" 2>/dev/null
    wait "$LOGS_PID" 2>/dev/null
  fi
  if [[ -d "$PROJECT" ]]; then
    mkdir -p "$OUT/project/main"
    cp "$PROJECT/CMakeLists.txt" "$PROJECT/sdkconfig.defaults.wendy" "$OUT/project/" 2>/dev/null
    cp "$PROJECT/main/idf_component.yml" "$OUT/project/main/" 2>/dev/null
  fi
  w vm logs "$VM" > "$OUT/vm-console.log" 2>&1
  w vm stop "$VM" --force
  w vm rm "$VM" --force
  [[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::endgroup::"

  local result="passed"
  if [[ $status -ne 0 ]]; then
    result="failed at: $STAGE"
    echo "Test failed at: $STAGE (exit $status)" >&2
  fi
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    {
      echo "### Wendy Lite simulator: blink-rgb-led"
      echo
      echo "- Wendy Lite version: \`$VERSION\`"
      echo "- Templates branch: \`$TEMPLATES_BRANCH\`"
      echo "- Result: $result"
      if [[ -s "$OUT/device-logs.txt" ]]; then
        echo
        echo '```'
        grep -E 'Cycle [0-9]+' "$OUT/device-logs.txt" | head -5
        echo '```'
      fi
    } >> "$GITHUB_STEP_SUMMARY"
  fi
  exit $status
}
trap finish EXIT

[[ -n "${GITHUB_ACTIONS:-}" ]] && echo "::group::Prepare"
w vm rm "$VM" --force >/dev/null 2>&1 || true
rm -rf "$OUT/work" "$OUT/project"
mkdir -p "$OUT/work"

group "Create the simulator (wendy vm create)"
w vm create "$VM" --profile wendy-lite-native --version "$VERSION"

group "Start the simulator (wendy vm start)"
w vm start "$VM" -d --yes

group "Wait for the simulator (wendy device info)"
"$WENDY" --json --device "vm:$VM" device info > "$OUT/device-info.json"
cat "$OUT/device-info.json"
if ! grep -Eq '"nativeAppSupport"[[:space:]]*:[[:space:]]*true' "$OUT/device-info.json"; then
  echo "vm:$VM does not report native app support" >&2
  exit 1
fi

group "Create the blink template (wendy init)"
(
  cd "$OUT/work"
  w init "$APP" --template blink-rgb-led --target wendy-lite --language c \
    --assistant skip --git-init no --branch "$TEMPLATES_BRANCH"
)

group "Build and run the template (wendy run)"
(
  cd "$PROJECT"
  w run --device "vm:$VM" --yes --detach 2>&1 | tee "$OUT/run.log"
)

group "Check the app output (wendy device logs)"
w --device "vm:$VM" device logs > "$OUT/device-logs.txt" 2>&1 &
LOGS_PID=$!
cycles=0
for _ in $(seq "$LOG_WAIT_SECONDS"); do
  cycles=$(grep -cE 'Cycle [0-9]+' "$OUT/device-logs.txt" || true)
  if [[ $cycles -ge 2 ]]; then
    break
  fi
  if ! kill -0 "$LOGS_PID" 2>/dev/null; then
    break
  fi
  sleep 1
done
kill "$LOGS_PID" 2>/dev/null || true
wait "$LOGS_PID" 2>/dev/null || true
LOGS_PID=""
if [[ $cycles -lt 2 ]]; then
  echo "The app printed $cycles 'Cycle N' line(s) in ${LOG_WAIT_SECONDS}s; expected at least 2." >&2
  echo "--- device logs (tail) ---" >&2
  tail -n 40 "$OUT/device-logs.txt" >&2 || true
  echo "--- simulator console (tail) ---" >&2
  w vm logs "$VM" 2>&1 | tail -n 40 >&2 || true
  exit 1
fi
grep -E 'Cycle [0-9]+' "$OUT/device-logs.txt" | head -5
echo "blink-rgb-led runs on vm:$VM"
