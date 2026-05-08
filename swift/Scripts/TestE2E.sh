#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd "$SWIFT_DIR/.." && pwd)"
PACKAGE_DIR="$SWIFT_DIR/WendyAgentE2ETests"
DEFAULT_FIXTURES_DIR="$REPO_ROOT/.github/swift-e2e-tests"
DEFAULT_RECORDS_DIR="$PACKAGE_DIR/.build/e2e-test-records.current"
DEFAULT_ARTIFACT_DIR="$SWIFT_DIR/Build/E2E"

FIXTURES_DIR="${WENDY_AGENT_E2E_FIXTURES_DIR:-$DEFAULT_FIXTURES_DIR}"
RECORDS_DIR="${WENDY_AGENT_E2E_TEST_RECORDS_DIR:-$DEFAULT_RECORDS_DIR}"
ARTIFACT_DIR="${WENDY_AGENT_E2E_ARTIFACT_DIR:-$DEFAULT_ARTIFACT_DIR}"
REPORT_ZIP="${WENDY_AGENT_E2E_REPORT_ZIP:-$ARTIFACT_DIR/swift-e2e-test-reports.zip}"
AGENT_SSH="${WENDY_AGENT_E2E_AGENT_SSH:-}"
AGENT_WORKDIR="${WENDY_AGENT_E2E_AGENT_WORKING_DIRECTORY:-}"
SYNC_AGENT="${WENDY_AGENT_E2E_SYNC_AGENT:-auto}"
VERBOSE="${WENDY_AGENT_E2E_VERBOSE:-false}"
PROGRESS_INTERVAL="${WENDY_AGENT_E2E_PROGRESS_INTERVAL:-10}"
TEST_OUTPUT_LOG="${WENDY_AGENT_E2E_TEST_OUTPUT_LOG:-}"
TEST_FILTERS=()

usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Run the WendyAgent Swift E2E tests and package the generated Markdown command
records as a zip artifact.

Options:
  --filter FILTER       Pass a SwiftPM test filter (can be repeated). If omitted,
                        WENDY_AGENT_E2E_TEST_FILTERS may contain comma-separated
                        filters, otherwise the WendyAgentE2ETests target is run.
  --records-dir DIR     Directory for generated *.md command records.
  --artifact-dir DIR    Directory for the final zip artifact.
  --report-zip PATH     Path to the final zip artifact.
  --test-output-log PATH Path to the captured swift test stdout/stderr log.
  --fixtures-dir DIR    Fixture directory exposed to tests.
  --agent-ssh SSH       Optional SSH target for the agent machine; omitted runs locally.
  --agent-workdir DIR   Existing swift/ working directory to use for the agent.
  --no-agent-sync       Do not rsync this checkout to --agent-ssh.
  --verbose             Print each E2E machine command before it runs.
  --help                Show this help message.

Environment:
  WENDY_AGENT_E2E_TEST_FILTERS              Comma-separated SwiftPM filters.
  WENDY_AGENT_E2E_AGENT_SSH                 Optional SSH target for the agent machine.
  WENDY_AGENT_E2E_AGENT_WORKING_DIRECTORY   swift/ directory for the agent.
  WENDY_AGENT_E2E_SYNC_AGENT                auto, true, or false.
  WENDY_AGENT_E2E_FIXTURES_DIR              Defaults to .github/swift-e2e-tests.
  WENDY_AGENT_E2E_TEST_RECORDS_DIR          Defaults to package .build records dir.
  WENDY_AGENT_E2E_TEST_OUTPUT_LOG           Defaults to artifact dir swift-e2e-test-output.log.
  WENDY_AGENT_E2E_VERBOSE                   true/false; prints machine commands.
  WENDY_AGENT_E2E_PROGRESS_INTERVAL         Seconds between progress heartbeats; 0 disables.
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
    --test-output-log)
      TEST_OUTPUT_LOG="$2"
      shift 2
      ;;
    --fixtures-dir)
      FIXTURES_DIR="$2"
      shift 2
      ;;
    --agent-ssh)
      AGENT_SSH="$2"
      shift 2
      ;;
    --agent-workdir)
      AGENT_WORKDIR="$2"
      shift 2
      ;;
    --no-agent-sync)
      SYNC_AGENT="false"
      shift
      ;;
    --verbose)
      VERBOSE="true"
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

