#!/bin/bash
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Defaults
DEFAULT_HOSTNAME="wendyos-jolly-cedar.local"
DEFAULT_USER="edge"
DEFAULT_PASSWORD="edge"

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Integration test script for the Go wendy CLI.
Exercises most CLI commands against a real WendyOS device.

Options:
  -h, --hostname HOST    Device hostname (default: $DEFAULT_HOSTNAME)
  -u, --user USER        SSH username (default: $DEFAULT_USER)
  -p, --password PASS    SSH password (default: $DEFAULT_PASSWORD)
  --skip-deploy          Skip build/run/remove tests (read-only mode)
  --help                 Show this help message

Examples:
  $(basename "$0")
  $(basename "$0") -h wendyos-merry-aurora
  $(basename "$0") -h wendyos-merry-aurora --skip-deploy
EOF
    exit 0
}

HOSTNAME="$DEFAULT_HOSTNAME"
SSH_USER="$DEFAULT_USER"
SSH_PASS="$DEFAULT_PASSWORD"
SKIP_DEPLOY=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--hostname) HOSTNAME="$2"; shift 2 ;;
        -u|--user)     SSH_USER="$2"; shift 2 ;;
        -p|--password) SSH_PASS="$2"; shift 2 ;;
        --skip-deploy) SKIP_DEPLOY=true; shift ;;
        --help)        usage ;;
        *)             echo "Unknown option: $1"; usage ;;
    esac
done

# Add .local suffix if missing.
if [[ "$HOSTNAME" != *.local ]]; then
    HOSTNAME="${HOSTNAME}.local"
fi

# ── Colors & test harness ────────────────────────────────────────────

GREEN="\033[0;32m"
RED="\033[0;31m"
YELLOW="\033[0;33m"
BOLD="\033[1m"
RESET="\033[0m"

PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0

WENDY="$PROJECT_DIR/bin/wendy"

run_test() {
    local name="$1"
    shift
    printf "  %-50s " "$name"
    local output
    output=$("$@" 2>&1)
    local rc=$?
    if [[ $rc -eq 0 ]]; then
        echo -e "${GREEN}PASS${RESET}"
        ((PASS_COUNT++))
    else
        echo -e "${RED}FAIL${RESET} (exit $rc)"
        echo "    Output: $(echo "$output" | head -5)"
        ((FAIL_COUNT++))
    fi
    return $rc
}

run_test_expect_output() {
    local name="$1"
    local pattern="$2"
    shift 2
    printf "  %-50s " "$name"
    local output
    output=$("$@" 2>&1)
    local rc=$?
    if [[ $rc -eq 0 ]] && echo "$output" | grep -qiE "$pattern"; then
        echo -e "${GREEN}PASS${RESET}"
        ((PASS_COUNT++))
    else
        echo -e "${RED}FAIL${RESET} (exit $rc)"
        echo "    Output: $(echo "$output" | head -5)"
        ((FAIL_COUNT++))
    fi
    return 0
}

run_test_json() {
    local name="$1"
    shift
    printf "  %-50s " "$name"
    local output
    output=$("$@" 2>&1)
    local rc=$?
    if [[ $rc -eq 0 ]] && echo "$output" | jq . >/dev/null 2>&1; then
        echo -e "${GREEN}PASS${RESET}"
        ((PASS_COUNT++))
    else
        echo -e "${RED}FAIL${RESET} (exit $rc)"
        echo "    Output: $(echo "$output" | head -5)"
        ((FAIL_COUNT++))
    fi
    return 0
}

skip_test() {
    local name="$1"
    printf "  %-50s " "$name"
    echo -e "${YELLOW}SKIP${RESET}"
    ((SKIP_COUNT++))
}

# ── Build CLI ────────────────────────────────────────────────────────

echo -e "${BOLD}==> Building wendy CLI...${RESET}"
cd "$PROJECT_DIR"
make build-cli
if [[ ! -x "$WENDY" ]]; then
    echo -e "${RED}ERROR: CLI binary not found at $WENDY${RESET}"
    exit 1
fi
echo ""

# ── Phase 1: Local commands ─────────────────────────────────────────

echo -e "${BOLD}Phase 1: Local commands${RESET}"

run_test "wendy info" \
    "$WENDY" info

run_test_json "wendy info --json" \
    "$WENDY" info --json

run_test "wendy cache list" \
    "$WENDY" cache list

run_test "wendy cache clear" \
    "$WENDY" cache clear

# init tests in temp dirs
TMPDIR1=$(mktemp -d)
run_test "wendy init (default)" \
    bash -c "cd '$TMPDIR1' && '$WENDY' init"
if [[ -f "$TMPDIR1/wendy.json" ]]; then
    : # already counted as pass above
else
    echo "    Warning: wendy.json not created in $TMPDIR1"
fi

TMPDIR2=$(mktemp -d)
run_test "wendy init --language python" \
    bash -c "cd '$TMPDIR2' && '$WENDY' init --language python"

rm -rf "$TMPDIR1" "$TMPDIR2"
echo ""

# ── Phase 2: Discovery ──────────────────────────────────────────────

echo -e "${BOLD}Phase 2: Discovery${RESET}"

run_test "wendy discover --timeout 5s" \
    "$WENDY" discover --timeout 5s

run_test_json "wendy discover --timeout 5s --json" \
    "$WENDY" discover --timeout 5s --json

