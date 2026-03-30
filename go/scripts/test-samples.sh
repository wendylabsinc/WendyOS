#!/bin/bash
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Test foundational examples across all languages by deploying to a real
WendyOS device using 'wendy run'.

Foundational examples: hello-world, simple-server, web-app,
persistent-volume, sqlite-persistence.

Languages: python, swift, rust, node-typescript, cpp.

Device Selection:
  If --hostname is not provided, the script auto-discovers a device on the
  local network using 'wendy discover --json'. The first LAN device found
  is used.

Options:
  -h, --hostname HOST   Device hostname (skips auto-discovery)
  -w, --wendy PATH      Path to wendy binary (default: wendy on PATH)
  --help                Show this help message

Examples:
  $(basename "$0")                                  # auto-discover
  $(basename "$0") -h wendyos-merry-aurora          # explicit host
  $(basename "$0") -w /path/to/wendy                # custom binary
EOF
    exit 0
}

HOSTNAME=""
HOSTNAME_PROVIDED=false
WENDY="wendy"

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--hostname)      HOSTNAME="$2"; HOSTNAME_PROVIDED=true; shift 2 ;;
        -w|--wendy)         WENDY="$2"; shift 2 ;;
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
        echo "    Output: $(echo "$output" | head -5)"
        ((FAIL_COUNT++))
    fi
    return $rc
}

skip_test() {
    local name="$1"
    printf "  %-50s " "$name"
    echo -e "${YELLOW}SKIP${RESET}"
    ((SKIP_COUNT++))
}

# ── Wendy binary validation ─────────────────────────────────────────

if [[ "$WENDY" != "wendy" ]]; then
    # Explicit path provided — check it exists and is executable
    if [[ ! -x "$WENDY" ]]; then
        echo -e "${RED}ERROR: wendy binary not found or not executable at $WENDY${RESET}"
        exit 1
    fi
else
    # Default — check wendy is on PATH
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

# ── Test definitions ─────────────────────────────────────────────────

EXAMPLES=(hello-world simple-server web-app persistent-volume sqlite-persistence)
LANGUAGES=(python swift rust node-typescript cpp)

# Server examples use --detach; everything else runs to completion.
is_server() {
    [[ "$1" == "simple-server" || "$1" == "web-app" ]]
}

# ── Test loop ────────────────────────────────────────────────────────

for example in "${EXAMPLES[@]}"; do
    echo -e "${BOLD}==> $example${RESET}"

    for lang in "${LANGUAGES[@]}"; do
        test_name="$lang/$example"
        dir="$REPO_DIR/$lang/$example"

        # Check directory exists
        if [[ ! -d "$dir" ]]; then
            skip_test "$test_name (no directory)"
            continue
        fi

        # Check wendy.json exists
        if [[ ! -f "$dir/wendy.json" ]]; then
            skip_test "$test_name (no wendy.json)"
            continue
        fi

        # Extract appId
        app_id=$(jq -r '.appId' "$dir/wendy.json" 2>/dev/null)
        if [[ -z "$app_id" || "$app_id" == "null" ]]; then
            skip_test "$test_name (no appId)"
            continue
        fi

        # Pre-cleanup: remove leftover container from previous runs (best-effort)
        "$WENDY" apps remove "$app_id" --device "$HOSTNAME" --force &>/dev/null || true

        # Run the example
        if is_server "$example"; then
            run_test "$test_name" \
                bash -c "cd '$dir' && '$WENDY' run --device '$HOSTNAME' --detach"
        else
            run_test "$test_name" \
                bash -c "cd '$dir' && '$WENDY' run --device '$HOSTNAME'"
        fi

        # Post-cleanup: stop and remove (best-effort)
        "$WENDY" apps stop "$app_id" --device "$HOSTNAME" &>/dev/null || true
        "$WENDY" apps remove "$app_id" --device "$HOSTNAME" --force &>/dev/null || true
    done

    echo ""
done

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
