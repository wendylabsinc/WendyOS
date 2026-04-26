#!/bin/bash
set -uo pipefail

# Smoke test for wendy init --template. Generates all template x language
# combinations from meta.json, validates each scaffolded project, then
# optionally deploys them to a WendyOS device via test-wendyos.sh.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/test-harness.sh"

# ── Usage ───────────────────────────────────────────────────────────

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Generate all template x language combinations via 'wendy init --template',
validate the scaffolded projects, and optionally deploy them to a WendyOS device.

Options:
  -h, --hostname HOST           Device hostname for run phase (skips auto-discovery)
  -w, --wendy PATH              Path to wendy binary (default: wendy on PATH)
  --templates-dir PATH          Use local templates repo instead of cloning
  --templates-branch BRANCH     Git branch when cloning (default: main)
  --skip-run                    Only test generation, skip deploying to device
  --template NAME               Only test a specific template (e.g. simple-api)
  --language LANG               Only test a specific language (e.g. python)
  --help                        Show this help message

Examples:
  $(basename "$0") --skip-run                              # generate + validate only
  $(basename "$0") --skip-run --language python             # filter to one language
  $(basename "$0") --skip-run --template simple-api         # filter to one template
  $(basename "$0") -h wendyos-merry-aurora                  # full run with device
  $(basename "$0") --templates-dir ../WendyTemplates        # use local templates
EOF
    exit 0
}

# ── Parse arguments ─────────────────────────────────────────────────

HOSTNAME=""
HOSTNAME_PROVIDED=false
WENDY="wendy"
TEMPLATES_DIR=""
TEMPLATES_DIR_PROVIDED=false
TEMPLATES_BRANCH="main"
SKIP_RUN=false
FILTER_TEMPLATE=""
FILTER_LANGUAGE=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--hostname)          HOSTNAME="$2"; HOSTNAME_PROVIDED=true; shift 2 ;;
        -w|--wendy)             WENDY="$2"; shift 2 ;;
        --templates-dir)        TEMPLATES_DIR="$2"; TEMPLATES_DIR_PROVIDED=true; shift 2 ;;
        --templates-branch)     TEMPLATES_BRANCH="$2"; shift 2 ;;
        --skip-run)             SKIP_RUN=true; shift ;;
        --template)             FILTER_TEMPLATE="$2"; shift 2 ;;
        --language)             FILTER_LANGUAGE="$2"; shift 2 ;;
        --help)                 usage ;;
        *)                      echo "Unknown option: $1"; usage ;;
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

if ! require_tool jq; then
    echo -e "${RED}ERROR: jq is required${RESET}"
    exit 1
fi
echo ""

# ── Phase 2: Acquire templates repo ────────────────────────────────

echo -e "${BOLD}==> Phase 2: Acquire templates repo${RESET}"

CLONE_DIR=""
if [[ "$TEMPLATES_DIR_PROVIDED" == true ]]; then
    if [[ ! -d "$TEMPLATES_DIR" ]]; then
        echo -e "${RED}ERROR: Templates directory not found: $TEMPLATES_DIR${RESET}"
        exit 1
    fi
    TEMPLATES_DIR="$(cd "$TEMPLATES_DIR" && pwd)"
    echo "Using local templates: $TEMPLATES_DIR"
else
    CLONE_DIR=$(mktemp -d)
    echo "Cloning wendylabsinc/templates ($TEMPLATES_BRANCH) into $CLONE_DIR..."
    git clone --depth 1 --branch "$TEMPLATES_BRANCH" \
        https://github.com/wendylabsinc/templates.git "$CLONE_DIR" 2>&1 | tail -1
    TEMPLATES_DIR="$CLONE_DIR"
fi

META_JSON="$TEMPLATES_DIR/meta.json"
if [[ ! -f "$META_JSON" ]]; then
    echo -e "${RED}ERROR: meta.json not found at $META_JSON${RESET}"
    exit 1
fi
echo ""

# ── Phase 3: Load template registry ────────────────────────────────

echo -e "${BOLD}==> Phase 3: Load template registry${RESET}"

TEMPLATES=()
while IFS= read -r name; do
    TEMPLATES+=("$name")
done < <(jq -r '.templates[].name' "$META_JSON")

LANGUAGES=()
while IFS= read -r key; do
    LANGUAGES+=("$key")
done < <(jq -r '.languages[].key' "$META_JSON")

# Apply filters
if [[ -n "$FILTER_TEMPLATE" ]]; then
    found=false
    for t in "${TEMPLATES[@]}"; do
        if [[ "$t" == "$FILTER_TEMPLATE" ]]; then found=true; break; fi
    done
    if [[ "$found" != true ]]; then
        echo -e "${RED}ERROR: Unknown template '$FILTER_TEMPLATE'. Available: ${TEMPLATES[*]}${RESET}"
        exit 1
    fi
    TEMPLATES=("$FILTER_TEMPLATE")
