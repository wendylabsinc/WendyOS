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
html_escape() {
  local value="$1"
  value="${value//&/&amp;}"
  value="${value//</&lt;}"
  value="${value//>/&gt;}"
  value="${value//\"/&quot;}"
  printf "%s" "$value"
}

render_single_report() {
  local run_dir="$1"
  local report_path="$run_dir/report.html"

  echo "==> Rendering Swift E2E HTML report"
  echo "    Package: $PACKAGE_DIR"
  echo "    Run dir: $run_dir"
  echo "    Output:  $report_path"

  bash "$SCRIPT_DIR/E2ESanitizeXUnit.sh" --run-dir "$run_dir"

  set +e
  (
    cd "$PACKAGE_DIR"
    swift run swift-e2e-testing report --run-dir "$run_dir"
  )
  local report_status=$?
  set -e

  if [[ "$report_status" -eq 0 && -f "$report_path" ]]; then
    echo "==> Wrote Swift E2E HTML report: $report_path"
    return 0
  fi

  if [[ "$report_status" -eq 0 ]]; then
    report_status=1
  fi
  return "$report_status"
}

render_aggregate_index() {
  local index_path="$RUN_DIR/index.html"
  local links=()
  local run_path run_name href

  shopt -s nullglob
  for run_path in "$RUN_DIR/_runs"/*; do
    [[ -d "$run_path" ]] || continue
    run_name="${run_path##*/}"
    href="_runs/$run_name/report.html"
    if [[ -f "$run_path/report.html" ]]; then
      links+=("<li><a href=\"$(html_escape "$href")\">$(html_escape "$run_name")</a></li>")
    fi
  done
  shopt -u nullglob

  {
    echo "<!doctype html>"
    echo "<html lang=\"en\">"
    echo "<head><meta charset=\"utf-8\"><title>Swift E2E Aggregate</title></head>"
    echo "<body>"
    echo "<h1>Swift E2E Aggregate</h1>"
    echo "<ul>"
    printf '%s\n' "${links[@]}"
    echo "</ul>"
    echo "</body>"
    echo "</html>"
  } > "$index_path"

  echo "==> Wrote Swift E2E aggregate index: $index_path"
}

if [[ -d "$RUN_DIR/_runs" ]]; then
  status=0
  shopt -s nullglob
  for run_path in "$RUN_DIR/_runs"/*; do
    [[ -d "$run_path" ]] || continue
    render_single_report "$run_path" || { step_status=$?; [[ "$status" -eq 0 ]] && status="$step_status"; }
  done
  shopt -u nullglob
  if [[ "$status" -eq 0 ]]; then
    render_aggregate_index
  fi
  exit "$status"
fi

render_single_report "$RUN_DIR" || {
  echo "ERROR: Swift E2E HTML report generation failed." >&2
  exit $?
}
