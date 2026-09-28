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
  swift-hello               Basic Swift containerized deployment (no entitlements)
  swift-network             Swift with network entitlement (WiFi connectivity)
  swift-bluetooth           Swift with bluetooth entitlement
  swift-resources           Swift app with bundled resources (verifies resource loading)
  python-hello              Basic Python deployment (no entitlements)
  python-network            Python with network entitlement (WiFi connectivity)
  python-gpu                Python with GPU entitlement (CUDA verification)
  python-onnx-gpu           Python with GPU entitlement (ONNX Runtime CUDA inference)
  python-bluetooth          Python with bluetooth entitlement
  python-audio              Python with audio entitlement
  python-notifications      Verify notifications System API socket access WITH entitlement
  python-no-notifications   Verify notifications System API access is blocked WITHOUT entitlement
  python-no-network         Verify network is blocked WITHOUT entitlement
  python-no-bluetooth       Verify bluetooth is blocked WITHOUT entitlement
  python-no-ptrace          Verify ptrace is blocked by default seccomp profile (WDY-1099)
  python-no-unshare         Verify unshare is blocked by default seccomp profile (WDY-1099)
  python-no-kexec-module    Verify kernel-module/kexec syscalls are blocked by seccomp (WDY-1012)
  python-network-host-admin Verify host-admin networking grants CAP_NET_ADMIN (WDY-1094)
  python-no-net-admin       Verify plain host networking does NOT grant CAP_NET_ADMIN (WDY-1094)
  python-resources          Verify app-level CPU/memory/PID limits are enforced via cgroups (WDY-1729)
  python-multiservice-resources  Verify per-service resource override + app-level inheritance (WDY-1729)
  python-multiservice       Multi-service wendy.json: parallel build + dep-order creation
  python-servicename        Single service with serviceName: verifies WENDY_HOSTNAME/WENDY_APP_GROUP env injection (WDY-878)
  python-env                Verify top-level wendy.json env reaches the container, incl. \${VAR} expansion (WDY-2040)
                            Skipped for now: needs an agent carrying RunContainerLayersRequest.env.
  python-env-flag           Verify 'wendy run --env' overrides wendy.json env per key (WDY-2040)
                            Skipped for now, same agent requirement as python-env.
  python-multiservice-env   Verify app-level env is the per-service default and a service overrides it per key (WDY-2040)
  python-device-top         Deploy a long-running app and verify 'wendy device top --json' reports it (device top)
  compose-hello             docker-compose multi-service deployment with build: Dockerfiles
  compose-images            docker-compose multi-service deployment using public images
  otel-localhost-only       Verify OTEL receivers (4317/4318) are not reachable from the network

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
    # Exit code is a parameter: --help is success, a bad invocation is not.
    # A usage error exiting 0 let CI report a green run that never started
    # a single test (WDY-2171).
    exit "${1:-0}"
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
        *)                  echo "Unknown option: $1" >&2; usage 2 ;;
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

# A per-run docker-container builder is shared by every integration fixture.
# Retry one failed OCI export because self-hosted runners occasionally lose a
# solve while otherwise remaining healthy. If buildkitd itself died, recreate
# the disposable builder first so one failure does not cascade through the
# remaining fixtures. This is deliberately gated on WENDY_BUILDX_BUILDER; local
# user builds are never retried or removed automatically.
prepare_ci_oci_retry() {
    local output="$1"
    [[ -n "${WENDY_BUILDX_BUILDER:-}" ]] || return 1

    if grep -qiE \
        'failed to receive status:.*(EOF|Unavailable)|error reading from server: EOF|/run/buildkit/buildkitd\.sock: connect: connection refused|failed to list workers:.*Unavailable|ResourceExhausted:.*cannot allocate memory' \
        <<<"$output"; then
        local builder="${WENDY_BUILDX_BUILDER}-oci"
        echo "    BuildKit daemon failed; recreating disposable CI builder $builder and retrying once"
        docker buildx rm "$builder" --force >/dev/null 2>&1 || true
        return 0
    fi

    grep -qF 'docker buildx build (OCI export) failed' <<<"$output" || return 1
    echo "    OCI export failed on the disposable CI builder; retrying once"
}

