#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DEFAULT_PACKAGE_DIR="$SWIFT_DIR/WendyE2ETests"

OUTPUT_DIR=""
PACKAGE_DIR="$DEFAULT_PACKAGE_DIR"
RUN_DIRS=()

usage() {
  cat <<EOF
Usage: $(basename "$0") --output-dir DIR RUN_DIR...

Aggregate one or more Swift E2E run directories into the canonical aggregate layout.

Options:
  --output-dir DIR  Directory where the aggregate root is written.
  --package-dir DIR Swift package directory containing swift-e2e-testing;
                    defaults to $DEFAULT_PACKAGE_DIR.
  --help            Show this help message.
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

absolute_dir_path() {
  local path
  path="$(expand_local_path "$1")"
  mkdir -p "$path"
  (cd "$path" && pwd)
}

absolute_existing_dir_path() {
  local path
  path="$(expand_local_path "$1")"
  (cd "$path" && pwd)
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --output-dir)
      OUTPUT_DIR="$2"
      shift 2
      ;;
    --package-dir)
      PACKAGE_DIR="$2"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    --*)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 64
      ;;
    *)
      RUN_DIRS+=("$1")
      shift
      ;;
  esac
done

if [[ -z "$OUTPUT_DIR" ]]; then
  echo "ERROR: --output-dir is required." >&2
  usage >&2
  exit 64
fi
if [[ ${#RUN_DIRS[@]} -eq 0 ]]; then
  echo "ERROR: at least one RUN_DIR is required." >&2
  usage >&2
  exit 64
fi

OUTPUT_DIR="$(absolute_dir_path "$OUTPUT_DIR")"
PACKAGE_DIR="$(absolute_existing_dir_path "$PACKAGE_DIR")"

ABSOLUTE_RUN_DIRS=()
for run_dir in "${RUN_DIRS[@]}"; do
  ABSOLUTE_RUN_DIRS+=("$(absolute_existing_dir_path "$run_dir")")
done

echo "==> Aggregating Swift E2E runs"
echo "    Package:    $PACKAGE_DIR"
echo "    Output dir: $OUTPUT_DIR"
for run_dir in "${ABSOLUTE_RUN_DIRS[@]}"; do
  echo "    Run:        $run_dir"
done

(
  cd "$PACKAGE_DIR"
  swift run swift-e2e-testing aggregate \
    --output-dir "$OUTPUT_DIR" \
    "${ABSOLUTE_RUN_DIRS[@]}"
)
