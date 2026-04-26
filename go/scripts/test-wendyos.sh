#!/bin/bash
set -uo pipefail

# Smoke test for WendyOS devices (Jetson and Pi).
# Clones WendySamples, runs appropriate tests based on device type, cleans up.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/test-harness.sh"

# ── Usage ───────────────────────────────────────────────────────────

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Smoke test for WendyOS devices (Jetson and Pi). Clones WendySamples, runs
foundational examples across all languages, and optionally tests GPU samples
on Jetson devices.

Options:
  -h, --hostname HOST         Device hostname (skips auto-discovery)
  -w, --wendy PATH            Path to wendy binary (default: wendy on PATH)
  --device-type jetson|pi     Device type (auto-detected if omitted)
  --skip-gpu                  Force skip GPU samples even on Jetson
  --samples-dir PATH          Run all wendy apps found in PATH (recursive)
  --samples-branch BRANCH     Git branch (default: main)
  --help                      Show this help message

Examples:
  $(basename "$0")                                        # auto-discover everything
  $(basename "$0") -h wendyos-merry-aurora                # explicit host
  $(basename "$0") -h jetson-01 --device-type jetson      # explicit Jetson
  $(basename "$0") --samples-dir ../WendySamples          # run all apps in dir
EOF
    exit 0
}

# ── Parse arguments ─────────────────────────────────────────────────

HOSTNAME=""
HOSTNAME_PROVIDED=false
WENDY="wendy"
DEVICE_TYPE=""
SKIP_GPU=false
SAMPLES_DIR=""
SAMPLES_DIR_PROVIDED=false
SAMPLES_BRANCH="main"

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--hostname)      HOSTNAME="$2"; HOSTNAME_PROVIDED=true; shift 2 ;;
        -w|--wendy)         WENDY="$2"; shift 2 ;;
        --device-type)      DEVICE_TYPE="$2"; shift 2 ;;
        --skip-gpu)         SKIP_GPU=true; shift ;;
        --samples-dir)      SAMPLES_DIR="$2"; SAMPLES_DIR_PROVIDED=true; shift 2 ;;
        --samples-branch)   SAMPLES_BRANCH="$2"; shift 2 ;;
        --help)             usage ;;
        *)                  echo "Unknown option: $1"; usage ;;
    esac
done

# Add .local suffix if hostname was explicitly provided and missing it.
if [[ "$HOSTNAME_PROVIDED" == true ]] && [[ "$HOSTNAME" != *.local ]]; then
    HOSTNAME="${HOSTNAME}.local"
fi

# ── Phase 1: Setup ──────────────────────────────────────────────────

echo -e "${BOLD}==> Phase 1: Setup${RESET}"

validate_wendy_binary || exit 1
echo -e "${BOLD}==> Using wendy: ${WENDY}${RESET}"
echo ""

# ── Phase 2: Clone WendySamples ─────────────────────────────────────

echo -e "${BOLD}==> Phase 2: Acquire samples${RESET}"

CLONE_DIR=""
if [[ -n "$SAMPLES_DIR" ]]; then
    if [[ ! -d "$SAMPLES_DIR" ]]; then
        echo -e "${RED}ERROR: Samples directory not found: $SAMPLES_DIR${RESET}"
        exit 1
    fi
    SAMPLES_DIR="$(cd "$SAMPLES_DIR" && pwd)"
    echo "Using local samples: $SAMPLES_DIR"
else
    CLONE_DIR=$(mktemp -d)
    trap 'rm -rf "$CLONE_DIR"' EXIT
    echo "Cloning WendySamples ($SAMPLES_BRANCH) into $CLONE_DIR..."
    git clone --depth 1 --branch "$SAMPLES_BRANCH" \
        https://github.com/wendylabsinc/samples.git "$CLONE_DIR" 2>&1 | tail -1
    SAMPLES_DIR="$CLONE_DIR"
fi
echo ""

# ── Phase 3: Discover device ────────────────────────────────────────

echo -e "${BOLD}==> Phase 3: Device discovery${RESET}"

if [[ -z "$HOSTNAME" ]]; then
    discover_device "$WENDY" || exit 1
fi

echo -e "${BOLD}==> Target device: ${HOSTNAME}${RESET}"
echo ""

# ── Phase 4: Auto-detect device type ────────────────────────────────

echo -e "${BOLD}==> Phase 4: Device type detection${RESET}"

if [[ -z "$DEVICE_TYPE" ]]; then
    echo "Auto-detecting device type..."
    HW_JSON=$("$WENDY" hardware list --json --device "$HOSTNAME" 2>&1) || true
    if echo "$HW_JSON" | grep -qiE "nvidia|jetson"; then
        DEVICE_TYPE="jetson"
    else
        DEVICE_TYPE="pi"
    fi
fi

echo -e "Device type: ${BOLD}${DEVICE_TYPE}${RESET}"
if [[ "$DEVICE_TYPE" == "jetson" ]] && [[ "$SKIP_GPU" == true ]]; then
    echo "(GPU samples will be skipped per --skip-gpu)"
fi
echo ""

# ── Phase 5: Run samples ───────────────────────────────────────────

TESTED_APPS=()

