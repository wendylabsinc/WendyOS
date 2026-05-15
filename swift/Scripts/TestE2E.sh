#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
PACKAGE_DIR="$SWIFT_DIR/WendyE2ETests"

default_run_id() {
  local timestamp
  timestamp="$(date -u +"%Y%m%dT%H%M%SZ")"

  if [[ -n "${GITHUB_RUN_ID:-}" ]]; then
    printf "gh%s-attempt%s-%s-p%s-r%s%s" \
      "$GITHUB_RUN_ID" \
      "${GITHUB_RUN_ATTEMPT:-1}" \
      "$timestamp" \
      "$$" \
      "$RANDOM" \
      "$RANDOM"
  else
    printf "%s-p%s-r%s%s" "$timestamp" "$$" "$RANDOM" "$RANDOM"
  fi
}

sanitize_run_id() {
  local value="$1"
  value="${value//[![:alnum:]._-]/-}"
  while [[ "$value" == *--* ]]; do
    value="${value//--/-}"
  done
  value="${value#-}"
  value="${value%-}"
  printf "%s" "$value"
}

RUN_ID="$(sanitize_run_id "${WENDY_E2E_RUN_ID:-$(default_run_id)}")"
if [[ -z "$RUN_ID" ]]; then
  RUN_ID="$(default_run_id)"
fi

DEFAULT_RUN_DIR="$SWIFT_DIR/Build/e2e-run.$RUN_ID"

RUN_DIR="${WENDY_E2E_RUN_DIR:-$DEFAULT_RUN_DIR}"
AGENT_USER="${WENDY_E2E_AGENT_USER:-}"
AGENT_ADDRESS="${WENDY_E2E_AGENT_ADDRESS:-}"
AGENT_WORKDIR="${WENDY_E2E_AGENT_WORKING_DIRECTORY:-}"
CLI_ADDRESS="${WENDY_E2E_CLI_ADDRESS:-}"
VERBOSE="${WENDY_E2E_VERBOSE:-false}"
GENERATE_REPORT="${WENDY_E2E_GENERATE_REPORT:-true}"
PARALLEL="${WENDY_E2E_PARALLEL:-false}"
TEST_FILTERS=()

usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Run the WendyAgent Swift E2E tests and write generated files to an E2E run directory.

Options:
  --filter FILTER       Pass a SwiftPM test filter (can be repeated). If omitted,
                        WENDY_E2E_TEST_FILTERS may contain comma-separated
                        filters, otherwise the WendyE2ETests target is run.
  --run-dir DIR         Directory for all generated E2E run files.
  --agent-user USER     Optional SSH user for the agent machine.
  --agent-address HOST  Optional address for the agent machine; defaults to hostname.
  --agent-workdir DIR   Existing swift/ working directory to use for the agent.
  --parallel            Allow SwiftPM to run tests in parallel. Only valid when
                        both CLI and agent machines use local transport.
  --verbose             Print each E2E machine command before it runs.
  --no-report           Do not generate report.html from the E2E run directory.
  --help                Show this help message.

Environment:
  WENDY_E2E_TEST_FILTERS              Comma-separated SwiftPM filters.
  WENDY_E2E_RUN_ID                    Optional run identifier for default paths.
  WENDY_E2E_RUN_DIR                   Defaults to Build/e2e-run.<run-id>.
  WENDY_E2E_AGENT_USER                Optional SSH user for the agent machine.
  WENDY_E2E_AGENT_ADDRESS             Optional address for the agent machine.
  WENDY_E2E_AGENT_WORKING_DIRECTORY   swift/ directory for the agent.
  WENDY_E2E_GENERATE_REPORT           true/false; generates report.html.
  WENDY_E2E_PARALLEL                  true/false; enables SwiftPM parallel tests.
  WENDY_E2E_VERBOSE                   true/false; prints machine commands.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --filter)
      TEST_FILTERS+=("$2")
      shift 2
      ;;
    --run-dir)
      RUN_DIR="$2"
      shift 2
      ;;
    --recording-dir|--records-dir|--artifact-dir)
      echo "ERROR: $1 is no longer supported; use --run-dir instead." >&2
      exit 64
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
    --parallel)
      PARALLEL="true"
      shift
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

parallel_normalized="$(printf '%s' "$PARALLEL" | tr '[:upper:]' '[:lower:]')"
case "$parallel_normalized" in
  true|1|yes|on)
    PARALLEL="true"
    ;;
  false|0|no|off)
    PARALLEL="false"
    ;;
  *)
    echo "ERROR: WENDY_E2E_PARALLEL must be true or false." >&2
    exit 64
    ;;
esac

if [[ "$PARALLEL" == "true" && ( -n "$CLI_ADDRESS" || -n "$AGENT_ADDRESS" ) ]]; then
  echo "ERROR: --parallel is only valid when CLI and agent machines are local." >&2
  echo "Unset WENDY_E2E_CLI_ADDRESS and WENDY_E2E_AGENT_ADDRESS, or omit --parallel." >&2
  exit 64
fi

if [[ -n "$CLI_ADDRESS" ]]; then
  echo "ERROR: TestE2E.sh builds the wendy CLI into the local run directory." >&2
  echo "Unset WENDY_E2E_CLI_ADDRESS to use the managed E2E CLI binary." >&2
  exit 64
