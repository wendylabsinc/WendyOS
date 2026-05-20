#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
PACKAGE_DIR="$SWIFT_DIR/WendyE2ETests"

default_run_id() {
  local output_dir="$1"
  local run_name="${2:-local}"
  local evaluation_id attempt base path leaf suffix max_attempt=0

  if [[ -n "${GITHUB_RUN_ID:-}" ]]; then
    evaluation_id="gh${GITHUB_RUN_ID}"
    printf -v attempt "%04d" "${GITHUB_RUN_ATTEMPT:-1}"
  else
    evaluation_id="local$(date -u +"%y%m%d")"
    base="swift-e2e-tests.${evaluation_id}.${run_name}"
    shopt -s nullglob
    for path in "$output_dir/$base".[0-9][0-9][0-9][0-9]; do
      leaf="${path##*/}"
      suffix="${leaf##*.}"
      if [[ "$suffix" =~ ^[0-9]{4}$ && $((10#$suffix)) -gt $max_attempt ]]; then
        max_attempt=$((10#$suffix))
      fi
    done
    shopt -u nullglob
    printf -v attempt "%04d" "$((max_attempt + 1))"
  fi

  printf "swift-e2e-tests.%s.%s.%s" "$evaluation_id" "$run_name" "$attempt"
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

RUN_ID="${WENDY_E2E_RUN_ID:-}"
RUN_NAME="${WENDY_E2E_RUN_NAME:-local}"
OUTPUT_DIR="${WENDY_E2E_OUTPUT_DIR:-}"
CLI_ROOT_DIR="${WENDY_E2E_CLI_ROOT_DIR:-}"
CLI_REPO_DIR="${WENDY_E2E_CLI_REPO_DIR:-}"
CLI_BIN_DIR="${WENDY_E2E_CLI_BIN_DIR:-}"
CLI_USER="${WENDY_E2E_CLI_USER:-}"
CLI_ADDRESS="${WENDY_E2E_CLI_ADDRESS:-}"
CLI_OS="${WENDY_E2E_CLI_OS:-}"
AGENT_ROOT_DIR="${WENDY_E2E_AGENT_ROOT_DIR:-}"
AGENT_REPO_DIR="${WENDY_E2E_AGENT_REPO_DIR:-}"
AGENT_BIN_DIR="${WENDY_E2E_AGENT_BIN_DIR:-}"
AGENT_USER="${WENDY_E2E_AGENT_USER:-}"
AGENT_ADDRESS="${WENDY_E2E_AGENT_ADDRESS:-}"
AGENT_OS="${WENDY_E2E_AGENT_OS:-}"
TRANSPORT="${WENDY_E2E_TRANSPORT:-}"
ISOLATION="${WENDY_E2E_ISOLATION:-per-test}"
VERBOSE="${WENDY_E2E_VERBOSE:-false}"
PARALLEL="${WENDY_E2E_PARALLEL:-false}"
TEST_FILTERS=()

normalize_bool() {
  local name="$1"
  local value
  value="$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')"

  case "$value" in
    true|1|yes|on|enabled)
      printf "true"
      ;;
    false|0|no|off|disabled)
      printf "false"
      ;;
    *)
      echo "ERROR: $name must be true or false." >&2
      exit 64
      ;;
  esac
}

normalize_isolation() {
  local value
  value="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"

  case "$value" in
    none|per-run|per-test)
      printf "%s" "$value"
      ;;
    *)
      echo "ERROR: WENDY_E2E_ISOLATION must be none, per-run, or per-test." >&2
      exit 64
      ;;
  esac
}

usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Run the WendyAgent Swift E2E tests and write generated files to an E2E run directory.

Options:
  --filter FILTER       Pass a SwiftPM test filter (can be repeated). If omitted,
                        WENDY_E2E_TEST_FILTERS may contain comma-separated
                        filters, otherwise the WendyE2ETests target is run.
  --run-id ID           Run identifier used for default paths.
  --output-dir DIR      Required local root directory for runner output runs.
  --cli-root-dir DIR    Root directory for CLI machine runs.
  --cli-repo-dir DIR    wendy-agent repo root on the CLI machine.
  --cli-bin-dir DIR     Directory where the built wendy CLI is written.
  --cli-user USER       Optional SSH user for the CLI machine.
  --cli-address HOST    Optional address for the CLI machine.
  --cli-os OS           Optional OS override for the CLI machine.
  --agent-root-dir DIR  Root directory for agent machine runs.
  --agent-repo-dir DIR  wendy-agent repo root on the agent machine.
  --agent-bin-dir DIR   Optional directory prepended to PATH on the agent machine.
  --agent-user USER     Optional SSH user for the agent machine.
  --agent-address HOST  Optional address for the agent machine; defaults to hostname.
  --agent-os OS         Optional OS override for the agent machine.
  --isolation MODE      Sandbox isolation: none, per-run, or per-test; defaults to per-test.
  --parallel            Allow SwiftPM to run tests in parallel. Only valid when
                        both CLI and agent machines use local transport.
  --no-parallel         Do not run SwiftPM tests in parallel.
  --verbose             Print each E2E machine command before it runs.
  --no-verbose          Do not print each E2E machine command before it runs.
  --help                Show this help message.

Environment:
  WENDY_E2E_TEST_FILTERS              Comma-separated SwiftPM filters.
  WENDY_E2E_RUN_ID                    Optional run identifier for default paths.
  WENDY_E2E_RUN_NAME                  Run name for generated run IDs; defaults to local.
  WENDY_E2E_OUTPUT_DIR                Required local root directory for runner output runs.
  WENDY_E2E_CLI_ROOT_DIR              Root directory for CLI machine runs.
  WENDY_E2E_CLI_REPO_DIR              wendy-agent repo root on the CLI machine.
  WENDY_E2E_CLI_BIN_DIR               Directory where the built wendy CLI is written.
  WENDY_E2E_CLI_USER                  Optional SSH user for the CLI machine.
  WENDY_E2E_CLI_ADDRESS               Optional address for the CLI machine.
  WENDY_E2E_CLI_OS                    Optional OS override for the CLI machine.
  WENDY_E2E_AGENT_ROOT_DIR            Root directory for agent machine runs.
  WENDY_E2E_AGENT_REPO_DIR            wendy-agent repo root on the agent machine.
  WENDY_E2E_AGENT_BIN_DIR             Optional directory prepended to PATH on the agent machine.
  WENDY_E2E_AGENT_USER                Optional SSH user for the agent machine.
  WENDY_E2E_AGENT_ADDRESS             Optional address for the agent machine.
  WENDY_E2E_AGENT_OS                  Optional OS override for the agent machine.
  WENDY_E2E_TRANSPORT                 Optional transport label for report metadata.
  WENDY_E2E_ISOLATION                 none, per-run, or per-test; defaults to per-test.
  WENDY_E2E_PARALLEL                  Boolean; enables SwiftPM parallel tests.
  WENDY_E2E_VERBOSE                   Boolean; prints machine commands.

Boolean values accept true/false, 1/0, yes/no, on/off, enabled/disabled.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --filter)
      TEST_FILTERS+=("$2")
      shift 2
      ;;
    --run-id)
      RUN_ID="$2"
      shift 2
      ;;
    --output-dir)
      OUTPUT_DIR="$2"
      shift 2
      ;;
    --cli-root-dir)
      CLI_ROOT_DIR="$2"
      shift 2
      ;;
    --cli-repo-dir)
      CLI_REPO_DIR="$2"
      shift 2
      ;;
    --cli-bin-dir)
      CLI_BIN_DIR="$2"
      shift 2
      ;;
    --cli-user)
      CLI_USER="$2"
      shift 2
      ;;
    --cli-address)
      CLI_ADDRESS="$2"
      shift 2
      ;;
    --cli-os)
      CLI_OS="$2"
      shift 2
      ;;
    --agent-root-dir)
      AGENT_ROOT_DIR="$2"
      shift 2
      ;;
    --agent-repo-dir)
      AGENT_REPO_DIR="$2"
      shift 2
      ;;
    --agent-bin-dir)
      AGENT_BIN_DIR="$2"
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
    --agent-os)
      AGENT_OS="$2"
      shift 2
      ;;
    --isolation)
      ISOLATION="$2"
      shift 2
      ;;
    --parallel)
      PARALLEL="true"
      shift
      ;;
    --no-parallel)
      PARALLEL="false"
      shift
      ;;
    --verbose)
      VERBOSE="true"
      shift
      ;;
    --no-verbose)
      VERBOSE="false"
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

