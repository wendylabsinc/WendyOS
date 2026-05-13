#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd "$SWIFT_DIR/.." && pwd)"
PACKAGE_DIR="$SWIFT_DIR/WendyE2ETests"
DEFAULT_FIXTURES_DIR="$REPO_ROOT/.github/swift-e2e-tests"
DEFAULT_RECORDS_DIR="$PACKAGE_DIR/.build/e2e-test-records.current"
DEFAULT_ARTIFACT_DIR="$SWIFT_DIR/Build/E2E"

FIXTURES_DIR="${WENDY_E2E_FIXTURES_DIR:-$DEFAULT_FIXTURES_DIR}"
RECORDS_DIR="${WENDY_E2E_TEST_RECORDS_DIR:-$DEFAULT_RECORDS_DIR}"
ARTIFACT_DIR="${WENDY_E2E_ARTIFACT_DIR:-$DEFAULT_ARTIFACT_DIR}"
REPORT_ZIP="${WENDY_E2E_REPORT_ZIP:-$ARTIFACT_DIR/swift-e2e-test-reports.zip}"
AGENT_USER="${WENDY_E2E_AGENT_USER:-}"
AGENT_ADDRESS="${WENDY_E2E_AGENT_ADDRESS:-}"
AGENT_WORKDIR="${WENDY_E2E_AGENT_WORKING_DIRECTORY:-}"
VERBOSE="${WENDY_E2E_VERBOSE:-false}"
GENERATE_REPORT="${WENDY_E2E_GENERATE_REPORT:-true}"
TEST_FILTERS=()

usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Run the WendyAgent Swift E2E tests and package the generated Markdown command
records as a zip artifact.

Options:
  --filter FILTER       Pass a SwiftPM test filter (can be repeated). If omitted,
                        WENDY_E2E_TEST_FILTERS may contain comma-separated
                        filters, otherwise the WendyE2ETests target is run.
  --records-dir DIR     Directory for generated *.md command records.
  --artifact-dir DIR    Directory for the final zip artifact.
  --report-zip PATH     Path to the final zip artifact.
  --fixtures-dir DIR    Fixture directory exposed to tests.
  --agent-user USER     Optional SSH user for the agent machine.
  --agent-address HOST  Optional address for the agent machine; defaults to hostname.
  --agent-workdir DIR   Existing swift/ working directory to use for the agent.
  --verbose             Print each E2E machine command before it runs.
  --no-report           Do not generate index.html from command records.
  --help                Show this help message.

Environment:
  WENDY_E2E_TEST_FILTERS              Comma-separated SwiftPM filters.
  WENDY_E2E_AGENT_USER                Optional SSH user for the agent machine.
  WENDY_E2E_AGENT_ADDRESS             Optional address for the agent machine.
  WENDY_E2E_AGENT_WORKING_DIRECTORY   swift/ directory for the agent.
  WENDY_E2E_FIXTURES_DIR              Defaults to .github/swift-e2e-tests.
  WENDY_E2E_TEST_RECORDS_DIR          Defaults to package .build records dir.
  WENDY_E2E_GENERATE_REPORT           true/false; generates index.html.
  WENDY_E2E_VERBOSE                   true/false; prints machine commands.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --filter)
      TEST_FILTERS+=("$2")
      shift 2
      ;;
    --records-dir)
      RECORDS_DIR="$2"
      shift 2
      ;;
    --artifact-dir)
      ARTIFACT_DIR="$2"
      REPORT_ZIP="$ARTIFACT_DIR/swift-e2e-test-reports.zip"
      shift 2
      ;;
    --report-zip)
      REPORT_ZIP="$2"
      ARTIFACT_DIR="$(dirname "$REPORT_ZIP")"
      shift 2
      ;;
    --fixtures-dir)
      FIXTURES_DIR="$2"
      shift 2
      ;;
    --agent-user)
      AGENT_USER="$2"
      shift 2
      ;;
    --agent-address)
      AGENT_ADDRESS="$2"
      shift 2
      ;;
    --agent-workdir)
      AGENT_WORKDIR="$2"
      shift 2
      ;;
    --verbose)
      VERBOSE="true"
      shift
      ;;
    --no-report)
      GENERATE_REPORT="false"
      shift
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

