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

Render the WendyAgent Swift E2E HTML report for an existing E2E run directory.

Options:
  --run-dir DIR      Required E2E run directory produced by E2ETest.sh.
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

update_readme_block() {
  local readme_path="$1"
  local status="$2"
  local report_path="$3"
  local start="<!-- swift-e2e-report:start -->"
  local end="<!-- swift-e2e-report:end -->"
  local tmp_path
  tmp_path="$(mktemp)"

  if [[ -f "$readme_path" ]]; then
    awk -v start="$start" -v end="$end" '
      $0 == start { skipping = 1; next }
      $0 == end { skipping = 0; next }
      skipping != 1 { print }
    ' "$readme_path" > "$tmp_path"
  else
    : > "$tmp_path"
  fi

  {
    cat "$tmp_path"
    if [[ -s "$tmp_path" ]]; then
      printf '\n'
    fi
    echo "$start"
    echo "## Report Rendering"
    echo
    echo "- Status: \`$status\`"
    echo "- HTML report: \`$report_path\`"
    if [[ -f "$RUN_DIR/ai-analysis.md" ]]; then
      echo "- AI analysis: \`$RUN_DIR/ai-analysis.md\`"
    fi
    if [[ -f "$RUN_DIR/ai-analysis.json" ]]; then
      echo "- AI analysis JSON: \`$RUN_DIR/ai-analysis.json\`"
    fi
    echo
    echo "### Files after report rendering"
    find "$RUN_DIR" -type f | sort | sed "s#^$RUN_DIR/#- #"
    echo "$end"
  } > "$readme_path"

  rm -f "$tmp_path"
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
README_PATH="$RUN_DIR/README.md"
REPORT_PATH="$RUN_DIR/report.html"

echo "==> Rendering Swift E2E HTML report"
echo "    Package: $PACKAGE_DIR"
echo "    Run dir: $RUN_DIR"
echo "    Output:  $REPORT_PATH"

set +e
(
  cd "$PACKAGE_DIR"
  swift run swift-e2e-testing report --run-dir "$RUN_DIR"
)
REPORT_STATUS=$?
set -e

if [[ "$REPORT_STATUS" -eq 0 && -f "$REPORT_PATH" ]]; then
  update_readme_block "$README_PATH" "generated" "$REPORT_PATH"
  echo "==> Wrote Swift E2E HTML report: $REPORT_PATH"
  exit 0
fi

FAILURE_STATUS="$REPORT_STATUS"
if [[ "$FAILURE_STATUS" -eq 0 ]]; then
  FAILURE_STATUS=1
fi

update_readme_block "$README_PATH" "failed (exit $FAILURE_STATUS)" "$REPORT_PATH"
echo "ERROR: Swift E2E HTML report generation failed." >&2
exit "$FAILURE_STATUS"