if [[ -z "$OUTPUT_DIR" ]]; then
  echo "ERROR: --output-dir or WENDY_E2E_OUTPUT_DIR is required." >&2
  exit 64
fi

if [[ -z "$CLI_ROOT_DIR" ]]; then
  if [[ -n "$CLI_ADDRESS" ]]; then
    CLI_ROOT_DIR="\$HOME/.wendy/e2e"
  else
    CLI_ROOT_DIR="${HOME:?}/.wendy/e2e"
  fi
fi

if [[ -z "$AGENT_ROOT_DIR" ]]; then
  if [[ -n "$AGENT_ADDRESS" ]]; then
    AGENT_ROOT_DIR="\$HOME/.wendy/e2e"
  else
    AGENT_ROOT_DIR="${HOME:?}/.wendy/e2e"
  fi
fi

ISOLATION="$(normalize_isolation "$ISOLATION")"
PARALLEL="$(normalize_bool "WENDY_E2E_PARALLEL" "$PARALLEL")"
VERBOSE="$(normalize_bool "WENDY_E2E_VERBOSE" "$VERBOSE")"

if [[ "$PARALLEL" == "true" && "$ISOLATION" != "per-test" ]]; then
  echo "ERROR: --parallel requires --isolation per-test." >&2
  exit 64