echo ""

# ── Phase 3: Device commands ────────────────────────────────────────

echo -e "${BOLD}Phase 3: Device commands${RESET}"

run_test "wendy device set-default" \
    "$WENDY" device set-default "$HOSTNAME"

run_test "wendy device version" \
    "$WENDY" device version --device "$HOSTNAME"

run_test_json "wendy device version --json" \
    "$WENDY" device version --device "$HOSTNAME" --json

echo ""

# ── Phase 4: Hardware / peripheral queries ──────────────────────────

echo -e "${BOLD}Phase 4: Hardware & peripherals${RESET}"

run_test "wendy hardware list" \
    "$WENDY" hardware list --device "$HOSTNAME"

run_test_json "wendy hardware list --json" \
    "$WENDY" hardware list --device "$HOSTNAME" --json

run_test "wendy audio list" \
    "$WENDY" audio list --device "$HOSTNAME"

run_test "wendy bluetooth list" \
    "$WENDY" bluetooth list --device "$HOSTNAME"

echo ""

# ── Phase 5: App lifecycle ──────────────────────────────────────────

echo -e "${BOLD}Phase 5: App lifecycle${RESET}"

if [[ "$SKIP_DEPLOY" == true ]]; then
    skip_test "wendy build"
    skip_test "wendy run --detach"
    skip_test "wendy device apps list (app present)"
    skip_test "wendy device apps stop"
    skip_test "wendy device apps start"
    skip_test "wendy device apps remove"
    skip_test "wendy device apps list (app gone)"
else
    HELLO_DIR="$PROJECT_DIR/tmp/hello-python"
    if [[ ! -d "$HELLO_DIR" ]]; then
        echo -e "${RED}ERROR: Example project not found at $HELLO_DIR${RESET}"
        skip_test "wendy build"
        skip_test "wendy run --detach"
        skip_test "wendy device apps list (app present)"
        skip_test "wendy device apps stop"
        skip_test "wendy device apps start"
        skip_test "wendy device apps remove"
        skip_test "wendy device apps list (app gone)"
    else
        APP_ID="sh.wendy.examples.hello-python"

        run_test "wendy build" \
            bash -c "cd '$HELLO_DIR' && '$WENDY' build"

        run_test "wendy run --detach" \
            bash -c "cd '$HELLO_DIR' && '$WENDY' run --device '$HOSTNAME' --detach"

        run_test_expect_output "wendy device apps list (app present)" "$APP_ID" \
            "$WENDY" device device apps list --device "$HOSTNAME"

        run_test "wendy device apps stop" \
            "$WENDY" device apps stop "$APP_ID" --device "$HOSTNAME"

        run_test "wendy device apps start" \
            "$WENDY" device apps start "$APP_ID" --device "$HOSTNAME"

        run_test "wendy device apps remove" \
            "$WENDY" device apps remove "$APP_ID" --device "$HOSTNAME" --force

        # Verify the app is gone
        printf "  %-50s " "wendy device apps list (app gone)"
        LIST_OUTPUT=$("$WENDY" device apps list --device "$HOSTNAME" 2>&1)
        if echo "$LIST_OUTPUT" | grep -q "$APP_ID"; then
            echo -e "${RED}FAIL${RESET} (app still present)"
            ((FAIL_COUNT++))
        else
            echo -e "${GREEN}PASS${RESET}"
            ((PASS_COUNT++))
        fi
    fi
fi

echo ""

# ── Phase 6: Telemetry ──────────────────────────────────────────────

echo -e "${BOLD}Phase 6: Telemetry${RESET}"

# Streaming command — run with a short timeout; both success and timeout are OK
printf "  %-50s " "wendy telemetry logs (3s timeout)"
TELEM_OUTPUT=$(timeout 3 "$WENDY" telemetry logs --device "$HOSTNAME" 2>&1)
TELEM_RC=$?
# exit 124 = timeout reached, which is fine for a streaming command
if [[ $TELEM_RC -eq 0 ]] || [[ $TELEM_RC -eq 124 ]]; then
    echo -e "${GREEN}PASS${RESET}"
    ((PASS_COUNT++))
else
    echo -e "${RED}FAIL${RESET} (exit $TELEM_RC)"
    echo "    Output: $(echo "$TELEM_OUTPUT" | head -5)"
    ((FAIL_COUNT++))
fi

echo ""

# ── Phase 7: Cleanup ────────────────────────────────────────────────

echo -e "${BOLD}Phase 7: Cleanup${RESET}"

run_test "wendy device unset-default" \
    "$WENDY" device unset-default

echo ""

# ── Summary ──────────────────────────────────────────────────────────

TOTAL=$((PASS_COUNT + FAIL_COUNT + SKIP_COUNT))
echo -e "${BOLD}========================================${RESET}"
echo -e "${BOLD}Results:${RESET} $TOTAL tests"
echo -e "  ${GREEN}Passed:  $PASS_COUNT${RESET}"
echo -e "  ${RED}Failed:  $FAIL_COUNT${RESET}"
if [[ $SKIP_COUNT -gt 0 ]]; then
    echo -e "  ${YELLOW}Skipped: $SKIP_COUNT${RESET}"
fi
echo -e "${BOLD}========================================${RESET}"

if [[ $FAIL_COUNT -gt 0 ]]; then
    exit 1
fi
exit 0