fi

absolute_dir_path() {
  mkdir -p "$1"
  (cd "$1" && pwd)
}

RUN_DIR="$(absolute_dir_path "$RUN_DIR")"
CLI_BIN_DIR="$RUN_DIR/cli/bin"
AGENT_BIN_DIR="$RUN_DIR/agent/bin"
TESTS_DIR="$RUN_DIR/tests"
TEST_RESULTS_OUTPUT_BASE="$RUN_DIR/test-results.xml"

rm -rf "$RUN_DIR"
mkdir -p \
  "$CLI_BIN_DIR" \
  "$AGENT_BIN_DIR" \
  "$TESTS_DIR"

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

build_cli() {
  local go_dir="$SWIFT_DIR/../go"
  local wendy_path="$CLI_BIN_DIR/wendy"

  echo "==> Building wendy CLI"
  echo "    Output: $wendy_path"
  (
    cd "$go_dir"
    go build -o "$wendy_path" ./cmd/wendy
  )

  local resolved
  resolved="$(PATH="$CLI_BIN_DIR:$PATH" command -v wendy || true)"
  if [[ "$resolved" != "$wendy_path" ]]; then
    echo "ERROR: managed wendy CLI was not first on PATH." >&2
    echo "Expected: $wendy_path" >&2
    echo "Resolved: ${resolved:-<not found>}" >&2
    exit 1
  fi

  echo "    Version: $("$wendy_path" --version)"
}

generate_html_report() {
  if [[ "$GENERATE_REPORT" != "true" ]]; then
    return
  fi

  echo "==> Generating Swift E2E HTML report"
  (
    cd "$PACKAGE_DIR"
    swift run swift-e2e-testing report --run-dir "$RUN_DIR"
  )
}

write_run_summary() {
  local status="$1"

  mkdir -p "$RUN_DIR"

  {
    echo "# Swift E2E Test Reports"
    echo
    echo "- Exit status: \`$status\`"
    echo "- Run ID: \`$RUN_ID\`"
    echo "- Run directory: \`$RUN_DIR\`"
    echo "- CLI binary: \`$CLI_BIN_DIR/wendy\`"
    echo "- Tests directory: \`$TESTS_DIR\`"
    echo "- Verbose: \`$VERBOSE\`"
    echo "- Parallel: \`$PARALLEL\`"
    echo "- HTML report: \`$GENERATE_REPORT\`"
    if [[ -n "$AGENT_ADDRESS" ]]; then
      echo "- Agent user: \`${AGENT_USER:-<none>}\`"
      echo "- Agent address: \`$AGENT_ADDRESS\`"
      echo "- Agent working directory: \`${AGENT_WORKDIR:-<default>}\`"
    fi
    echo
    echo "## Files"
    find "$RUN_DIR" -type f | sort | sed "s#^$RUN_DIR/#- #"
  } > "$RUN_DIR/README.md"

  echo "==> Wrote Swift E2E run summary: $RUN_DIR/README.md"
}

SWIFT_TEST_ARGS=("test")
if [[ "$PARALLEL" != "true" ]]; then
  SWIFT_TEST_ARGS+=("--no-parallel")
fi
if [[ ${#TEST_FILTERS[@]} -eq 1 ]]; then
  SWIFT_TEST_ARGS+=("--filter" "${TEST_FILTERS[0]}")
else
  joined_filter="$(IFS='|'; echo "${TEST_FILTERS[*]}")"
  SWIFT_TEST_ARGS+=("--filter" "$joined_filter")
fi

build_cli

SWIFT_TEST_ENV=(
  "WENDY_E2E_RUN_ID=$RUN_ID"
  "WENDY_E2E_RUN_DIR=$RUN_DIR"
  "WENDY_E2E_AGENT_USER=$AGENT_USER"
  "WENDY_E2E_AGENT_ADDRESS=$AGENT_ADDRESS"
  "WENDY_E2E_AGENT_WORKING_DIRECTORY=$AGENT_WORKDIR"
  "WENDY_E2E_PARALLEL=$PARALLEL"
  "WENDY_E2E_VERBOSE=$VERBOSE"
)
echo "==> Running Swift E2E tests"
echo "    Package:  $PACKAGE_DIR"
echo "    Run ID:   $RUN_ID"
echo "    Run dir:  $RUN_DIR"
echo "    CLI:      $CLI_BIN_DIR/wendy"
echo "    Tests:    $TESTS_DIR"
echo "    Report:   $RUN_DIR/report.html"
echo "    Filters:  ${TEST_FILTERS[*]}"
echo "    Verbose:  $VERBOSE"
echo "    Parallel: $PARALLEL"
echo "    HTML:     $GENERATE_REPORT"
if [[ -n "$AGENT_ADDRESS" ]]; then
  echo "    Agent:   $(ssh_target):${AGENT_WORKDIR:-<default>}"
fi

set +e
(
  cd "$PACKAGE_DIR"
  env "${SWIFT_TEST_ENV[@]}" \
    swift "${SWIFT_TEST_ARGS[@]}" \
    --xunit-output "$TEST_RESULTS_OUTPUT_BASE"
)
TEST_STATUS=$?
set -e

generate_html_report
write_run_summary "$TEST_STATUS"
exit "$TEST_STATUS"
