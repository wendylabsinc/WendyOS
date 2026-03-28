#!/bin/bash
set -uo pipefail

# Smoke test for ESP32 (WendyLite) — runs Swift samples via wendy run.
# Clones wendy-lite-samples, discovers Swift projects, runs them, cleans up.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/test-harness.sh"

# ── Usage ───────────────────────────────────────────────────────────

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Smoke test for ESP32 (WendyLite). Clones the wendy-lite-samples repo,
discovers Swift projects, and runs each one via 'wendy run'.

Options:
  -h, --hostname HOST         Device hostname (skips auto-discovery)
  -w, --wendy PATH            Path to wendy binary (default: wendy on PATH)
  --samples-dir PATH          Use local samples dir instead of cloning
  --samples-branch BRANCH     Git branch (default: main)
  --help                      Show this help message

Examples:
  $(basename "$0")                                        # auto-discover everything
  $(basename "$0") -h wendylite-blinky-star               # explicit host
  $(basename "$0") --samples-dir ../wendy-lite-samples    # local samples
EOF
    exit 0
}

# ── Parse arguments ─────────────────────────────────────────────────

HOSTNAME=""
HOSTNAME_PROVIDED=false
WENDY="wendy"
SAMPLES_DIR=""
SAMPLES_BRANCH="main"

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--hostname)      HOSTNAME="$2"; HOSTNAME_PROVIDED=true; shift 2 ;;
        -w|--wendy)         WENDY="$2"; shift 2 ;;
        --samples-dir)      SAMPLES_DIR="$2"; shift 2 ;;
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

# ── Phase 2: Clone wendy-lite-samples ───────────────────────────────

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
    echo "Cloning wendy-lite-samples ($SAMPLES_BRANCH) into $CLONE_DIR..."
    git clone --depth 1 --branch "$SAMPLES_BRANCH" \
        https://github.com/wendylabsinc/wendy-lite-samples.git "$CLONE_DIR" 2>&1 | tail -1
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

# ── Phase 4: Discover and run Swift samples ─────────────────────────

echo -e "${BOLD}==> Phase 4: Swift samples${RESET}"

FOUND=0

while IFS= read -r package_swift; do
    dir="$(dirname "$package_swift")"

    # Extract test name relative to samples dir
    test_name="${dir#"$SAMPLES_DIR/"}"

    # Generate wendy.json via wendy init if missing
    ensure_wendy_json "$dir" "swift" "wendy-lite" "$WENDY"

    # Still need wendy.json to proceed
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

    ((FOUND++))

    # Pre-cleanup: remove leftover container from previous runs
    "$WENDY" apps remove "$app_id" --device "$HOSTNAME" --force &>/dev/null || true

    # Run the sample
    run_test "$test_name" \
        bash -c "cd '$dir' && '$WENDY' run --device '$HOSTNAME'"

    # Post-cleanup: stop and remove
    "$WENDY" apps stop "$app_id" --device "$HOSTNAME" &>/dev/null || true
    "$WENDY" apps remove "$app_id" --device "$HOSTNAME" --force &>/dev/null || true

    # Clean up generated wendy.json
    cleanup_generated_wendy_json "$dir"

done < <(find "$SAMPLES_DIR" -name Package.swift -type f 2>/dev/null | sort)

if [[ $FOUND -eq 0 ]]; then
    echo -e "${YELLOW}No Swift samples found in repo.${RESET}"
fi
echo ""

# ── Phase 5: Summary ───────────────────────────────────────────────

print_summary
exit $?
