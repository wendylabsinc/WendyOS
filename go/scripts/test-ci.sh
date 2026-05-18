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
  swift-hello           Basic Swift containerized deployment (no entitlements)
  swift-network         Swift with network entitlement (WiFi connectivity)
  swift-bluetooth       Swift with bluetooth entitlement
  swift-resources       Swift app with bundled resources (verifies resource loading)
  python-hello          Basic Python deployment (no entitlements)
  python-network        Python with network entitlement (WiFi connectivity)
  python-gpu            Python with GPU entitlement (CUDA verification)
  python-onnx-gpu       Python with GPU entitlement (ONNX Runtime CUDA inference)
  python-bluetooth      Python with bluetooth entitlement
  python-no-network     Verify network is blocked WITHOUT entitlement
  python-no-bluetooth   Verify bluetooth is blocked WITHOUT entitlement
  python-no-ptrace      Verify ptrace is blocked by default seccomp profile (WDY-1099)
  python-no-unshare     Verify unshare is blocked by default seccomp profile (WDY-1099)
  python-restart        Verify --restart-unless-stopped restarts containers; stop suppresses restart
  compose-hello         docker-compose multi-service deployment with build: Dockerfiles
  compose-images        docker-compose multi-service deployment using public images
  otel-localhost-only   Verify OTEL receivers (4317/4318) are not reachable from the network

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

# Add .local suffix only for bare mDNS hostnames (no dots or colons).
# Leave IPs (e.g. 192.168.1.1), FQDNs (device.example.com), and IPv6 alone.
if [[ "$HOSTNAME_PROVIDED" == true ]] && [[ "$HOSTNAME" != *.local ]] && [[ "$HOSTNAME" != *.* ]] && [[ "$HOSTNAME" != *:* ]]; then
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
    WENDY="$(cd "$(dirname "$WENDY")" && pwd)/$(basename "$WENDY")"
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
    DISCOVER_STDERR=$(mktemp -t wendy-discover-stderr.XXXXXX)
    trap 'rm -f "$DISCOVER_STDERR"' EXIT
    DISCOVER_JSON=$("$WENDY" discover --json --timeout 5s 2>"$DISCOVER_STDERR")
    cat "$DISCOVER_STDERR" >&2 || true
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

# ── Device capability detection ──────────────────────────────────────

DEVICE_HAS_GPU=false
VERSION_JSON=$("$WENDY" device info --json --device "$HOSTNAME" 2>/dev/null || true)
if [[ -n "$VERSION_JSON" ]]; then
    GPU_VAL=$(echo "$VERSION_JSON" | jq -r '.hasGpu // false' 2>/dev/null || true)
    if [[ "$GPU_VAL" == "true" ]]; then
        DEVICE_HAS_GPU=true
    fi
fi
echo -e "${BOLD}==> GPU: ${DEVICE_HAS_GPU}${RESET}"
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
    swift-resources
    python-hello
    python-network
    python-gpu
    python-onnx-gpu
    python-bluetooth
    python-no-network
    python-no-bluetooth
    python-no-ptrace
    python-no-unshare
    python-restart
    compose-hello
    compose-images
    otel-localhost-only
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

    # Skip GPU tests on devices that don't have a GPU.
    if [[ "$test_name" == *"-gpu"* ]] && [[ "$DEVICE_HAS_GPU" != "true" ]]; then
        skip_test "$test_name" "no GPU"
        continue
    fi

    # ── Security: OTEL ports must not be reachable from the network ──────
    if [[ "$test_name" == "otel-localhost-only" ]]; then
        otel_grpc_closed() { ! nc -z -w 3 "$HOSTNAME" 4317 2>/dev/null; }
        otel_http_closed() { ! nc -z -w 3 "$HOSTNAME" 4318 2>/dev/null; }
        run_test "otel-localhost-only (gRPC 4317 not reachable)" otel_grpc_closed
        run_test "otel-localhost-only (HTTP 4318 not reachable)" otel_http_closed
        continue
    fi

    # ── Restart policy: deploy with --restart-unless-stopped, verify restart,
    #    then stop and verify no restart occurs. ──────────────────────────
    if [[ "$test_name" == "python-restart" ]]; then
        APP_ID="sh.wendy.ci.python-restart"

        # Sub-test 1: container is restarted by the monitor after it exits.
        restart_is_restarted() {
            # Deploy the app with --restart-unless-stopped.
            # The container exits immediately; the monitor should restart it
            # within ~15 s (the monitor's default tick interval).
            "$WENDY" deploy \
                --device "$HOSTNAME" \
                --restart-unless-stopped \
                "$test_dir" >/dev/null 2>&1 || return 1

            # Wait up to 30 s for the monitor to restart the container.
            local deadline=$(( $(date +%s) + 30 ))
            while [[ $(date +%s) -lt $deadline ]]; do
                sleep 3
                STATUS=$("$WENDY" container list --json --device "$HOSTNAME" 2>/dev/null \
                    | jq -r --arg id "$APP_ID" \
                        '.[] | select(.appId == $id) | .status' 2>/dev/null || true)
                if [[ "$STATUS" == "running" ]]; then
                    return 0
                fi
            done
            echo "Container did not restart within 30 s (last status: ${STATUS:-unknown})" >&2
            return 1
        }
        run_test "python-restart (restarted by monitor after exit)" restart_is_restarted

        # Sub-test 2: after an explicit stop the container must NOT restart.
        restart_stop_no_restart() {
            # Stop the container (sets ExplicitStop in the monitor).
            "$WENDY" container stop --device "$HOSTNAME" "$APP_ID" >/dev/null 2>&1 || return 1

            # Wait the full monitor tick interval plus a safety margin (20 s)
            # and confirm the container has not been restarted.
            sleep 20
            STATUS=$("$WENDY" container list --json --device "$HOSTNAME" 2>/dev/null \
                | jq -r --arg id "$APP_ID" \
                    '.[] | select(.appId == $id) | .status' 2>/dev/null || true)
            if [[ "$STATUS" == "running" ]]; then
                echo "Container restarted after explicit stop (status: running)" >&2
                return 1
            fi
            return 0
        }
        run_test "python-restart (not restarted after explicit stop)" restart_stop_no_restart

        # Cleanup: delete the deployed app regardless of test outcome.
        "$WENDY" container delete --device "$HOSTNAME" "$APP_ID" >/dev/null 2>&1 || true
        continue
    fi

    # ── Standard deploy-and-run tests ───────────────────────────────────
    if [[ -f "$test_dir/docker-compose.yml" ]] && [[ ! -f "$test_dir/wendy.json" ]]; then
        run_test "$test_name" \
            "$WENDY" compose up \
                --device "$HOSTNAME" \
                --detach=false \
                "$test_dir/docker-compose.yml"
    else
        run_test "$test_name" \
            "$WENDY" run \
                --device "$HOSTNAME" \
                "$test_dir"
    fi
done

echo ""
echo -e "${BOLD}==> Results: ${GREEN}${PASS_COUNT} passed${RESET}, ${RED}${FAIL_COUNT} failed${RESET}, ${YELLOW}${SKIP_COUNT} skipped${RESET}"
echo ""

if [[ $FAIL_COUNT -gt 0 ]]; then
    exit 1
fi
exit 0
