#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DEFAULT_PACKAGE_DIR="$SWIFT_DIR/WendyE2ETests"

RUN_DIR=""
PACKAGE_DIR="$DEFAULT_PACKAGE_DIR"
PROVIDER="${WENDY_E2E_AI_PROVIDER:-auto}"
MODEL="${WENDY_E2E_AI_MODEL:-}"
OVERWRITE="false"
EXTRA_ARGS=()

usage() {
  cat <<EOF
Usage: $(basename "$0") --run-dir DIR [OPTIONS]

Review WendyAgent Swift E2E run artifacts with Anthropic/Claude or OpenAI.

Options:
  --run-dir DIR      Required E2E run directory produced by E2ETest.sh.
  --package-dir DIR  Swift package directory containing swift-e2e-testing;
                     defaults to $DEFAULT_PACKAGE_DIR.
  --provider NAME    AI provider: auto, anthropic, claude, openai, or none; defaults to auto.
  --model NAME       Provider model override.
  --overwrite        Overwrite existing per-test ai-review.md files.
  --help             Show this help message.

Environment:
  ANTHROPIC_API_KEY  API key used when provider is anthropic or auto.
  ANTHROPIC_MODEL    Optional Anthropic model override.
  OPENAI_API_KEY     API key used when provider is openai or auto.
  OPENAI_MODEL       Optional OpenAI model override.
  WENDY_E2E_AI_PROVIDER  Default provider override.
  WENDY_E2E_AI_MODEL     Default model override.
EOF
}

expand_local_path() {
  local path="$1"
  case "$path" in
    '~')
      printf "%s" "${HOME:?}"
      ;;
    '~/'*)
      printf "%s/%s" "${HOME:?}" "${path#~/}"
      ;;
    *)
      printf "%s" "$path"
      ;;
  esac
}

absolute_existing_dir_path() {
  local path
  path="$(expand_local_path "$1")"
  (cd "$path" && pwd)
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --run-dir)
      RUN_DIR="$2"
      shift 2
      ;;
    --package-dir)
      PACKAGE_DIR="$2"
      shift 2
      ;;
    --provider)
      PROVIDER="$2"
      shift 2
      ;;
    --model)
      MODEL="$2"
      shift 2
      ;;
    --overwrite)
      OVERWRITE="true"
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      EXTRA_ARGS+=("$1")
      shift
      ;;
  esac
done

if [[ -z "$RUN_DIR" ]]; then
  echo "ERROR: --run-dir is required." >&2
  usage >&2
  exit 64
fi

RUN_DIR="$(absolute_existing_dir_path "$RUN_DIR")"
PACKAGE_DIR="$(absolute_existing_dir_path "$PACKAGE_DIR")"

COMMAND_ARGS=(
  "run" "swift-e2e-testing" "review"
  "--run-dir" "$RUN_DIR"
  "--provider" "$PROVIDER"
)

if [[ -n "$MODEL" ]]; then
  COMMAND_ARGS+=("--model" "$MODEL")
fi
if [[ "$OVERWRITE" == "true" ]]; then
  COMMAND_ARGS+=("--overwrite")
fi
COMMAND_ARGS+=("${EXTRA_ARGS[@]}")

echo "==> Reviewing Swift E2E results"
echo "    Package:  $PACKAGE_DIR"
echo "    Run dir:  $RUN_DIR"
echo "    Provider: $PROVIDER"
if [[ -n "$MODEL" ]]; then
  echo "    Model:    $MODEL"
fi

(
  cd "$PACKAGE_DIR"
  swift "${COMMAND_ARGS[@]}"
)
