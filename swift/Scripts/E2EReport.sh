#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DEFAULT_PACKAGE_DIR="$SWIFT_DIR/WendyE2ETests"

RUN_DIR=""
PACKAGE_DIR="$DEFAULT_PACKAGE_DIR"

usage() {
  cat <<EOF
Usage: $(basename "$0") --run-dir DIR [--package-dir DIR]

Render the WendyAgent Swift E2E aggregate HTML report for an aggregate run directory.

Options:
  --run-dir DIR      Required aggregate E2E run directory produced by E2EAggregate.sh.
  --package-dir DIR  Swift package directory containing swift-e2e-testing;
                     defaults to $DEFAULT_PACKAGE_DIR.
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
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 64
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
REPORT_PATH="$RUN_DIR/index.html"

aggregate_test_result_files() {
  local suite_dir test_dir target_dir attempt_dir result_path
  for suite_dir in "$RUN_DIR"/*; do
    [[ -d "$suite_dir" ]] || continue
    for test_dir in "$suite_dir"/*; do
      [[ -d "$test_dir" ]] || continue
      for target_dir in "$test_dir"/*; do
        [[ -d "$target_dir" ]] || continue
        for attempt_dir in "$target_dir"/*; do
          [[ -d "$attempt_dir" ]] || continue
          result_path="$attempt_dir/test-results.xml"
          [[ -f "$result_path" ]] || continue
          printf '%s\n' "$result_path"
        done
      done
    done
  done | sort -u
}

sanitize_aggregate_xunit() {
  while IFS= read -r result_path; do
    bash "$SCRIPT_DIR/E2ESanitizeXUnit.sh" --file "$result_path"
  done < <(aggregate_test_result_files)
}

echo "==> Rendering Swift E2E aggregate HTML report"
echo "    Package: $PACKAGE_DIR"
echo "    Run dir: $RUN_DIR"
echo "    Output:  $REPORT_PATH"

sanitize_aggregate_xunit

set +e
(
  cd "$PACKAGE_DIR"
  swift run swift-e2e-testing report --run-dir "$RUN_DIR"
)
report_status=$?
set -e

if [[ "$report_status" -eq 0 && -f "$REPORT_PATH" ]]; then
  echo "==> Wrote Swift E2E aggregate HTML report: $REPORT_PATH"
  exit 0
fi

if [[ "$report_status" -eq 0 ]]; then
  report_status=1
fi

echo "ERROR: Swift E2E aggregate HTML report generation failed." >&2
exit "$report_status"
