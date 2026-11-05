#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DEFAULT_PACKAGE_DIR="$SWIFT_DIR/WendyE2ETests"

RUN_DIR=""
PACKAGE_DIR="$DEFAULT_PACKAGE_DIR"
DIFF=""
HARNESS="${WENDY_E2E_REVIEW_HARNESS:-}"
OVERWRITE="false"
EXTRA_ARGS=()

usage() {
  cat <<EOF
Usage: $(basename "$0") --run-dir RUN_DIR [OPTIONS]

Review WendyAgent Swift E2E run artifacts with a single AI review pass.

Options:
  --run-dir DIR      Required E2E run directory produced by E2EAggregate.sh.
  --package-dir DIR  Swift package directory containing swift-e2e-testing;
                     defaults to $DEFAULT_PACKAGE_DIR.
  --diff RANGE       Git diff range for diff-scoped review, for example
                     origin/main...HEAD.
  --harness NAME     AI review harness: auto, claude, or codex.
  --review-prompt PATH
                     Custom single-pass review prompt path.
  --overwrite        Overwrite existing run review files.
  --help             Show this help message.

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
    --diff)
      DIFF="$2"
      shift 2
      ;;
    --harness)
      HARNESS="$2"
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
  )

  if [[ -n "$DIFF" ]]; then
    command_args+=("--diff" "$DIFF")
  fi
  if [[ -n "$HARNESS" ]]; then
    command_args+=("--harness" "$HARNESS")
  fi
  if [[ "$OVERWRITE" == "true" ]]; then
    command_args+=("--overwrite")
  fi
  if (( ${#EXTRA_ARGS[@]} > 0 )); then
    command_args+=("${EXTRA_ARGS[@]}")
  fi

  echo "==> Reviewing Swift E2E run results in one AI pass"
  echo "    Package:  $PACKAGE_DIR"
  echo "    Run dir:  $run_dir"
  if [[ -n "$DIFF" ]]; then
    echo "    Diff:     $DIFF"
  fi
  if [[ -n "$HARNESS" ]]; then
    echo "    Harness:  $HARNESS"
  fi

  (
    cd "$PACKAGE_DIR"
    swift "${command_args[@]}"
  )
}

review_single_run "$RUN_DIR"