if [[ "$SAMPLES_DIR_PROVIDED" == true ]]; then
    echo -e "${BOLD}==> Phase 5: Run all apps in ${SAMPLES_DIR}${RESET}"

    while IFS= read -r wendy_json; do
        dir="$(dirname "$wendy_json")"
        test_name="${dir#"$SAMPLES_DIR/"}"

        # Validate JSON
        if ! jq . "$wendy_json" >/dev/null 2>&1; then
            skip_test "$test_name (invalid wendy.json)"
            continue
        fi

        # Extract appId
        app_id=$(jq -r '.appId' "$wendy_json" 2>/dev/null)
        if [[ -z "$app_id" || "$app_id" == "null" ]]; then
            skip_test "$test_name (no appId)"
            continue
        fi

        # Determine run mode: detach if readiness config exists
        detach_flag=""
        if jq -e '.readiness' "$wendy_json" >/dev/null 2>&1; then
            detach_flag="--detach"
        fi

        # Pre-cleanup: remove leftover container and image from previous runs
        "$WENDY" apps remove "$app_id" --device "$HOSTNAME" --force --cleanup &>/dev/null || true

        # Run the app
        run_test "$test_name" \
            bash -c "cd '$dir' && '$WENDY' run --device '$HOSTNAME' $detach_flag"

        # Track tested apps for GPU dedup
        TESTED_APPS+=("$dir")

        # Post-cleanup: stop and remove container + image
        "$WENDY" apps stop "$app_id" --device "$HOSTNAME" &>/dev/null || true
        "$WENDY" apps remove "$app_id" --device "$HOSTNAME" --force --cleanup &>/dev/null || true

    done < <(find "$SAMPLES_DIR" -name wendy.json -type f 2>/dev/null | sort)
    echo ""
else
    echo -e "${BOLD}==> Phase 5: Foundational samples${RESET}"

    EXAMPLES=(hello-world simple-server web-app persistent-volume sqlite-persistence)
    LANGUAGES=(python swift rust node-typescript cpp)

    is_server() {
        [[ "$1" == "simple-server" || "$1" == "web-app" ]]
    }

    for example in "${EXAMPLES[@]}"; do
        echo -e "${BOLD}--- $example ---${RESET}"

        for lang in "${LANGUAGES[@]}"; do
            test_name="$lang/$example"
            dir="$SAMPLES_DIR/$lang/$example"

            # Check directory exists
            if [[ ! -d "$dir" ]]; then
                skip_test "$test_name (no directory)"
                continue
            fi

            # Generate wendy.json via wendy init if missing (swift/python only)
            case "$lang" in
                swift|python) ensure_wendy_json "$dir" "$lang" "wendyos" "$WENDY" ;;
            esac

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

            # Pre-cleanup: remove leftover container and image from previous runs
            "$WENDY" apps remove "$app_id" --device "$HOSTNAME" --force --cleanup &>/dev/null || true

            # Run the example
            if is_server "$example"; then
                run_test "$test_name" \
                    bash -c "cd '$dir' && '$WENDY' run --device '$HOSTNAME' --detach"
            else
                run_test "$test_name" \
                    bash -c "cd '$dir' && '$WENDY' run --device '$HOSTNAME'"
            fi

            # Track tested apps for GPU dedup
            TESTED_APPS+=("$dir")

            # Post-cleanup: stop and remove container + image
            "$WENDY" apps stop "$app_id" --device "$HOSTNAME" &>/dev/null || true
            "$WENDY" apps remove "$app_id" --device "$HOSTNAME" --force --cleanup &>/dev/null || true

            # Clean up generated wendy.json
            cleanup_generated_wendy_json "$dir"
        done

        echo ""
    done
fi

# ── Phase 6: GPU samples (Jetson only) ─────────────────────────────

echo -e "${BOLD}==> Phase 6: GPU samples${RESET}"

if [[ "$DEVICE_TYPE" != "jetson" ]]; then
    echo "Skipping GPU samples (device type: $DEVICE_TYPE)"
    echo ""
elif [[ "$SKIP_GPU" == true ]]; then
    echo "Skipping GPU samples (--skip-gpu)"
    echo ""
else
    GPU_FOUND=0

    while IFS= read -r wendy_json; do
        # Check for GPU entitlement
        if ! has_gpu_entitlement "$wendy_json"; then
            continue
        fi

        gpu_dir="$(dirname "$wendy_json")"

        # Skip if already tested in Phase 5
        already_tested=false
        for tested in "${TESTED_APPS[@]}"; do
            if [[ "$gpu_dir" == "$tested" ]]; then
                already_tested=true
                break
            fi
        done
        if [[ "$already_tested" == true ]]; then
            continue
        fi

        # Extract test name relative to samples dir
        test_name="${gpu_dir#"$SAMPLES_DIR/"}"

        # Extract appId
        app_id=$(jq -r '.appId' "$wendy_json" 2>/dev/null)
        if [[ -z "$app_id" || "$app_id" == "null" ]]; then
            skip_test "$test_name (no appId)"
            continue
        fi

        ((GPU_FOUND++))

        # Pre-cleanup: remove leftover container and image from previous runs
        "$WENDY" apps remove "$app_id" --device "$HOSTNAME" --force --cleanup &>/dev/null || true

        # All GPU samples run detached (they're long-running)
        run_test "$test_name" \
            bash -c "cd '$gpu_dir' && '$WENDY' run --device '$HOSTNAME' --detach"

        # Post-cleanup: stop and remove container + image
        "$WENDY" apps stop "$app_id" --device "$HOSTNAME" &>/dev/null || true
        "$WENDY" apps remove "$app_id" --device "$HOSTNAME" --force --cleanup &>/dev/null || true

    done < <(find "$SAMPLES_DIR" -name wendy.json -type f 2>/dev/null)

    if [[ $GPU_FOUND -eq 0 ]]; then
        echo "No GPU samples found in samples repo."
    fi
    echo ""
fi

# ── Phase 7: Summary ───────────────────────────────────────────────

print_summary
exit $?