fi

if [[ "$PARALLEL" == "true" && ( -n "$CLI_ADDRESS" || -n "$AGENT_ADDRESS" ) ]]; then
  echo "ERROR: --parallel is only valid when CLI and agent machines are local." >&2
  echo "Unset WENDY_E2E_CLI_ADDRESS and WENDY_E2E_AGENT_ADDRESS, or omit --parallel." >&2
  exit 64
fi

if [[ -n "$CLI_ADDRESS" && -z "$CLI_REPO_DIR" ]]; then
  echo "ERROR: --cli-repo-dir is required when --cli-address is set." >&2
  exit 64
fi

shell_quote() {
  printf "'%s'" "${1//\'/\'\\\'\'}"
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
    '\$HOME')
      printf "%s" "${HOME:?}"
      ;;
    '\$HOME/'*)
      printf "%s/%s" "${HOME:?}" "${path#\$HOME/}"
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

REPO_DIR="$(absolute_existing_dir_path "$SWIFT_DIR/..")"
OUTPUT_DIR="$(absolute_dir_path "$OUTPUT_DIR")"

RUN_NAME="$(sanitize_run_id "$RUN_NAME")"
RUN_ID="$(sanitize_run_id "${RUN_ID:-$(default_run_id "$OUTPUT_DIR" "$RUN_NAME")}")"
if [[ -z "$RUN_ID" ]]; then
  RUN_ID="$(sanitize_run_id "$(default_run_id "$OUTPUT_DIR" "$RUN_NAME")")"
fi
if [[ -z "$CLI_ADDRESS" ]]; then
  CLI_ROOT_DIR="$(absolute_dir_path "$CLI_ROOT_DIR")"
  CLI_REPO_DIR="${CLI_REPO_DIR:-$REPO_DIR}"
  CLI_REPO_DIR="$(absolute_existing_dir_path "$CLI_REPO_DIR")"
fi
if [[ -z "$AGENT_ADDRESS" ]]; then
  AGENT_ROOT_DIR="$(absolute_dir_path "$AGENT_ROOT_DIR")"
  AGENT_REPO_DIR="${AGENT_REPO_DIR:-$REPO_DIR}"
  AGENT_REPO_DIR="$(absolute_existing_dir_path "$AGENT_REPO_DIR")"
fi

if [[ -z "$CLI_BIN_DIR" ]]; then
  CLI_BIN_DIR="${CLI_REPO_DIR%/}/go/bin"
fi
if [[ -z "$CLI_ADDRESS" ]]; then
  CLI_BIN_DIR="$(absolute_dir_path "$CLI_BIN_DIR")"
