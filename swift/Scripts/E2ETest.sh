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
    evaluation_id="local0000"
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
CLI_AUTH_CONFIG_EXPLICIT="${WENDY_E2E_CLI_AUTH_CONFIG_PATH:-}"
CLI_AUTH_CONFIG_PATH="$CLI_AUTH_CONFIG_EXPLICIT"
CLI_USER="${WENDY_E2E_CLI_USER:-}"
CLI_ADDRESS="${WENDY_E2E_CLI_ADDRESS:-}"
CLI_OS="${WENDY_E2E_CLI_OS:-}"
AGENT_ROOT_DIR="${WENDY_E2E_AGENT_ROOT_DIR:-}"
AGENT_REPO_DIR="${WENDY_E2E_AGENT_REPO_DIR:-}"
AGENT_BIN_DIR="${WENDY_E2E_AGENT_BIN_DIR:-}"
AGENT_USER="${WENDY_E2E_AGENT_USER:-}"
AGENT_ADDRESS="${WENDY_E2E_AGENT_ADDRESS:-}"
DEVICE_ADDRESS="${WENDY_E2E_DEVICE_ADDRESS:-}"
AGENT_OS="${WENDY_E2E_AGENT_OS:-}"
TRANSPORT="${WENDY_E2E_TRANSPORT:-}"
MANAGED_AGENT="${WENDY_E2E_MANAGED_AGENT:-false}"
AGENT_INFO_JSON=""
ISOLATION="${WENDY_E2E_ISOLATION:-per-test}"
VERBOSE="${WENDY_E2E_VERBOSE:-false}"
PARALLEL="${WENDY_E2E_PARALLEL:-false}"
TEST_TIMEOUT_SECONDS="${WENDY_E2E_TEST_TIMEOUT_SECONDS:-5400}"
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