# ── Container verdict ────────────────────────────────────────────────
# An attached `wendy run` exits 0 whatever the container did, so its exit code
# only tells us the deploy worked. Every standard test asserts inside the app
# and signals failure by exiting non-zero, so read that verdict back from the
# device instead (WDY-2171).

# app_id_for_test prints the appId a test directory declares.
app_id_for_test() {
    jq -r '.appId // empty' "$1/wendy.json" 2>/dev/null
}

# container_exit_code prints the exit code the device recorded for an app, or
# nothing when the app is absent or still running.
container_exit_code() {
    local app_id="$1"
    "$WENDY" device apps list --device "$HOSTNAME" --json 2>/dev/null |
        jq -r --arg app "$app_id" '
            .[]? | select(.name == $app)
                 | select(.runningState != "RUNNING")
                 | .exitCode // empty'
}

# assert_container_verdict decides whether the app's own assertions passed.
# Preferred signal is the exit code the device recorded, polled briefly because
# the state can lag the attached stream. Apps deployed under a serviceName
# report no exit code (their services[] entry carries only a runningState), so
# those fall back to the app's printed verdict.
assert_container_verdict() {
    local app_id="$1" out="$2" code="" state=""
    for _ in 1 2 3 4 5 6; do
        code=$(container_exit_code "$app_id")
        [[ -n "$code" ]] && break
        sleep 1
    done

    state=$("$WENDY" device apps list --device "$HOSTNAME" --json 2>/dev/null |
        jq -r --arg app "$app_id" '.[]? | select(.name == $app) | .runningState // empty')

    if [[ -n "$code" ]]; then
        if [[ "$code" != "0" ]]; then
            echo "App $app_id exited $code (runningState=$state) - its assertions failed"
            return 1
        fi
        return 0
    fi

    if [[ -z "$state" ]]; then
        echo "Device does not report app $app_id at all; the deploy cannot be verified"
        return 1
    fi

    # No exit code available: require a printed PASS verdict and nothing that
    # looks like a failure.
    if grep -qE "^(FAIL|Traceback)" <<<"$out"; then
        echo "App $app_id printed a failure verdict (runningState=$state)"
        return 1
    fi
    if ! grep -q "PASS" <<<"$out"; then
        echo "App $app_id reports no exit code (runningState=$state) and printed no PASS verdict"
        return 1
    fi
    return 0
}