fi
if [[ -n "$AGENT_BIN_DIR" && -z "$AGENT_ADDRESS" ]]; then
  AGENT_BIN_DIR="$(absolute_dir_path "$AGENT_BIN_DIR")"
fi

RUN_DIR="$OUTPUT_DIR/$RUN_ID"
CLI_RUN_DIR="$CLI_ROOT_DIR/$RUN_ID/cli"
AGENT_RUN_DIR="$AGENT_ROOT_DIR/$RUN_ID/agent"
TESTS_DIR="$RUN_DIR/tests"
TEST_RESULTS_OUTPUT_PATH="$RUN_DIR/test-results.xml"

rm -rf "$RUN_DIR"
if [[ -z "$AGENT_ADDRESS" ]]; then
  rm -rf "$AGENT_RUN_DIR"
fi
mkdir -p \
  "$RUN_DIR" \
  "$TESTS_DIR"

ssh_target() {
  local user="$1"
  local address="$2"
  local host="$address"
  if [[ "$host" == *:* ]]; then
    host="[$host]"
  fi

  if [[ -n "$user" ]]; then
    printf "%s@%s" "$user" "$host"
  else
    printf "%s" "$host"
  fi
}

run_cli_command() {
  local command="$1"

  if [[ -n "$CLI_ADDRESS" ]]; then
    ssh \
      -o BatchMode=yes \
      -o StrictHostKeyChecking=no \
      -o UserKnownHostsFile=/dev/null \
      -o LogLevel=ERROR \
      -T \
      "$(ssh_target "$CLI_USER" "$CLI_ADDRESS")" \
      "bash -lc $(shell_quote "$command")"
  else
    bash -lc "$command"
  fi
}

build_cli() {
  local wendy_path="$CLI_BIN_DIR/wendy"

  echo "==> Building wendy CLI"
  echo "    Target: ${CLI_USER:+$CLI_USER@}${CLI_ADDRESS:-<local>}"
  echo "    Output: $wendy_path"

  local command
  IFS= read -r -d '' command <<EOF || true
set -euo pipefail

expand_target_path() {
  local path="\$1"
  case "\$path" in
    '~')
      printf "%s" "\${HOME:?}"
      ;;
    '~/'*)
      printf "%s/%s" "\${HOME:?}" "\${path#~/}"
      ;;
    '\$HOME')
      printf "%s" "\${HOME:?}"
      ;;
    '\$HOME/'*)
      printf "%s/%s" "\${HOME:?}" "\${path#\$HOME/}"
      ;;
    *)
      printf "%s" "\$path"
      ;;
  esac
}

cli_repo_dir="\$(expand_target_path $(shell_quote "$CLI_REPO_DIR"))"
cli_bin_dir="\$(expand_target_path $(shell_quote "$CLI_BIN_DIR"))"
wendy_path="\$cli_bin_dir/wendy"

mkdir -p "\$cli_bin_dir"
cd "\$cli_repo_dir/go"
go build -o "\$wendy_path" ./cmd/wendy

resolved="\$(PATH="\$cli_bin_dir:\$PATH" command -v wendy || true)"
if [[ "\$resolved" != "\$wendy_path" ]]; then
  echo "ERROR: managed wendy CLI was not first on PATH." >&2
  echo "Expected: \$wendy_path" >&2
  echo "Resolved: \${resolved:-<not found>}" >&2
  exit 1
fi

"\$wendy_path" --version
EOF

  local version
  version="$(run_cli_command "$command")"
  WENDY_CLI_VERSION="$version"
  echo "    Version: $version"
}

json_escape() {
  local value="${1:-}"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  value="${value//$'\r'/\\r}"
  value="${value//$'\t'/\\t}"
  printf "%s" "$value"
}

json_string() {
  printf '"%s"' "$(json_escape "${1:-}")"
}

json_string_or_null() {
  if [[ -n "${1:-}" ]]; then
    json_string "$1"
  else
    printf "null"
  fi
}

json_bool() {
  if [[ "${1:-}" == "true" ]]; then
    printf "true"
  else
    printf "false"
  fi
}

json_string_array() {
  local first="true"
  printf "["
  for value in "$@"; do
    if [[ "$first" == "true" ]]; then
      first="false"
    else
      printf ","
    fi
    json_string "$value"
  done
  printf "]"
}