if [[ ${#TEST_FILTERS[@]} -eq 0 && -n "${WENDY_E2E_TEST_FILTERS:-}" ]]; then
  IFS=',' read -ra RAW_FILTERS <<< "${WENDY_E2E_TEST_FILTERS}"
  for filter in "${RAW_FILTERS[@]}"; do
    filter="$(echo "$filter" | xargs)"
    [[ -n "$filter" ]] && TEST_FILTERS+=("$filter")
  done
fi

if [[ ${#TEST_FILTERS[@]} -eq 0 ]]; then
  TEST_FILTERS+=("WendyE2ETests")
fi

absolute_dir_path() {
  mkdir -p "$1"
  (cd "$1" && pwd)
}

absolute_file_path() {
  local path="$1"
  local dir
  local base
  dir="$(dirname "$path")"
  base="$(basename "$path")"
  mkdir -p "$dir"
  dir="$(cd "$dir" && pwd)"
  printf "%s/%s" "$dir" "$base"
}

ARTIFACT_DIR="$(absolute_dir_path "$ARTIFACT_DIR")"
RECORDS_DIR="$(absolute_dir_path "$RECORDS_DIR")"
REPORT_ZIP="$(absolute_file_path "$REPORT_ZIP")"

mkdir -p "$ARTIFACT_DIR"
rm -rf "$RECORDS_DIR"
mkdir -p "$RECORDS_DIR"

if [[ ! -d "$FIXTURES_DIR" ]]; then
  echo "ERROR: Swift E2E fixtures directory not found: $FIXTURES_DIR" >&2
  exit 1
fi
FIXTURES_DIR="$(cd "$FIXTURES_DIR" && pwd)"

ssh_target() {
  local host="$AGENT_ADDRESS"
  if [[ "$host" == *:* ]]; then
    host="[$host]"
  fi

  if [[ -n "$AGENT_USER" ]]; then
    printf "%s@%s" "$AGENT_USER" "$host"
  else
    printf "%s" "$host"
  fi
}

generate_html_report() {
  if [[ "$GENERATE_REPORT" != "true" ]]; then
    return
  fi

  echo "==> Generating Swift E2E HTML report"
  (
    cd "$PACKAGE_DIR"
    swift run swift-e2e-testing report \
      --records-dir "$RECORDS_DIR" \
      --output "$RECORDS_DIR/index.html"
  )
}

collect_reports() {
  local status="$1"
  local staging_dir="$ARTIFACT_DIR/swift-e2e-test-reports"

  rm -rf "$staging_dir" "$REPORT_ZIP"
  mkdir -p "$staging_dir"

  if [[ -d "$RECORDS_DIR" ]]; then
    find "$RECORDS_DIR" -maxdepth 1 -type f \( -name '*.md' -o -name '*.html' \) -print0 \
      | while IFS= read -r -d '' file; do
          cp "$file" "$staging_dir/"
        done
  fi

  {
    echo "# Swift E2E Test Reports"
    echo
    echo "- Exit status: \`$status\`"
    echo "- Records directory: \`$RECORDS_DIR\`"
    echo "- Fixtures directory: \`$FIXTURES_DIR\`"
    echo "- Verbose: \`$VERBOSE\`"
    echo "- HTML report: \`$GENERATE_REPORT\`"
    if [[ -n "$AGENT_ADDRESS" ]]; then
      echo "- Agent user: \`${AGENT_USER:-<none>}\`"
      echo "- Agent address: \`$AGENT_ADDRESS\`"
      echo "- Agent working directory: \`${AGENT_WORKDIR:-<default>}\`"
    fi
    echo
    echo "## Files"
    find "$staging_dir" -maxdepth 1 -type f | sort | sed "s#^$staging_dir/#- #"
  } > "$staging_dir/README.md"

  local report_zip_dir
  local report_zip
  report_zip_dir="$(dirname "$REPORT_ZIP")"
  mkdir -p "$report_zip_dir"
  report_zip_dir="$(cd "$report_zip_dir" && pwd)"
  report_zip="$report_zip_dir/$(basename "$REPORT_ZIP")"

  if command -v zip >/dev/null 2>&1; then
    (cd "$staging_dir" && zip -qr "$report_zip" .)
  else
    (cd "$ARTIFACT_DIR" && ditto -c -k --keepParent "$(basename "$staging_dir")" "$report_zip")
  fi

  echo "==> Wrote Swift E2E reports zip: $report_zip"
}

SWIFT_TEST_ARGS=("test")
if [[ ${#TEST_FILTERS[@]} -eq 1 ]]; then
  SWIFT_TEST_ARGS+=("--filter" "${TEST_FILTERS[0]}")
else
  joined_filter="$(IFS='|'; echo "${TEST_FILTERS[*]}")"
  SWIFT_TEST_ARGS+=("--filter" "$joined_filter")
fi

echo "==> Running Swift E2E tests"
echo "    Package:  $PACKAGE_DIR"
echo "    Fixtures: $FIXTURES_DIR"
echo "    Records:  $RECORDS_DIR"
echo "    Filters:  ${TEST_FILTERS[*]}"
echo "    Verbose:  $VERBOSE"
echo "    Report:   $GENERATE_REPORT"
if [[ -n "$AGENT_ADDRESS" ]]; then
  echo "    Agent:   $(ssh_target):${AGENT_WORKDIR:-<default>}"
fi

set +e
(
  cd "$PACKAGE_DIR"
  WENDY_E2E_FIXTURES_DIR="$FIXTURES_DIR" \
  WENDY_E2E_TEST_RECORDS_DIR="$RECORDS_DIR" \
  WENDY_E2E_AGENT_USER="$AGENT_USER" \
  WENDY_E2E_AGENT_ADDRESS="$AGENT_ADDRESS" \
  WENDY_E2E_AGENT_WORKING_DIRECTORY="$AGENT_WORKDIR" \
  WENDY_E2E_VERBOSE="$VERBOSE" \
  swift "${SWIFT_TEST_ARGS[@]}"
)
TEST_STATUS=$?
set -e

generate_html_report
collect_reports "$TEST_STATUS"
exit "$TEST_STATUS"