# run_container_test deploys a single-container test and requires both that the
# deploy succeeded and that the app exited 0.
run_container_test() {
    local test_name="$1" test_dir="$2"; shift 2
    local app_id
    app_id=$(app_id_for_test "$test_dir")

    container_test_passes() {
        local out rc
        # --no-restart stops the agent restarting a test app that has done its
        # job and exited, which otherwise churns the recorded state (a
        # short-lived app reads back as CRASH_LOOPING).
        out=$("$WENDY" run --device "$HOSTNAME" --prefix "$test_dir" --no-restart "$@" 2>&1)
        rc=$?
        if [[ $rc -ne 0 ]] && prepare_ci_oci_retry "$out"; then
            echo "$out"
            out=$("$WENDY" run --device "$HOSTNAME" --prefix "$test_dir" --no-restart "$@" 2>&1)
            rc=$?
        fi
        echo "$out"
        if [[ $rc -ne 0 ]]; then
            return $rc
        fi
        if [[ -z "$app_id" ]]; then
            echo "Could not read appId from $test_dir/wendy.json; cannot verify the app's verdict"
            return 1
        fi
        assert_container_verdict "$app_id" "$out"
    }
    run_test "$test_name" container_test_passes "$@"
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

# ── Device under test ────────────────────────────────────────────────
# Recorded before anything runs, because a failure caused by an out-of-date
# agent is otherwise indistinguishable in the log from a product bug. The whole
# suite's verdict is only meaningful against a known agent, so identify it.

VERSION_JSON=$("$WENDY" device info --json --device "$HOSTNAME" 2>/dev/null || true)

# device_field prints a field of VERSION_JSON, or its fallback when the field is
# absent, null, or the empty string — a device reporting no deviceType at all is
# exactly the case worth seeing, so an empty value must not read as blank.
device_field() {
    local value=""
    if [[ -n "$VERSION_JSON" ]]; then
        value=$(echo "$VERSION_JSON" | jq -r --arg f "$1" '.[$f] // ""' 2>/dev/null || true)
    fi
    if [[ -n "$value" ]]; then
        echo "$value"
    else
        echo "$2"
    fi
}

if [[ -z "$VERSION_JSON" ]]; then
    echo -e "${YELLOW}==> WARNING: 'wendy device info' returned nothing — the agent under test cannot be identified${RESET}"
fi
DEVICE_TYPE=$(device_field deviceType '')
DEVICE_GPU_ARCH=$(device_field gpuArch '')

echo -e "${BOLD}==> Agent version: $(device_field version 'unknown')${RESET}"
echo -e "${BOLD}==> Device OS: $(device_field os 'unknown') $(device_field osVersion 'unknown')${RESET}"
echo -e "${BOLD}==> Device type: ${DEVICE_TYPE:-none reported} ($(device_field cpuArchitecture 'unknown'))${RESET}"

# ── Device capability detection ──────────────────────────────────────

# Both *-gpu tests are CUDA tests, so they need an NVIDIA GPU specifically —
# hasGpu is also true for any device with a /dev/dri node, which selected them
# on an amd64 host with an AMD iGPU where a CUDA test cannot pass.
DEVICE_GPU_VENDOR=$(device_field gpuVendor '')
DEVICE_HAS_CUDA=false
if [[ "$DEVICE_GPU_VENDOR" == "nvidia" ]]; then
    DEVICE_HAS_CUDA=true
fi

# GPU framework wheels are architecture-specific. Pick a userspace that
# matches the device instead of treating the NVIDIA vendor bit as sufficient:
# Orin (sm_87) uses the existing JetPack-6 / CUDA-12 fixture, while Thor
# (sm_110) uses the Ubuntu-24.04 / CUDA-13 fixture. Older agents did not report
# gpuArch, so identify Thor by device type and preserve CUDA-12 behavior for
# other older NVIDIA devices. Every other reported architecture remains opt-in
# after its fixture is hardware-verified.
DEVICE_GPU_FIXTURE_DOCKERFILE=""
if [[ "$DEVICE_HAS_CUDA" == "true" ]]; then
    case "$DEVICE_GPU_ARCH" in
        sm_87)          DEVICE_GPU_FIXTURE_DOCKERFILE="Dockerfile" ;;
        sm_110|sm_121)  DEVICE_GPU_FIXTURE_DOCKERFILE="Dockerfile.cuda13" ;;
        "")
            # gpuArch was added after Thor support. Device type lets those
            # older Thor agents select CUDA 13 without regressing older Orin
            # and generic NVIDIA agents, which retain the CUDA-12 fixture.
            if [[ "$DEVICE_TYPE" == "jetson-agx-thor" ]]; then
                DEVICE_GPU_FIXTURE_DOCKERFILE="Dockerfile.cuda13"
            else
                DEVICE_GPU_FIXTURE_DOCKERFILE="Dockerfile"
            fi
            ;;
    esac
fi
echo -e "${BOLD}==> CUDA GPU: ${DEVICE_HAS_CUDA} (vendor: ${DEVICE_GPU_VENDOR:-none}, arch: ${DEVICE_GPU_ARCH:-unknown})${RESET}"

# The Swift tests build their image with swift-container-plugin, so this host
# needs a Swift toolchain — which the CLI provisions through swiftly, and
# refuses to proceed without. Checking swiftly rather than swift matters on
# macOS, where /usr/bin/swift is an Xcode shim that exists even with no usable
# toolchain behind it. Skipping here rather than listing tests per-runner in
# the workflow keeps one source of truth for what exists.
HOST_HAS_SWIFT=false
if command -v swiftly &>/dev/null; then
    HOST_HAS_SWIFT=true