if [[ ${#TEST_FILTERS[@]} -eq 0 && -n "${WENDY_AGENT_E2E_TEST_FILTERS:-}" ]]; then
  IFS=',' read -ra RAW_FILTERS <<< "${WENDY_AGENT_E2E_TEST_FILTERS}"
  for filter in "${RAW_FILTERS[@]}"; do
    filter="$(echo "$filter" | xargs)"
    [[ -n "$filter" ]] && TEST_FILTERS+=("$filter")
  done
fi

if [[ ${#TEST_FILTERS[@]} -eq 0 ]]; then
  TEST_FILTERS+=("WendyAgentE2ETests")
fi

if [[ -z "$TEST_OUTPUT_LOG" ]]; then
  TEST_OUTPUT_LOG="$ARTIFACT_DIR/swift-e2e-test-output.log"
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
TEST_OUTPUT_LOG="$(absolute_file_path "$TEST_OUTPUT_LOG")"

mkdir -p "$ARTIFACT_DIR"
mkdir -p "$(dirname "$TEST_OUTPUT_LOG")"
rm -f "$TEST_OUTPUT_LOG"
rm -rf "$RECORDS_DIR"
mkdir -p "$RECORDS_DIR"

if [[ ! -d "$FIXTURES_DIR" ]]; then
  echo "ERROR: Swift E2E fixtures directory not found: $FIXTURES_DIR" >&2
  exit 1
fi
FIXTURES_DIR="$(cd "$FIXTURES_DIR" && pwd)"

shell_quote() {
  printf "%q" "$1"
}

sync_agent_checkout_if_needed() {
  if [[ -z "$AGENT_SSH" ]]; then
    return 0
  fi

  if [[ "$SYNC_AGENT" == "false" ]]; then
    return 0
  fi

  if [[ -n "$AGENT_WORKDIR" && "$SYNC_AGENT" == "auto" ]]; then
    return 0
  fi

  if ! command -v rsync >/dev/null 2>&1; then
    echo "ERROR: rsync is required when WENDY_AGENT_E2E_AGENT_SSH is set" >&2
    exit 1
  fi

  local run_id="${GITHUB_RUN_ID:-local}"
  local run_attempt="${GITHUB_RUN_ATTEMPT:-1}"
  local remote_root="wendy-agent-swift-e2e/${run_id}-${run_attempt}"
  local remote_swift_dir="$remote_root/swift"

  echo "==> Syncing checkout to $AGENT_SSH:$remote_root"
  ssh -o StrictHostKeyChecking=no "$AGENT_SSH" "mkdir -p $(shell_quote "$remote_root")"
  rsync -az --delete \
    -e 'ssh -o StrictHostKeyChecking=no' \
    --exclude '.git/' \
    --exclude '.build/' \
    --exclude 'Build/' \
    --exclude 'swift/WendyAgentCore/.build/' \
    --exclude 'swift/WendyAgentE2ETests/.build/' \
    --exclude 'swift/Build/' \
    "$REPO_ROOT/" "$AGENT_SSH:$remote_root/"

  AGENT_WORKDIR="$remote_swift_dir"
}

emit_progress_message() {
  local message="$1"
  printf "%s\n" "$message" | tee -a "$TEST_OUTPUT_LOG" >&2
}

stop_progress_reporter() {
  if [[ -n "${PROGRESS_PID:-}" ]]; then
    kill "$PROGRESS_PID" 2>/dev/null || true
    wait "$PROGRESS_PID" 2>/dev/null || true
    PROGRESS_PID=""
  fi
}

progress_reporter() {
  local last_count=""
  local last_latest=""

  while true; do
    sleep "$PROGRESS_INTERVAL" || return 0

    local count=0
    local latest=""
    local file
    while IFS= read -r -d '' file; do
      count=$((count + 1))
      if [[ -z "$latest" || "$file" -nt "$latest" ]]; then
        latest="$file"
      fi
    done < <(find "$RECORDS_DIR" -maxdepth 1 -type f -name '*.md' -print0 2>/dev/null)

    local latest_name="<none>"
    if [[ -n "$latest" ]]; then
      latest_name="$(basename "$latest")"
    fi

    if [[ "$count" == "$last_count" && "$latest_name" == "$last_latest" ]]; then
      emit_progress_message "==> Swift E2E progress: still running; $count command record(s), latest: $latest_name"
    else
      emit_progress_message "==> Swift E2E progress: $count command record(s), latest: $latest_name"
    fi

    last_count="$count"
    last_latest="$latest_name"
  done
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

  if [[ -f "$TEST_OUTPUT_LOG" ]]; then
    cp "$TEST_OUTPUT_LOG" "$staging_dir/"
  fi

  {
    echo "# Swift E2E Test Reports"
    echo
    echo "- Exit status: \`$status\`"
    echo "- Records directory: \`$RECORDS_DIR\`"
    echo "- Fixtures directory: \`$FIXTURES_DIR\`"
    echo "- Test output log: \`$TEST_OUTPUT_LOG\`"
    echo "- Verbose: \`$VERBOSE\`"
    if [[ -n "$AGENT_SSH" ]]; then
      echo "- Agent SSH: \`$AGENT_SSH\`"
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

sync_agent_checkout_if_needed

SWIFT_TEST_ARGS=("test" "--no-parallel")
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
echo "    Log:      $TEST_OUTPUT_LOG"
echo "    Filters:  ${TEST_FILTERS[*]}"
echo "    Verbose:  $VERBOSE"
echo "    Progress: every ${PROGRESS_INTERVAL}s"
if [[ -n "$AGENT_SSH" ]]; then
  echo "    Agent:   $AGENT_SSH:${AGENT_WORKDIR:-<default>}"
fi

PROGRESS_PID=""
if [[ "$PROGRESS_INTERVAL" != "0" ]]; then
  progress_reporter &
  PROGRESS_PID="$!"
  trap stop_progress_reporter EXIT INT TERM
fi

set +e
(
  cd "$PACKAGE_DIR"
  WENDY_AGENT_E2E_FIXTURES_DIR="$FIXTURES_DIR" \
  WENDY_AGENT_E2E_TEST_RECORDS_DIR="$RECORDS_DIR" \
  WENDY_AGENT_E2E_AGENT_SSH="$AGENT_SSH" \
  WENDY_AGENT_E2E_AGENT_WORKING_DIRECTORY="$AGENT_WORKDIR" \
  WENDY_AGENT_E2E_VERBOSE="$VERBOSE" \
  swift "${SWIFT_TEST_ARGS[@]}"
) 2>&1 | tee "$TEST_OUTPUT_LOG"
TEST_STATUS=${PIPESTATUS[0]}
stop_progress_reporter
set -e

collect_reports "$TEST_STATUS"
exit "$TEST_STATUS"
