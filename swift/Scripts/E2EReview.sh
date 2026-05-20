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

Review WendyAgent Swift E2E run artifacts with Claude Code or Codex.

Options:
  --run-dir DIR      Required E2E run directory produced by E2ETest.sh.
  --package-dir DIR  Swift package directory containing swift-e2e-testing;
                     defaults to $DEFAULT_PACKAGE_DIR.
  --provider NAME    AI agent: auto, claude, codex, or none; defaults to auto.
                    Legacy aliases claude-code/anthropic and openai are accepted.
  --model NAME       Provider model override. Use latest/default to let the
                     agent CLI choose its default model.
  --overwrite        Overwrite existing per-test review.md files.
  --help             Show this help message.

Environment:
  ANTHROPIC_API_KEY  Enables Claude when provider is claude or auto.
  ANTHROPIC_MODEL    Optional Claude model override.
  OPENAI_API_KEY     Enables Codex when provider is codex or auto.
  OPENAI_MODEL       Optional Codex model override.
  WENDY_E2E_AI_PROVIDER  Default provider override.
  WENDY_E2E_AI_MODEL     Default model override.
  WENDY_E2E_CLAUDE_COMMAND Optional shell command for Claude Code. Reads the
                           prompt from WENDY_E2E_AGENT_PROMPT.
  WENDY_E2E_CODEX_COMMAND  Optional shell command for Codex. Reads the prompt
                           from WENDY_E2E_AGENT_PROMPT.
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

review_single_run() {
  local run_dir="$1"
  local command_args=(
    "run" "swift-e2e-testing" "review"
    "--run-dir" "$run_dir"
    "--provider" "$PROVIDER"
  )

  if [[ -n "$MODEL" ]]; then
    command_args+=("--model" "$MODEL")
  fi
  if [[ "$OVERWRITE" == "true" ]]; then
    command_args+=("--overwrite")
  fi
  command_args+=("${EXTRA_ARGS[@]}")

  echo "==> Reviewing Swift E2E results"
  echo "    Package:  $PACKAGE_DIR"
  echo "    Run dir:  $run_dir"
  echo "    Provider: $PROVIDER"
  if [[ -n "$MODEL" ]]; then
    echo "    Model:    $MODEL"
  fi

  (
    cd "$PACKAGE_DIR"
    swift "${command_args[@]}"
  )
}

if [[ -d "$RUN_DIR/_runs" ]]; then
  status=0
  run_paths=()
  shopt -s nullglob
  for run_path in "$RUN_DIR/_runs"/*; do
    [[ -d "$run_path" ]] || continue
    run_paths+=("$run_path")
    review_single_run "$run_path" || { step_status=$?; [[ "$status" -eq 0 ]] && status="$step_status"; }
  done
  shopt -u nullglob

  if [[ ${#run_paths[@]} -gt 0 ]]; then
    (
      cd "$PACKAGE_DIR"
      swift run swift-e2e-testing aggregate \
        --output-dir "$(dirname "$RUN_DIR")" \
        "${run_paths[@]}"
    )
  fi
  exit "$status"
fi

review_single_run "$RUN_DIR"