fi
echo -e "${BOLD}==> Swift toolchain (swiftly): ${HOST_HAS_SWIFT}${RESET}"
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
    swift-start-detach
    swift-network
    swift-bluetooth
    swift-resources
    python-hello
    python-network
    python-gpu
    python-onnx-gpu
    python-bluetooth
    python-audio
    python-notifications
    python-no-notifications
    python-no-network
    python-no-bluetooth
    python-no-ptrace
    python-no-unshare
    python-no-kexec-module
    python-network-host-admin
    python-no-net-admin
    python-resources
    python-multiservice
    python-multiservice-resources
    python-servicename
    python-env
    python-env-flag
    python-multiservice-env
    compose-hello
    compose-images
    compose-companion
    otel-localhost-only
    python-device-top
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

    if [[ "$test_name" == swift-* ]] && [[ "$HOST_HAS_SWIFT" != "true" ]]; then
        skip_test "$test_name" "no Swift toolchain (swiftly) on this host"
        continue
    fi

    if [[ "$test_name" == *"-gpu"* ]] && [[ "$DEVICE_HAS_CUDA" != "true" ]]; then
        skip_test "$test_name" "no NVIDIA GPU (vendor: ${DEVICE_GPU_VENDOR:-none})"
        continue
    fi

    if [[ "$test_name" == *"-gpu"* ]] && [[ -z "$DEVICE_GPU_FIXTURE_DOCKERFILE" ]]; then
        skip_test "$test_name" "CI GPU fixtures support sm_87, sm_110, and sm_121; device reports ${DEVICE_GPU_ARCH:-an unknown architecture}"
        continue
    fi

    # These two deploy over the chunk-diff path, which carries env only on an
    # agent built with RunContainerLayersRequest.env (WDY-2040). An older agent
    # ignores the field and starts the container without the env, and no CLI
    # change can work around that — so the result would say "product bug" when
    # it means "stale agent". Delete this block once the CI devices run an
    # agent carrying the field. python-multiservice-env is deliberately not
    # gated: it deploys over the registry path, which has carried env since
    # WDY-1268, so it still covers the app-level/per-service merge.
    if [[ "$test_name" == "python-env" || "$test_name" == "python-env-flag" ]]; then
        skip_test "$test_name" "needs an agent with chunk-diff env support (WDY-2040); device has $(device_field version 'unknown')"
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

    # ── Multi-service test ───────────────────────────────────────────────
    if [[ "$test_name" == "python-multiservice" ]]; then
        echo -e "${BOLD}── $test_name${RESET}"

        # 1. Full multi-service deploy: both services build & run.
        run_test "python-multiservice (all services)" \
            "$WENDY" run --device "$HOSTNAME" --prefix "$test_dir" --deploy

        # 2. --service filtering: deploy only 'api' (and its dep 'db').
        run_test "python-multiservice (--service api)" \
            "$WENDY" run --device "$HOSTNAME" --prefix "$test_dir" --deploy --service api

        # 3. --service with unknown name must fail with a clear error.
        unknown_service_fails() {
            local out
            out=$("$WENDY" run --device "$HOSTNAME" --prefix "$test_dir" --deploy --service ghost 2>&1)
            local rc=$?
            if [[ $rc -ne 0 ]] && echo "$out" | grep -qi "ghost"; then
                return 0
            fi
            echo "Expected non-zero exit and 'ghost' in output; got rc=$rc output=$out"
            return 1
        }
        run_test "python-multiservice (--service ghost -> error)" unknown_service_fails

        continue
    fi

    # ── Multi-service resource limits (WDY-1729) ─────────────────────────
    # Deploys two services: 'db' inherits the app-level memory limit (256Mi),
    # 'api' overrides it to 128Mi while inheriting pids. Each service asserts
    # its own cgroup limits and prints "<svc>: PASS" / "<svc>: FAIL". We read
    # back the logs (bounded by a background reader) and require both PASS lines
    # with no FAIL.
    if [[ "$test_name" == "python-multiservice-resources" ]]; then
        app_id="sh.wendy.ci.python-multiservice-resources"

        # Attached, not --deploy: --deploy creates the containers without
        # starting them, so neither service would run and neither verdict
        # would exist to read (WDY-2171). Attached mode streams both services'
        # stdout, and per-service exit codes are not reported by the device, so
        # assert on the verdict lines.
        per_service_limits_enforced() {
            local out
            out=$("$WENDY" run --device "$HOSTNAME" --prefix "$test_dir" --no-restart 2>&1)
            echo "$out"
            if grep -qE "(db|api): FAIL" <<<"$out"; then
                return 1
            fi
            grep -q "db: PASS" <<<"$out" && grep -q "api: PASS" <<<"$out"
        }
        run_test "python-multiservice-resources (per-service limits)" per_service_limits_enforced

        "$WENDY" device apps remove "$app_id" --device "$HOSTNAME" --force --cleanup >/dev/null 2>&1 || true
        continue
    fi

    # ── Multi-service env (WDY-2040) ─────────────────────────────────────
    # 'alpha' declares no env and must inherit both app-level keys; 'beta'
    # overrides one and adds its own, and must still inherit the other. Each
    # service prints "<svc>: PASS" / "<svc>: FAIL" and exits.
    #
    # Deliberately an attached run, not --deploy: --deploy creates the
    # containers without starting them, so neither service would ever run.
    # Attached mode streams both services' stdout, and the per-service exit
    # codes do not reach the CLI, so assert on the verdict lines.
    if [[ "$test_name" == "python-multiservice-env" ]]; then
        app_id="sh.wendy.ci.python-multiservice-env"

        per_service_env_applied() {
            local out
            out=$("$WENDY" run --device "$HOSTNAME" --prefix "$test_dir" 2>&1)
            echo "$out"
            if grep -q "alpha: FAIL" <<<"$out" || grep -q "beta: FAIL" <<<"$out"; then
                return 1
            fi
            grep -q "alpha: PASS" <<<"$out" && grep -q "beta: PASS" <<<"$out"
        }
        run_test "$test_name" per_service_env_applied

        "$WENDY" device apps remove "$app_id" --device "$HOSTNAME" --force --cleanup >/dev/null 2>&1 || true
        continue
    fi

    # ── device top (live monitor) ────────────────────────────────────────
    # Deploy a long-running app, then verify `wendy device top --json` reports
    # the host snapshot and lists the deployed container.
    if [[ "$test_name" == "python-device-top" ]]; then
        echo -e "${BOLD}── $test_name${RESET}"
        app_id="sh.wendy.ci.python-device-top"

        run_test "python-device-top (deploy)" \
            "$WENDY" run --device "$HOSTNAME" --prefix "$test_dir" --detach

        device_top_snapshot() {
            # stderr stays out of the jq input: in JSON mode it can carry
            # notices, progress, or the error envelope.
            local out err rc errf
            errf=$(mktemp)
            out=$("$WENDY" device top --device "$HOSTNAME" --json 2>"$errf")
            rc=$?
            err=$(cat "$errf"); rm -f "$errf"
            if [[ $rc -ne 0 ]]; then
                echo "wendy device top --json failed (rc=$rc): $err $out"
                return 1
            fi
            if ! echo "$out" | jq -e '.host.cpuCount > 0 and .host.memTotalBytes > 0' >/dev/null 2>&1; then
                echo "host.cpuCount / host.memTotalBytes missing or zero: $out"
                return 1
            fi
            if ! echo "$out" | jq -e --arg a "$app_id" \
				'(.containers // []) | any(.name == $a and .state == "running")' >/dev/null 2>&1; then
				echo "running app '$app_id' with populated state not found in containers[]: $out"
                return 1
            fi
            return 0
        }
        run_test "python-device-top (snapshot reports host + container)" device_top_snapshot

        "$WENDY" device apps remove "$app_id" --device "$HOSTNAME" --force --cleanup >/dev/null 2>&1 || true
        continue
    fi

    # ── apps start reporting (detached start must confirm the start) ──────
    # A detached `apps start` must return only after the agent confirms the
    # container started (deploy first WITHOUT starting so no prior restart
    # policy masks the result), and must fail — not falsely succeed — when the
    # target app does not exist.
    if [[ "$test_name" == "swift-start-detach" ]]; then
        echo -e "${BOLD}── $test_name${RESET}"
        app_id="sh.wendy.ci.swift-start-detach"

        run_test "swift-start-detach (deploy, not started)" \
            "$WENDY" run --device "$HOSTNAME" --prefix "$test_dir" --deploy

        detach_start_reaches_running() {
            if ! "$WENDY" device apps start "$app_id" --device "$HOSTNAME" --detach; then
                echo "apps start --detach returned non-zero"
                return 1
            fi
            local out err errf
            errf=$(mktemp)
            for _ in 1 2 3 4 5; do
                # stderr stays out of the jq input (notices, error envelope),
                # but is kept for the failure message.
                out=$("$WENDY" device apps list --device "$HOSTNAME" --json 2>"$errf")
                if echo "$out" | jq -e --arg a "$app_id" \
                    '(.[] | select(.name==$a) | .runningState) == "RUNNING"' >/dev/null 2>&1; then
                    rm -f "$errf"
                    return 0
                fi
                sleep 1
            done
            err=$(cat "$errf"); rm -f "$errf"
            echo "app '$app_id' never reached RUNNING after detached start: $out $err"
            return 1
        }
        run_test "swift-start-detach (detached start reaches RUNNING)" detach_start_reaches_running

        detach_start_missing_app_fails() {
            if "$WENDY" device apps start sh.wendy.ci.does-not-exist --device "$HOSTNAME" --detach >/dev/null 2>&1; then
                echo "apps start --detach of a nonexistent app unexpectedly reported success"
                return 1
            fi
            return 0
        }
        run_test "swift-start-detach (missing app start fails)" detach_start_missing_app_fails

        "$WENDY" device apps remove "$app_id" --device "$HOSTNAME" --force --cleanup >/dev/null 2>&1 || true
        continue
    fi

    # ── env tests ────────────────────────────────────────────────────────
    # An attached `wendy run` exits 0 even when the container exits non-zero,
    # so assert on the app's own verdict line instead of the exit code.
    if [[ "$test_name" == python-env* ]]; then
        env_test_passes() {
            local out
            out=$("$WENDY" run --device "$HOSTNAME" --prefix "$test_dir" "$@" 2>&1)
            echo "$out"
            grep -q '^PASS:' <<<"$out"
        }
        if [[ "$test_name" == "python-env-flag" ]]; then
            # The overrides are the thing under test, so the runner supplies
            # them: wendy.json sets CI_ENV_LEVEL=info and the flag must win,
            # CI_ENV_REGION must survive, CI_ENV_ONLY_FLAG is flag-only.
            run_test "$test_name" env_test_passes --env CI_ENV_LEVEL=debug --env CI_ENV_ONLY_FLAG=1
        else
            run_test "$test_name" env_test_passes
        fi
        continue
    fi

    # ── compose tests ────────────────────────────────────────────────────
    if [[ "$test_name" == compose-* ]]; then
        run_test "$test_name" \
            "$WENDY" run --device "$HOSTNAME" --prefix "$test_dir" --detach
        continue
    fi

    # ── Standard single-container tests ─────────────────────────────────
    if [[ "$test_name" == *"-gpu"* ]]; then
        # Both GPU test directories contain architecture-specific Dockerfiles,
        # so name the selected one explicitly and avoid an interactive picker.
        run_container_test "$test_name" "$test_dir" --dockerfile "$DEVICE_GPU_FIXTURE_DOCKERFILE"
    else
        run_container_test "$test_name" "$test_dir"
    fi
done

# ── Summary ──────────────────────────────────────────────────────────

echo ""
echo -e "${BOLD}==> Results: ${GREEN}${PASS_COUNT} passed${RESET}, ${RED}${FAIL_COUNT} failed${RESET}, ${YELLOW}${SKIP_COUNT} skipped${RESET}"
echo ""

if [[ $FAIL_COUNT -gt 0 ]]; then
    exit 1
fi
exit 0