normalize_xunit_output() {
  local expanded_path="${TEST_RESULTS_OUTPUT_PATH%.xml}-swift-testing.xml"

  if [[ -f "$expanded_path" ]]; then
    mv -f "$expanded_path" "$TEST_RESULTS_OUTPUT_PATH"
  fi
}

write_run_info() {
  local status="$1"
  local info_path="$RUN_DIR/info.json"

  mkdir -p "$RUN_DIR"

  local created_at git_commit git_branch git_ref git_remote git_dirty github_sha swift_version go_version
  created_at="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
  git_commit="$(git -C "$REPO_DIR" rev-parse HEAD 2>/dev/null || true)"
  git_branch="$(git -C "$REPO_DIR" branch --show-current 2>/dev/null || true)"
  git_branch="${git_branch:-${GITHUB_REF_NAME:-}}"
  git_ref="${GITHUB_REF:-}"
  git_remote="$(git -C "$REPO_DIR" remote get-url origin 2>/dev/null || true)"
  git_dirty="false"
  if [[ -n "$(git -C "$REPO_DIR" status --porcelain 2>/dev/null || true)" ]]; then
    git_dirty="true"
  fi
  github_sha="${GITHUB_SHA:-}"
  swift_version="$(swift --version 2>/dev/null | head -n 1 || true)"
  go_version="$(go version 2>/dev/null || true)"

  {
    echo "{"
    printf '  "runID": '; json_string "$RUN_ID"; echo ","
    printf '  "createdAt": '; json_string "$created_at"; echo ","
    printf '  "exitStatus": %s,\n' "$status"
    echo '  "git": {'
    printf '    "commit": '; json_string_or_null "$git_commit"; echo ","
    printf '    "branch": '; json_string_or_null "$git_branch"; echo ","
    printf '    "ref": '; json_string_or_null "$git_ref"; echo ","
    printf '    "remote": '; json_string_or_null "$git_remote"; echo ","
    printf '    "dirty": '; json_bool "$git_dirty"; echo
    echo '  },'
    echo '  "github": {'
    printf '    "repository": '; json_string_or_null "${GITHUB_REPOSITORY:-}"; echo ","
    printf '    "workflow": '; json_string_or_null "${GITHUB_WORKFLOW:-}"; echo ","
    printf '    "runID": '; json_string_or_null "${GITHUB_RUN_ID:-}"; echo ","
    printf '    "runAttempt": '; json_string_or_null "${GITHUB_RUN_ATTEMPT:-}"; echo ","
    printf '    "job": '; json_string_or_null "${GITHUB_JOB:-}"; echo ","
    printf '    "actor": '; json_string_or_null "${GITHUB_ACTOR:-}"; echo ","
    printf '    "sha": '; json_string_or_null "$github_sha"; echo
    echo '  },'
    echo '  "target": {'
    printf '    "cliOS": '; json_string_or_null "$CLI_OS"; echo ","
    printf '    "cliAddress": '; json_string_or_null "$CLI_ADDRESS"; echo ","
    printf '    "cliUser": '; json_string_or_null "$CLI_USER"; echo ","
    printf '    "agentOS": '; json_string_or_null "$AGENT_OS"; echo ","
    printf '    "agentAddress": '; json_string_or_null "$AGENT_ADDRESS"; echo ","
    printf '    "agentUser": '; json_string_or_null "$AGENT_USER"; echo ","
    printf '    "transport": '; json_string_or_null "$TRANSPORT"; echo
    echo '  },'
    echo '  "paths": {'
    printf '    "runDirectory": '; json_string "$RUN_DIR"; echo ","
    printf '    "outputDirectory": '; json_string "$OUTPUT_DIR"; echo ","
    printf '    "cliRunDirectory": '; json_string "$CLI_RUN_DIR"; echo ","
    printf '    "cliBinDirectory": '; json_string "$CLI_BIN_DIR"; echo ","
    printf '    "agentRunDirectory": '; json_string "$AGENT_RUN_DIR"; echo ","
    printf '    "agentBinDirectory": '; json_string_or_null "$AGENT_BIN_DIR"; echo ","
    printf '    "testsDirectory": '; json_string "$TESTS_DIR"; echo
    echo '  },'
    echo '  "test": {'
    printf '    "filters": '; json_string_array "${TEST_FILTERS[@]}"; echo ","
    printf '    "isolation": '; json_string "$ISOLATION"; echo ","
    printf '    "parallel": '; json_bool "$PARALLEL"; echo
    echo '  },'
    echo '  "tools": {'
    printf '    "swift": '; json_string_or_null "$swift_version"; echo ","
    printf '    "go": '; json_string_or_null "$go_version"; echo ","
    printf '    "wendy": '; json_string_or_null "${WENDY_CLI_VERSION:-}"; echo ","
    printf '    "wendyPath": '; json_string "$CLI_BIN_DIR/wendy"; echo
    echo '  }'
    echo "}"
  } > "$info_path"

  echo "==> Wrote Swift E2E run info: $info_path"
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
  "WENDY_E2E_CLI_RUN_DIR=$CLI_RUN_DIR"
  "WENDY_E2E_CLI_REPO_DIR=$CLI_REPO_DIR"
  "WENDY_E2E_CLI_BIN_DIR=$CLI_BIN_DIR"
  "WENDY_E2E_CLI_USER=$CLI_USER"
  "WENDY_E2E_CLI_ADDRESS=$CLI_ADDRESS"
  "WENDY_E2E_AGENT_RUN_DIR=$AGENT_RUN_DIR"
  "WENDY_E2E_AGENT_REPO_DIR=$AGENT_REPO_DIR"
  "WENDY_E2E_AGENT_BIN_DIR=$AGENT_BIN_DIR"
  "WENDY_E2E_AGENT_USER=$AGENT_USER"
  "WENDY_E2E_AGENT_ADDRESS=$AGENT_ADDRESS"
  "WENDY_E2E_CLI_OS=$CLI_OS"
  "WENDY_E2E_AGENT_OS=$AGENT_OS"
  "WENDY_E2E_ISOLATION=$ISOLATION"
  "WENDY_E2E_PARALLEL=$PARALLEL"
  "WENDY_E2E_VERBOSE=$VERBOSE"
)
echo "==> Running Swift E2E tests"
echo "    Package:  $PACKAGE_DIR"
echo "    Run ID:   $RUN_ID"
echo "    Run dir:  $RUN_DIR"
echo "    CLI run:  $CLI_RUN_DIR"
echo "    Agent run: $AGENT_RUN_DIR"
echo "    CLI bin:  $CLI_BIN_DIR"
echo "    CLI:      $CLI_BIN_DIR/wendy"
echo "    Tests:    $TESTS_DIR"
echo "    Filters:  ${TEST_FILTERS[*]}"
echo "    Isolation: $ISOLATION"
echo "    Verbose:  $VERBOSE"
echo "    Parallel: $PARALLEL"
echo "    HTML:     <deferred to Scripts/E2EReport.sh>"
echo "    CLI target: ${CLI_USER:+$CLI_USER@}${CLI_ADDRESS:-<local>}:${CLI_REPO_DIR:-<no-repo>}"
echo "    CLI OS:   ${CLI_OS:-<current>}"
if [[ -n "$AGENT_ADDRESS" ]]; then
  echo "    Agent:   $(ssh_target "$AGENT_USER" "$AGENT_ADDRESS"):${AGENT_REPO_DIR:-<no-repo>}"
else
  echo "    Agent:   <local>:${AGENT_REPO_DIR:-<no-repo>}"
fi
echo "    Agent OS: ${AGENT_OS:-<current>}"
echo "    Transport: ${TRANSPORT:-<none>}"

set +e
(
  cd "$PACKAGE_DIR"
  env "${SWIFT_TEST_ENV[@]}" \
    swift "${SWIFT_TEST_ARGS[@]}" \
    --xunit-output "$TEST_RESULTS_OUTPUT_PATH"
)
TEST_STATUS=$?
set -e

normalize_xunit_output
bash "$SCRIPT_DIR/E2ESanitizeXUnit.sh" --run-dir "$RUN_DIR"

write_run_info "$TEST_STATUS"
exit "$TEST_STATUS"
