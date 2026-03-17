#!/bin/bash
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd "$REPO_DIR/.." && pwd)"
TESTS_DIR="$REPO_ROOT/.github/ci-tests"

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Run CI integration tests against a real WendyOS device. Each test deploys a
minimal app that exercises a specific entitlement and verifies it works.

Tests:
  swift-hello         Basic Swift containerized deployment (no entitlements)
  swift-network       Swift with network entitlement (WiFi connectivity)
  swift-bluetooth     Swift with bluetooth entitlement
  python-hello        Basic Python deployment (no entitlements)
  python-network      Python with network entitlement (WiFi connectivity)
  python-gpu          Python with GPU entitlement (CUDA verification)
  python-bluetooth    Python with bluetooth entitlement

Device Selection:
  If --hostname is not provided, the script auto-discovers a device on the
  local network using 'wendy discover --json'. The first LAN device found
  is used.

Options:
  -h, --hostname HOST   Device hostname (skips auto-discovery)
  -w, --wendy PATH      Path to wendy binary (default: wendy on PATH)
  -t, --test NAME       Run only the named test (can be repeated)
  --help                Show this help message

Examples:
  $(basename "$0")                                  # auto-discover, all tests
  $(basename "$0") -h wendyos-merry-aurora          # explicit host
  $(basename "$0") -t swift-hello -t python-hello   # specific tests only
  $(basename "$0") -w /path/to/wendy                # custom binary
EOF
    exit 0
}

HOSTNAME=""
HOSTNAME_PROVIDED=false
WENDY="wendy"
SELECTED_TESTS=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--hostname)      HOSTNAME="$2"; HOSTNAME_PROVIDED=true; shift 2 ;;
        -w|--wendy)         WENDY="$2"; shift 2 ;;
        -t|--test)          SELECTED_TESTS+=("$2"); shift 2 ;;
        --help)             usage ;;
        *)                  echo "Unknown option: $1"; usage ;;
    esac
done

# Add .local suffix if hostname was explicitly provided and missing it.
if [[ "$HOSTNAME_PROVIDED" == true ]] && [[ "$HOSTNAME" != *.local ]]; then
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
        echo "    Output: $(echo "$output" | tail -10)"
        ((FAIL_COUNT++))
    fi
    return $rc
}

skip_test() {
    local name="$1"
    local reason="${2:-}"
    printf "  %-50s " "$name"
    if [[ -n "$reason" ]]; then
        echo -e "${YELLOW}SKIP${RESET} ($reason)"
    else
        echo -e "${YELLOW}SKIP${RESET}"
    fi
    ((SKIP_COUNT++))
}

# ── Wendy binary validation ─────────────────────────────────────────

if [[ "$WENDY" != "wendy" ]]; then
    if [[ ! -x "$WENDY" ]]; then
        echo -e "${RED}ERROR: wendy binary not found or not executable at $WENDY${RESET}"
        exit 1
    fi
else
    if ! command -v wendy &>/dev/null; then
        echo -e "${RED}ERROR: 'wendy' not found on PATH${RESET}"
        echo "Hint: pass -w /path/to/wendy to specify the binary location."
        exit 1
    fi
    WENDY="$(command -v wendy)"
fi

echo -e "${BOLD}==> Using wendy: ${WENDY}${RESET}"
echo ""

# ── Device discovery ─────────────────────────────────────────────────

if [[ -z "$HOSTNAME" ]]; then
    echo -e "${BOLD}==> Auto-discovering device...${RESET}"
    DISCOVER_JSON=$("$WENDY" discover --json --timeout 5s 2>&1)
    DISCOVERED_HOST=$(echo "$DISCOVER_JSON" | jq -r '.lanDevices[0].hostname // empty' 2>/dev/null)
    if [[ -z "$DISCOVERED_HOST" ]]; then
        echo -e "${RED}ERROR: No LAN device found via 'wendy discover --json --timeout 5s'${RESET}"
        echo "    Output: $(echo "$DISCOVER_JSON" | head -5)"
        echo ""
        echo "Hint: pass -h <hostname> to skip auto-discovery."
        exit 1
    fi
    HOSTNAME="$DISCOVERED_HOST"
fi

echo -e "${BOLD}==> Target device: ${HOSTNAME}${RESET}"
echo ""

# ── Validate tests directory ─────────────────────────────────────────

if [[ ! -d "$TESTS_DIR" ]]; then
    echo -e "${RED}ERROR: CI tests directory not found at $TESTS_DIR${RESET}"
    exit 1
fi

# ── Ordered test list ────────────────────────────────────────────────
# Swift (containerized) first, then Python, basic → entitlements.

ALL_TESTS=(
    swift-hello
    swift-network
    swift-bluetooth
    python-hello
    python-network
    python-gpu
    python-bluetooth
)

# If specific tests were requested via -t, filter the list.
if [[ ${#SELECTED_TESTS[@]} -gt 0 ]]; then
    TESTS=()
    for sel in "${SELECTED_TESTS[@]}"; do
        found=false
        for t in "${ALL_TESTS[@]}"; do
            if [[ "$t" == "$sel" ]]; then
                TESTS+=("$t")
                found=true
                break
            fi
        done
        if [[ "$found" == false ]]; then
            echo -e "${RED}ERROR: Unknown test '$sel'${RESET}"
            echo "Available tests: ${ALL_TESTS[*]}"
            exit 1
        fi
    done
else
    TESTS=("${ALL_TESTS[@]}")
fi

echo -e "${BOLD}==> Running ${#TESTS[@]} test(s)${RESET}"
echo ""

# ── Test loop ────────────────────────────────────────────────────────

for test_name in "${TESTS[@]}"; do
    test_dir="$TESTS_DIR/$test_name"

    # Verify directory exists
    if [[ ! -d "$test_dir" ]]; then
        skip_test "$test_name" "no directory"
        continue
    fi

    # Verify wendy.json exists
    if [[ ! -f "$test_dir/wendy.json" ]]; then
        skip_test "$test_name" "no wendy.json"
        continue
    fi

    # Extract appId
    app_id=$(jq -r '.appId' "$test_dir/wendy.json" 2>/dev/null)
    if [[ -z "$app_id" || "$app_id" == "null" ]]; then
        skip_test "$test_name" "no appId"
        continue
    fi

    # Pre-cleanup: remove leftover container from previous runs
    "$WENDY" apps remove "$app_id" --device "$HOSTNAME" --force &>/dev/null || true

    # Deploy and run
    run_test "$test_name" \
        bash -c "cd '$test_dir' && '$WENDY' run --device '$HOSTNAME'"

    # Post-cleanup: stop and remove
    "$WENDY" apps stop "$app_id" --device "$HOSTNAME" &>/dev/null || true
    "$WENDY" apps remove "$app_id" --device "$HOSTNAME" --force &>/dev/null || true
done

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
