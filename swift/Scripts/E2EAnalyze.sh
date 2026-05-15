#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SWIFT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

RUN_DIR=""

usage() {
  cat <<EOF
Usage: $(basename "$0") --run-dir DIR

Stub Anthropic/Claude AI analysis step for WendyAgent Swift E2E run artifacts.

Options:
  --run-dir DIR  Required E2E run directory produced by E2ETest.sh.
  --help         Show this help message.
EOF
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
    *)
      printf "%s" "$path"
      ;;
  esac
}

absolute_existing_dir_path() {
  local path
  path="$(expand_local_path "$1")"
  (cd "$path" && pwd)
}

update_readme_block() {
  local readme_path="$1"
  local start="<!-- swift-e2e-analyze:start -->"
  local end="<!-- swift-e2e-analyze:end -->"
  local tmp_path
  tmp_path="$(mktemp)"

  if [[ -f "$readme_path" ]]; then
    awk -v start="$start" -v end="$end" '
      $0 == start { skipping = 1; next }
      $0 == end { skipping = 0; next }
      skipping != 1 { print }
    ' "$readme_path" > "$tmp_path"
  else
    : > "$tmp_path"
  fi

  {
    cat "$tmp_path"
    if [[ -s "$tmp_path" ]]; then
      printf '\n'
    fi
    echo "$start"
    echo "## AI Analysis"
    echo
    echo "- Status: \`skipped\`"
    echo "- Markdown: \`$RUN_DIR/ai-analysis.md\`"
    echo "- JSON: \`$RUN_DIR/ai-analysis.json\`"
    echo "$end"
  } > "$readme_path"

  rm -f "$tmp_path"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --run-dir)
      RUN_DIR="$2"
      shift 2
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

if [[ -z "$RUN_DIR" ]]; then
  echo "ERROR: --run-dir is required." >&2
  usage >&2
  exit 64
fi

RUN_DIR="$(absolute_existing_dir_path "$RUN_DIR")"
README_PATH="$RUN_DIR/README.md"
AI_ANALYSIS_MD="$RUN_DIR/ai-analysis.md"
AI_ANALYSIS_JSON="$RUN_DIR/ai-analysis.json"

recording_count="0"
if [[ -d "$RUN_DIR/tests" ]]; then
  recording_count="$(find "$RUN_DIR/tests" -path '*/recording.md' -type f | wc -l | tr -d '[:space:]')"
fi

cat > "$AI_ANALYSIS_MD" <<EOF
# Swift E2E AI Analysis

Anthropic/Claude AI analysis skipped: E2EAnalyze.sh is currently a no-op stub.

- Run directory: \`$RUN_DIR\`
- Info: \`$RUN_DIR/info.json\`
- Test results: \`$RUN_DIR/test-results-swift-testing.xml\`
- Recording count: \`$recording_count\`
EOF

cat > "$AI_ANALYSIS_JSON" <<EOF
{
  "status": "pass",
  "summary": "Anthropic/Claude AI analysis skipped: E2EAnalyze.sh is currently a no-op stub.",
  "findings": []
}
EOF

update_readme_block "$README_PATH"

echo "==> Swift E2E Anthropic/Claude AI analysis skipped (stub)"
echo "    Run dir: $RUN_DIR"
echo "    Markdown: $AI_ANALYSIS_MD"
echo "    JSON: $AI_ANALYSIS_JSON"