fi

if [[ -n "$FILTER_LANGUAGE" ]]; then
    found=false
    for l in "${LANGUAGES[@]}"; do
        if [[ "$l" == "$FILTER_LANGUAGE" ]]; then found=true; break; fi
    done
    if [[ "$found" != true ]]; then
        echo -e "${RED}ERROR: Unknown language '$FILTER_LANGUAGE'. Available: ${LANGUAGES[*]}${RESET}"
        exit 1
    fi
    LANGUAGES=("$FILTER_LANGUAGE")
fi

echo "Templates: ${TEMPLATES[*]}"
echo "Languages: ${LANGUAGES[*]}"
echo "Combinations: $(( ${#TEMPLATES[@]} * ${#LANGUAGES[@]} ))"
echo ""

# ── Phase 4: Generate projects ─────────────────────────────────────

echo -e "${BOLD}==> Phase 4: Generate projects${RESET}"

WORK_DIR=$(mktemp -d)
# Clean up both temp dirs on exit
trap 'rm -rf "$WORK_DIR" ${CLONE_DIR:+"$CLONE_DIR"}' EXIT
echo "Working directory: $WORK_DIR"
echo ""

for lang in "${LANGUAGES[@]}"; do
    echo -e "${BOLD}--- $lang ---${RESET}"
    mkdir -p "$WORK_DIR/$lang"

    for tmpl in "${TEMPLATES[@]}"; do
        app_id="test-${lang}-${tmpl}"
        test_name="${lang}/${tmpl}"

        # Some templates only exist for a subset of languages (e.g.
        # camera-feed-yolo is python-only). Skip combinations that aren't in
        # the templates repo rather than counting them as failures.
        if [[ ! -f "$TEMPLATES_DIR/$lang/$tmpl/template.json" ]]; then
            skip_test "init $test_name (not available for $lang)"
            continue
        fi

        run_test "init $test_name" \
            bash -c "cd '$WORK_DIR/$lang' && '$WENDY' init \
                --app-id '$app_id' \
                --template '$tmpl' \
                --language '$lang' \
                --target wendyos \
                --assistant skip \
                --git-init no"
    done
    echo ""
done

# ── Phase 5: Validate generated projects ───────────────────────────

echo -e "${BOLD}==> Phase 5: Validate generated projects${RESET}"

for lang in "${LANGUAGES[@]}"; do
    for tmpl in "${TEMPLATES[@]}"; do
        app_id="test-${lang}-${tmpl}"
        project_dir="$WORK_DIR/$lang/$app_id"
        test_name="${lang}/${tmpl}"

        if [[ ! -f "$TEMPLATES_DIR/$lang/$tmpl/template.json" ]]; then
            skip_test "validate $test_name (not available for $lang)"
            continue
        fi

        if [[ ! -f "$project_dir/wendy.json" ]]; then
            skip_test "validate $test_name (no wendy.json)"
            continue
        fi

        run_test "validate $test_name" \
            bash -c "jq -e '.appId == \"$app_id\"' '$project_dir/wendy.json' >/dev/null"
    done
done
echo ""

# ── Phase 6: Deploy to device (optional) ──────────────────────────

echo -e "${BOLD}==> Phase 6: Deploy to device${RESET}"

DEPLOY_RC=0
if [[ "$SKIP_RUN" == true ]]; then
    echo "Skipping device deployment (--skip-run)"
    echo ""
else
    wendyos_args=(--samples-dir "$WORK_DIR")
    if [[ -n "$HOSTNAME" ]]; then
        wendyos_args+=(--hostname "$HOSTNAME")
    fi
    wendyos_args+=(--wendy "$WENDY")

    echo "Delegating to test-wendyos.sh ${wendyos_args[*]}"
    echo ""
    "$SCRIPT_DIR/test-wendyos.sh" "${wendyos_args[@]}"
    DEPLOY_RC=$?
fi

# ── Phase 7: Summary ──────────────────────────────────────────────

echo -e "${BOLD}==> Init + Validate Summary${RESET}"
print_summary
init_rc=$?

if [[ "$SKIP_RUN" != true ]]; then
    if [[ $DEPLOY_RC -eq 0 ]]; then
        echo -e "  ${GREEN}Deploy phase: PASSED${RESET}"
    else
        echo -e "  ${RED}Deploy phase: FAILED${RESET}"
    fi
fi

if [[ $DEPLOY_RC -ne 0 ]]; then
    exit 1
fi
exit $init_rc