normalize_timeout_seconds() {
  local name="$1"
  local value="$2"
  if [[ ! "$value" =~ ^(0|[1-9][0-9]*)$ ]]; then
    echo "ERROR: $name must be a non-negative integer number of seconds." >&2
    exit 64
  fi
  if (( 10#$value > 86400 )); then
    echo "ERROR: $name must be 86400 seconds or less." >&2
    exit 64
  fi
  printf "%s" "$((10#$value))"
}

normalized_agent_os() {
  local value="${AGENT_OS:-}"
  if [[ -z "$value" ]]; then
    value="$(uname -s)"
  fi
  printf '%s' "$value" | tr '[:upper:]' '[:lower:]'
}

managed_agent_is_macos() {
  case "$(normalized_agent_os)" in
    macos|darwin)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

validate_port() {
  local port="$1"
  [[ "$port" =~ ^[0-9]{1,5}$ ]] || return 1
  (( 10#$port >= 1 && 10#$port <= 65535 ))
}

valid_device_address() {
  local value="$1" port=""
  [[ "$value" != *@* ]] || return 1
  if [[ "$value" =~ ^[A-Za-z0-9._-]{1,253}(:([0-9]{1,5}))?$ ]]; then
    port="${BASH_REMATCH[2]:-}"
  elif [[ "$value" =~ ^\[[0-9A-Fa-f:]+\](:([0-9]{1,5}))?$ ]]; then
    port="${BASH_REMATCH[2]:-}"
  else
    return 1
  fi
  [[ -z "$port" ]] || validate_port "$port"
}

validate_test_filter() {
  local filter="$1"
  [[ -n "$filter" && ${#filter} -le 200 ]] || return 1
  [[ "$filter" != -* ]] || return 1
  [[ "$filter" =~ ^[A-Za-z0-9][A-Za-z0-9\ \._:/\|\(\),_-]*$ ]]
}

usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Run the WendyAgent Swift E2E tests and write generated files to an E2E attempt directory.

Options:
  --filter FILTER       Pass a SwiftPM test filter (can be repeated). If omitted,
                        WENDY_E2E_TEST_FILTERS may contain comma-separated
                        filters, otherwise the WendyE2ETests target is run.
  --run-id ID           Run identifier used for default paths.
  --output-dir DIR      Required local root directory for runner output runs.
  --cli-root-dir DIR    Root directory for CLI machine runs.
  --cli-repo-dir DIR    wendy-agent repo root on the CLI machine.
  --cli-bin-dir DIR     Directory where the built wendy CLI is written.
  --cli-auth-config PATH
                        CLI auth fixture config copied into authenticated tests.
  --cli-user USER       Optional SSH user for the CLI machine.
  --cli-address HOST    Optional address for the CLI machine.
  --cli-os OS           Optional OS override for the CLI machine.
  --agent-root-dir DIR  Root directory for agent machine runs.
  --agent-repo-dir DIR  wendy-agent repo root on the agent machine.
  --agent-bin-dir DIR   Optional directory prepended to PATH on the agent machine.
  --agent-user USER     Optional SSH user for the agent machine.
  --agent-address HOST  Optional SSH address for the agent machine; defaults to local.
  --device-address ADDRESS
                        Optional Wendy device address while commands run locally.
  --agent-os OS         Optional OS override for the agent machine.
  --managed-agent       Build and launch a local wendy-agent for this run.
  --no-managed-agent    Do not launch a managed local wendy-agent.
  --isolation MODE      Sandbox isolation: none, per-run, or per-test; defaults to per-test.
  --parallel            Allow SwiftPM to run tests in parallel. Only valid when
                        both CLI and agent machines use local transport.
  --no-parallel         Do not run SwiftPM tests in parallel.
  --test-timeout SECONDS
                        Kill the SwiftPM test process after this many seconds;
                        defaults to WENDY_E2E_TEST_TIMEOUT_SECONDS or 5400.
  --verbose             Print each E2E machine command before it runs.
  --no-verbose          Do not print each E2E machine command before it runs.
  --help                Show this help message.

Environment:
  WENDY_E2E_TEST_FILTERS              Comma-separated SwiftPM filters.
  WENDY_E2E_RUN_ID                    Optional attempt identifier for default paths.
  WENDY_E2E_RUN_NAME                  Attempt name for generated attempt IDs; defaults to local.
  WENDY_E2E_OUTPUT_DIR                Required local root directory for attempt output.
  WENDY_E2E_CLI_ROOT_DIR              Root directory for CLI machine runs.
  WENDY_E2E_CLI_REPO_DIR              wendy-agent repo root on the CLI machine.
  WENDY_E2E_CLI_BIN_DIR               Directory where the built wendy CLI is written.
  WENDY_E2E_CLI_AUTH_CONFIG_PATH      CLI auth fixture config copied into authenticated tests.
  WENDY_E2E_CLI_USER                  Optional SSH user for the CLI machine.
  WENDY_E2E_CLI_ADDRESS               Optional address for the CLI machine.
  WENDY_E2E_CLI_OS                    Optional OS override for the CLI machine.
  WENDY_E2E_AGENT_ROOT_DIR            Root directory for agent machine runs.
  WENDY_E2E_AGENT_REPO_DIR            wendy-agent repo root on the agent machine.
  WENDY_E2E_AGENT_BIN_DIR             Optional directory prepended to PATH on the agent machine.
  WENDY_E2E_AGENT_USER                Optional SSH user for the agent machine.
  WENDY_E2E_AGENT_ADDRESS             Optional SSH address for the agent machine.
  WENDY_E2E_DEVICE_ADDRESS           Optional Wendy device address while commands run locally.
  WENDY_E2E_AGENT_OS                  Optional OS override for the agent machine.
  WENDY_E2E_MANAGED_AGENT             Boolean; build and launch a local wendy-agent.
  WENDY_E2E_TRANSPORT                 Optional transport label for report metadata.
  WENDY_E2E_ISOLATION                 none, per-run, or per-test; defaults to per-test.
  WENDY_E2E_PARALLEL                  Boolean; enables SwiftPM parallel tests.
  WENDY_E2E_TEST_TIMEOUT_SECONDS      SwiftPM test process timeout in seconds; 0 disables.
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
    --cli-auth-config)
      CLI_AUTH_CONFIG_PATH="$2"
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
    --device-address)
      DEVICE_ADDRESS="$2"
      shift 2
      ;;
    --agent-os)
      AGENT_OS="$2"
      shift 2
      ;;
    --managed-agent)
      MANAGED_AGENT="true"
      shift
      ;;
    --no-managed-agent)
      MANAGED_AGENT="false"
      shift
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
    --test-timeout)
      if [[ $# -lt 2 || -z "$2" ]]; then
        echo "ERROR: --test-timeout requires a numeric argument." >&2
        exit 64
      fi
      TEST_TIMEOUT_SECONDS="$2"
      shift 2
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
    filter="${filter#${filter%%[![:space:]]*}}"
    filter="${filter%${filter##*[![:space:]]}}"
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
TEST_TIMEOUT_SECONDS="$(normalize_timeout_seconds "WENDY_E2E_TEST_TIMEOUT_SECONDS" "$TEST_TIMEOUT_SECONDS")"
VERBOSE="$(normalize_bool "WENDY_E2E_VERBOSE" "$VERBOSE")"
MANAGED_AGENT="$(normalize_bool "WENDY_E2E_MANAGED_AGENT" "$MANAGED_AGENT")"

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

if [[ "$MANAGED_AGENT" == "true" && -n "$AGENT_ADDRESS" ]]; then
  echo "ERROR: --managed-agent cannot be combined with --agent-address." >&2
  echo "Use --device-address for the local device address instead." >&2
  exit 64
fi
if [[ "$MANAGED_AGENT" == "true" ]] && managed_agent_is_macos && [[ "$(uname -s)" != "Darwin" ]]; then
  echo "ERROR: macOS managed agents can only run on macOS runners." >&2
  exit 64
fi

if [[ -n "$DEVICE_ADDRESS" ]] && ! valid_device_address "$DEVICE_ADDRESS"; then
  echo "ERROR: invalid --device-address." >&2
  exit 64
fi
for filter in "${TEST_FILTERS[@]}"; do
  if ! validate_test_filter "$filter"; then
    echo "ERROR: invalid SwiftPM test filter: $filter" >&2
    exit 64
  fi
done
if [[ -n "$TRANSPORT" && ! "$TRANSPORT" =~ ^[A-Za-z0-9._-]{1,40}$ ]]; then
  echo "ERROR: WENDY_E2E_TRANSPORT contains invalid characters." >&2
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
if [[ "$MANAGED_AGENT" == "true" && -z "$AGENT_BIN_DIR" ]]; then
  AGENT_BIN_DIR="${AGENT_REPO_DIR%/}/go/bin"
fi
if [[ -n "$AGENT_BIN_DIR" && -z "$AGENT_ADDRESS" ]]; then
  AGENT_BIN_DIR="$(absolute_dir_path "$AGENT_BIN_DIR")"
fi

RUN_DIR="$OUTPUT_DIR/$RUN_ID"
CLI_RUN_DIR="$CLI_ROOT_DIR/$RUN_ID/cli"
AGENT_RUN_DIR="$AGENT_ROOT_DIR/$RUN_ID/agent"
TEST_RESULTS_OUTPUT_PATH="$RUN_DIR/test-results.xml"
ATTEMPT_LOG_PATH="$RUN_DIR/attempt.log"
ATTEMPT_INFO_WRITTEN="false"
MANAGED_AGENT_PID=""

rm -rf "$RUN_DIR"
if [[ -z "$AGENT_ADDRESS" ]]; then
  rm -rf "$AGENT_RUN_DIR"
fi
(umask 077; mkdir -p "$RUN_DIR")

if [[ "$MANAGED_AGENT" == "true" && -z "$DEVICE_ADDRESS" ]]; then
  DEVICE_ADDRESS="127.0.0.1:${WENDY_AGENT_PORT:-50051}"
fi
if [[ -n "$DEVICE_ADDRESS" ]] && ! valid_device_address "$DEVICE_ADDRESS"; then
  echo "ERROR: invalid --device-address." >&2
  exit 64
fi

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

resolve_cli_auth_config_path() {
  if [[ -n "$CLI_AUTH_CONFIG_PATH" ]]; then
    printf "%s" "$CLI_AUTH_CONFIG_PATH"
    return
  fi

  if [[ -n "$CLI_ADDRESS" ]]; then
    run_cli_command 'printf "%s/.wendy/config.json" "$HOME"'
  else
    printf "%s/.wendy/config.json" "${HOME:?}"
  fi
}

preflight_cli_auth_fixture() {
  if [[ -z "$AGENT_ADDRESS" ]]; then
    return
  fi

  echo "==> Checking CLI auth fixture"

  local command
  IFS= read -r -d '' command <<EOF || true
set -euo pipefail

auth_config_path=$(shell_quote "$CLI_AUTH_CONFIG_PATH")
wendy_path=$(shell_quote "$CLI_BIN_DIR/wendy")
agent_address=$(shell_quote "$AGENT_ADDRESS")
preflight_home="\${TMPDIR:-/tmp}/wendy-e2e-auth-preflight.\$\$"
trap 'rm -rf "\$preflight_home"' EXIT

if [[ ! -f "\$auth_config_path" ]]; then
  echo "ERROR: CLI auth fixture config does not exist: \$auth_config_path" >&2
  echo "Run 'wendy auth login' or set WENDY_E2E_CLI_AUTH_CONFIG_PATH." >&2
  exit 1
fi

mkdir -p "\$preflight_home/.wendy"
cp "\$auth_config_path" "\$preflight_home/.wendy/config.json"
HOME="\$preflight_home" "\$wendy_path" --json --device "\$agent_address" device info
EOF

  local agent_info_json
  if ! agent_info_json="$(run_cli_command "$command")"; then
    echo "ERROR: CLI auth fixture cannot access $AGENT_ADDRESS." >&2
    echo "Run 'wendy auth login' with an account that can access the provisioned device, or set WENDY_E2E_CLI_AUTH_CONFIG_PATH." >&2
    exit 1
  fi
  AGENT_INFO_JSON="$agent_info_json"
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
# The Linux CLI links libusb via cgo (Thor flashing); fail with a clear message
# instead of an opaque cgo error when the CLI machine lacks the dev headers.
if [[ "\$(uname -s)" == "Linux" ]] && ! pkg-config --exists libusb-1.0 2>/dev/null; then
  echo "ERROR: building the wendy CLI on Linux needs libusb dev headers" >&2
  echo "  (apt install libusb-1.0-0-dev / dnf install libusb1-devel)" >&2
  exit 1
fi
make mcp-app
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

managed_agent_path() {
  if managed_agent_is_macos; then
    printf '%s/swift/Build/WendyAgentMac.app' "$REPO_DIR"
  else
    printf '%s/wendy-agent' "$AGENT_BIN_DIR"
  fi
}

managed_agent_executable_path() {
  if managed_agent_is_macos; then
    printf '%s/Contents/MacOS/WendyAgentMac' "$(managed_agent_path)"
  else
    managed_agent_path
  fi
}

has_mac_development_signing_identity() {
  local security_command=(security find-identity -v -p codesigning)
  if [[ -n "${KEYCHAIN_PATH:-}" ]]; then
    case "$KEYCHAIN_PATH" in
      *..*|*$'\n'*|*$'\r'*)
        echo "ERROR: KEYCHAIN_PATH contains invalid path components." >&2
        return 1
        ;;
    esac
    if [[ "$KEYCHAIN_PATH" != *.keychain && "$KEYCHAIN_PATH" != *.keychain-db ]]; then
      echo "ERROR: KEYCHAIN_PATH must end in .keychain or .keychain-db." >&2
      return 1
    fi
    security_command+=("$KEYCHAIN_PATH")
  fi

  if [[ -n "${SIGNING_IDENTITY:-}" ]]; then
    # This value is only used with grep -F -- below; keep that fixed-string usage.
    local identity_pattern='^[A-Za-z0-9][A-Za-z0-9 .,:_@()+&-]*$'
    if ! [[ "$SIGNING_IDENTITY" =~ $identity_pattern ]]; then
      echo "ERROR: SIGNING_IDENTITY contains invalid characters." >&2
      return 1
    fi
    "${security_command[@]}" 2>/dev/null | grep -qF -- "$SIGNING_IDENTITY"
  else
    "${security_command[@]}" 2>/dev/null | grep -qF -- 'Apple Development'
  fi
}

build_unsigned_managed_mac_app() {
  local agent_path
  agent_path="$(managed_agent_path)"

  echo "==> No Apple Development signing identity; building unsigned WendyAgentMac Debug app"
  if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
    echo "::warning::No Apple Development signing identity available; Swift macOS E2Es will use an unsigned Debug WendyAgentMac.app."
  fi
  xcodebuild build \
    -workspace WendyAgent.xcworkspace \
    -scheme WendyAgentMac \
    -configuration Debug \
    -destination 'platform=macOS' \
    -derivedDataPath "$REPO_DIR/swift/Build/Xcode" \
    WENDY_AGENT_VERSION="${WENDY_AGENT_VERSION:-0000.00.00-000000-dev}" \
    CODE_SIGNING_ALLOWED=NO \
    CODE_SIGNING_REQUIRED=NO \
    -skipMacroValidation
  rm -rf "$agent_path"
  ditto \
    "$REPO_DIR/swift/Build/Xcode/Build/Products/Debug/WendyAgentMac.app" \
    "$agent_path"
  [[ -x "$agent_path/Contents/MacOS/WendyAgentMac" ]] || {
    echo "ERROR: built WendyAgentMac executable is missing." >&2
    exit 1
  }
  mkdir -p "$RUN_DIR/managed-agent"
  shasum -a 256 "$agent_path/Contents/MacOS/WendyAgentMac" \
    | tee "$RUN_DIR/managed-agent/WendyAgentMac.sha256"
}

managed_mac_app_is_running() {
  [[ "$(osascript -e 'application id "sh.wendy.WendyAgentMac" is running' 2>/dev/null || true)" == "true" ]]
}

build_managed_agent() {
  local agent_path
  agent_path="$(managed_agent_path)"

  echo "==> Building managed wendy-agent"
  echo "    Agent OS: $(normalized_agent_os)"
  echo "    Output: $agent_path"

  if managed_agent_is_macos; then
    (
      cd "$REPO_DIR/swift"
      ./Scripts/Quit.sh
      if has_mac_development_signing_identity; then
        OUTPUT_DIR="$REPO_DIR/swift/Build" bash ./Scripts/Build.sh --dev
      else
        build_unsigned_managed_mac_app
      fi
    )
    /usr/libexec/PlistBuddy -c 'Print :WLWendyAgentVersion' \
      "$agent_path/Contents/Info.plist" 2>/dev/null || true
  else
    mkdir -p "$AGENT_BIN_DIR"
    (
      cd "$REPO_DIR/go"
      go build -o "$agent_path" ./cmd/wendy-agent
    )
    "$agent_path" --version
  fi
}

start_managed_agent() {
  local agent_path
  agent_path="$(managed_agent_executable_path)"
  local managed_dir="$RUN_DIR/managed-agent"
  local config_dir="$managed_dir/config"
  local stdout_path="$managed_dir/stdout.log"
  local stderr_path="$managed_dir/stderr.log"
  local pid_path="$managed_dir/pid"
  local port="50051"

  if [[ "$DEVICE_ADDRESS" =~ ^[A-Za-z0-9._-]{1,253}:([0-9]{1,5})$ ]]; then
    port="${BASH_REMATCH[1]}"
  elif [[ "$DEVICE_ADDRESS" =~ ^\[[0-9A-Fa-f:]+\]:([0-9]{1,5})$ ]]; then
    port="${BASH_REMATCH[1]}"
  fi
  if ! validate_port "$port"; then
    echo "ERROR: invalid managed agent port in --device-address: $port" >&2
    return 64
  fi

  echo "==> Starting managed wendy-agent"
  echo "    Address: ${DEVICE_ADDRESS##*@}"
  echo "    Logs:    managed-agent/stdout.log, managed-agent/stderr.log"

  (umask 077; mkdir -p "$managed_dir")
  if managed_agent_is_macos; then
    "$REPO_DIR/swift/Scripts/Quit.sh" || true
    (umask 077; mkdir -p "$config_dir/home")
    (umask 077; : >"$stdout_path"; : >"$stderr_path")
    env -i \
      HOME="$config_dir/home" \
      LOGNAME="wendy-e2e-agent" \
      PATH="/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
      TMPDIR="${TMPDIR:-/tmp}" \
      USER="wendy-e2e-agent" \
      open \
        -n \
        -g \
        --stdout "$stdout_path" \
        --stderr "$stderr_path" \
        --env "HOME=$config_dir/home" \
        --env "LOGNAME=wendy-e2e-agent" \
        --env "PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
        --env "TMPDIR=${TMPDIR:-/tmp}" \
        --env "USER=wendy-e2e-agent" \
        --env "WENDY_AGENT_HOST=127.0.0.1" \
        --env "WENDY_AGENT_PORT=$port" \
        --env "WENDY_OTEL_PORT=0" \
        "$(managed_agent_path)"
    sleep 1
  else
    mkdir -p "$config_dir/home" "$config_dir/state" "$config_dir/xdg-config" "$config_dir/xdg-data"
    chmod 700 "$managed_dir" "$config_dir" "$config_dir/home" "$config_dir/state" "$config_dir/xdg-config" "$config_dir/xdg-data"
    (umask 077; : > "$pid_path")
    env -i \
      HOME="$config_dir/home" \
      LOGNAME="wendy-e2e-agent" \
      PATH="/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" \
      TMPDIR="${TMPDIR:-/tmp}" \
      USER="wendy-e2e-agent" \
      WENDY_CONFIG_PATH="$config_dir" \
      XDG_CONFIG_HOME="$config_dir/xdg-config" \
      XDG_DATA_HOME="$config_dir/xdg-data" \
      WENDY_AGENT_PORT="$port" \
      WENDY_OTEL_PORT=0 \
      WENDY_OTEL_HTTP_PORT=0 \
      WENDY_REGISTRY_ADDR=127.0.0.1:0 \
      "$agent_path" >"$stdout_path" 2>"$stderr_path" &
    MANAGED_AGENT_PID=$!
    (umask 077; printf '%s\n' "$MANAGED_AGENT_PID" > "$pid_path")
  fi

  if ! valid_device_address "$DEVICE_ADDRESS"; then
    echo "ERROR: invalid --device-address." >&2
    return 64
  fi

  local attempt max_attempts=30
  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    if managed_agent_is_macos; then
      if ! managed_mac_app_is_running; then
        echo "ERROR: WendyAgentMac exited before becoming ready; see managed-agent/stderr.log in the E2E artifact." >&2
        tail -20 "$stderr_path" >&2 || true
        return 1
      fi
    elif ! kill -0 "$MANAGED_AGENT_PID" 2>/dev/null; then
      echo "ERROR: managed wendy-agent exited before becoming ready; see managed-agent/stderr.log in the E2E artifact." >&2
      return 1
    fi
    if "$CLI_BIN_DIR/wendy" --json --device "$DEVICE_ADDRESS" device info >/dev/null 2>&1; then
      echo "    Ready"
      return 0
    fi
    sleep 1
  done

  echo "ERROR: managed wendy-agent did not become ready at $DEVICE_ADDRESS; see managed-agent/stderr.log in the E2E artifact." >&2
  return 1
}

stop_managed_agent() {
  if managed_agent_is_macos; then
    local quit_log="$RUN_DIR/managed-agent/quit.log"
    if ! "$REPO_DIR/swift/Scripts/Quit.sh" >"$quit_log" 2>&1; then
      echo "WARN: WendyAgentMac quit script reported an error; see managed-agent/quit.log" >&2
    fi
    MANAGED_AGENT_PID=""
    return
  fi
  if [[ -z "${MANAGED_AGENT_PID:-}" ]]; then
    return
  fi
  if kill -0 "$MANAGED_AGENT_PID" 2>/dev/null; then
    kill "$MANAGED_AGENT_PID" 2>/dev/null || true
    local deadline=$((SECONDS + 10))
    while (( SECONDS < deadline )); do
      if ! kill -0 "$MANAGED_AGENT_PID" 2>/dev/null; then
        break
      fi
      sleep 1
    done
    if kill -0 "$MANAGED_AGENT_PID" 2>/dev/null; then
      kill -9 "$MANAGED_AGENT_PID" 2>/dev/null || true
    fi
  fi
  wait "$MANAGED_AGENT_PID" 2>/dev/null || true
  MANAGED_AGENT_PID=""
}

prepare_managed_agent_auth_fixture() {
  if [[ "$MANAGED_AGENT" != "true" || -n "$CLI_AUTH_CONFIG_EXPLICIT" ]]; then
    return
  fi

  local auth_dir="$RUN_DIR/managed-agent/cli-auth"
  mkdir -p "$auth_dir"
  CLI_AUTH_CONFIG_PATH="$auth_dir/config.json"
  # Managed E2Es run with throwaway HOME directories. Pre-dismiss only the
  # ambient shell-completion install prompt so PTY-backed command tests do not
  # block after printing their real command output.
  printf '{"completionPromptDismissed":true}\n' > "$CLI_AUTH_CONFIG_PATH"
  chmod 600 "$CLI_AUTH_CONFIG_PATH" 2>/dev/null || true
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

json_raw_or_null() {
  if [[ -n "${1:-}" ]]; then
    printf "%s" "$1"
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

run_swift_test_with_timeout() {
  local timeout_seconds="$1"
  local timeout_path="$2"
  shift 2

  python3 - "$timeout_seconds" "$timeout_path" "$@" <<'PY'
import json
import os
import signal
import subprocess
import sys
import time


def utc_now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def github_annotation_escape(value):
    return value.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")


timeout_seconds = int(sys.argv[1])
timeout_path = sys.argv[2]
command = sys.argv[3:]
started_at = time.time()
process = subprocess.Popen(command, start_new_session=True)
try:
    exit_status = process.wait(timeout=None if timeout_seconds == 0 else timeout_seconds)
except subprocess.TimeoutExpired:
    elapsed = time.time() - started_at
    message = (
        f"Swift E2E test process exceeded {timeout_seconds} seconds and was "
        "terminated before the GitHub Actions job timeout."
    )
    print(f"::error::{github_annotation_escape(message)}", flush=True)
    print(f"==> {message}", flush=True)
    try:
        # start_new_session=True calls setsid(), so the child PID is also its process group ID.
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    killed_with = "SIGTERM"
    try:
        process.wait(timeout=30)
    except subprocess.TimeoutExpired:
        print("==> Swift E2E test process did not exit after SIGTERM; sending SIGKILL", flush=True)
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        killed_with = "SIGKILL"
        try:
            process.wait(timeout=30)
        except subprocess.TimeoutExpired:
            print("==> WARNING: Swift E2E test process did not exit after SIGKILL", flush=True)

    os.makedirs(os.path.dirname(timeout_path), exist_ok=True)
    with open(timeout_path, "w", encoding="utf-8") as handle:
        json.dump(
            {
                "kind": "wendy-e2e-timeout",
                "phase": "swift test",
                "timeoutSeconds": timeout_seconds,
                "elapsedSeconds": round(elapsed, 3),
                "terminatedAt": utc_now(),
                "signal": killed_with,
                "exitStatus": 124,
                "message": message,
            },
            handle,
            indent=2,
            sort_keys=True,
        )
        handle.write("\n")
    exit_status = 124
sys.exit(exit_status)
PY
}

write_attempt_info() {
  local status="$1"
  local info_path="$RUN_DIR/attempt.json"

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
    printf '  "kind": '; json_string "wendy-e2e-attempt"; echo ","
    printf '  "version": 1,\n'
    printf '  "attemptID": '; json_string "$RUN_ID"; echo ","
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
    printf '    "transport": '; json_string_or_null "$TRANSPORT"; echo ","
    printf '    "agentInfo": '; json_raw_or_null "$AGENT_INFO_JSON"; echo
    echo '  },'
    echo '  "paths": {'
    printf '    "attemptDirectory": '; json_string "$RUN_DIR"; echo ","
    printf '    "outputDirectory": '; json_string "$OUTPUT_DIR"; echo ","
    printf '    "cliRunDirectory": '; json_string "$CLI_RUN_DIR"; echo ","
    printf '    "cliBinDirectory": '; json_string "$CLI_BIN_DIR"; echo ","
    printf '    "agentRunDirectory": '; json_string "$AGENT_RUN_DIR"; echo ","
    printf '    "agentBinDirectory": '; json_string_or_null "$AGENT_BIN_DIR"; echo
    echo '  },'
    echo '  "test": {'
    printf '    "filters": '; json_string_array "${TEST_FILTERS[@]}"; echo ","
    printf '    "isolation": '; json_string "$ISOLATION"; echo ","
    printf '    "parallel": '; json_bool "$PARALLEL"; echo ","
    printf '    "managedAgent": '; json_bool "$MANAGED_AGENT"; echo
    echo '  },'
    echo '  "tools": {'
    printf '    "swift": '; json_string_or_null "$swift_version"; echo ","
    printf '    "go": '; json_string_or_null "$go_version"; echo ","
    printf '    "wendy": '; json_string_or_null "${WENDY_CLI_VERSION:-}"; echo ","
    printf '    "wendyPath": '; json_string "$CLI_BIN_DIR/wendy"; echo
    if [[ -f "$RUN_DIR/timeout.json" ]]; then
      echo '  },'
      echo '  "failure": {'
      printf '    "kind": '; json_string "timeout"; echo ","
      printf '    "phase": '; json_string "swift test"; echo ","
      printf '    "timeoutSeconds": %s,\n' "$TEST_TIMEOUT_SECONDS"
      printf '    "message": '; json_string "Swift E2E test process exceeded ${TEST_TIMEOUT_SECONDS} seconds and was terminated before the GitHub Actions job timeout."; echo ","
      printf '    "evidence": '; json_string "timeout.json"; echo
      echo '  }'
    else
      echo '  }'
    fi
    echo "}"
  } > "$info_path"
  ATTEMPT_INFO_WRITTEN="true"

  echo "==> Wrote Swift E2E attempt info: $info_path"
}

finalize_attempt() {
  local status=$?
  trap - EXIT
  set +e
  stop_managed_agent
  if [[ "$ATTEMPT_INFO_WRITTEN" != "true" ]]; then
    write_attempt_info "$status"
  fi
  exit "$status"
}

# Capture the full attempt lifecycle in the attempt artifact so aggregate/review
# can diagnose setup, preflight, and test-launch failures that happen before
# Swift Testing writes per-test recordings.
exec > >(tee "$ATTEMPT_LOG_PATH") 2>&1
trap finalize_attempt EXIT

echo "==> Capturing Swift E2E attempt log: $ATTEMPT_LOG_PATH"

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

CLI_AUTH_CONFIG_PATH="$(resolve_cli_auth_config_path)"
if [[ -z "$CLI_ADDRESS" ]]; then
  CLI_AUTH_CONFIG_PATH="$(expand_local_path "$CLI_AUTH_CONFIG_PATH")"
fi
prepare_managed_agent_auth_fixture

build_cli
if [[ "$MANAGED_AGENT" == "true" ]]; then
  build_managed_agent
  start_managed_agent
fi
preflight_cli_auth_fixture

SWIFT_TEST_ENV=(
  "WENDY_E2E_RUN_ID=$RUN_ID"
  "WENDY_E2E_RUN_DIR=$RUN_DIR"
  "WENDY_E2E_CLI_RUN_DIR=$CLI_RUN_DIR"
  "WENDY_E2E_CLI_REPO_DIR=$CLI_REPO_DIR"
  "WENDY_E2E_CLI_BIN_DIR=$CLI_BIN_DIR"
  "WENDY_E2E_CLI_AUTH_CONFIG_PATH=$CLI_AUTH_CONFIG_PATH"
  "WENDY_E2E_CLI_USER=$CLI_USER"
  "WENDY_E2E_CLI_ADDRESS=$CLI_ADDRESS"
  "WENDY_E2E_AGENT_RUN_DIR=$AGENT_RUN_DIR"
  "WENDY_E2E_AGENT_REPO_DIR=$AGENT_REPO_DIR"
  "WENDY_E2E_AGENT_BIN_DIR=$AGENT_BIN_DIR"
  "WENDY_E2E_AGENT_USER=$AGENT_USER"
  "WENDY_E2E_AGENT_ADDRESS=$AGENT_ADDRESS"
  "WENDY_E2E_DEVICE_ADDRESS=$DEVICE_ADDRESS"
  "WENDY_E2E_CLI_OS=$CLI_OS"
  "WENDY_E2E_AGENT_OS=$AGENT_OS"
  "WENDY_E2E_ISOLATION=$ISOLATION"
  "WENDY_E2E_PARALLEL=$PARALLEL"
  "WENDY_E2E_VERBOSE=$VERBOSE"
)
echo "==> Running Swift E2E tests"
echo "    Package:  $PACKAGE_DIR"
echo "    Attempt ID:  $RUN_ID"
echo "    Attempt dir: $RUN_DIR"
echo "    CLI run:  $CLI_RUN_DIR"
echo "    Agent run: $AGENT_RUN_DIR"
echo "    CLI bin:  $CLI_BIN_DIR"
echo "    CLI:      $CLI_BIN_DIR/wendy"
echo "    Filters:  ${TEST_FILTERS[*]}"
echo "    Isolation: $ISOLATION"
echo "    Verbose:  $VERBOSE"
echo "    Parallel: $PARALLEL"
echo "    Timeout:  ${TEST_TIMEOUT_SECONDS}s"
echo "    HTML:     <deferred to Scripts/E2EReport.sh>"
echo "    CLI target: ${CLI_USER:+$CLI_USER@}${CLI_ADDRESS:-<local>}:${CLI_REPO_DIR:-<no-repo>}"
echo "    CLI OS:   ${CLI_OS:-<current>}"
if [[ -n "$AGENT_ADDRESS" ]]; then
  echo "    Agent:   $(ssh_target "$AGENT_USER" "$AGENT_ADDRESS"):${AGENT_REPO_DIR:-<no-repo>}"
else
  echo "    Agent:   <local>:${AGENT_REPO_DIR:-<no-repo>}"
fi
if [[ -n "$DEVICE_ADDRESS" ]]; then
  echo "    Device address: ${DEVICE_ADDRESS##*@}"
fi
echo "    Agent OS: ${AGENT_OS:-<current>}"
echo "    Transport: ${TRANSPORT:-<none>}"
echo "    Managed agent: $MANAGED_AGENT"

set +e
(
  cd "$PACKAGE_DIR"
  # Export is intentional: run_swift_test_with_timeout launches python3, which
  # inherits this environment before spawning swift test.
  export "${SWIFT_TEST_ENV[@]}"
  run_swift_test_with_timeout \
    "$TEST_TIMEOUT_SECONDS" \
    "$RUN_DIR/timeout.json" \
    swift "${SWIFT_TEST_ARGS[@]}" \
    --xunit-output "$TEST_RESULTS_OUTPUT_PATH"
)
TEST_STATUS=$?
set -e

normalize_xunit_output
bash "$SCRIPT_DIR/E2ESanitizeXUnit.sh" --run-dir "$RUN_DIR"

write_attempt_info "$TEST_STATUS"
exit "$TEST_STATUS"
