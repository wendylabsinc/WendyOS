#!/bin/bash
# Verify that dependency licenses haven't changed from the committed baseline.
# Run with --update to regenerate licenses.csv instead of comparing.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BASELINE="$SCRIPT_DIR/licenses.csv"

CURRENT=$(go run github.com/google/go-licenses/v2@latest report \
    github.com/wendylabsinc/wendy/... \
    --ignore github.com/wendylabsinc/wendy \
    2>/dev/null | sort)

if [[ "${1:-}" == "--update" ]]; then
    echo "$CURRENT" > "$BASELINE"
    echo "licenses.csv updated."
    exit 0
fi

DIFF=$(diff <(sort "$BASELINE") <(echo "$CURRENT") || true)

if [[ -z "$DIFF" ]]; then
    echo "License check passed."
    exit 0
fi

echo "Dependency licenses have changed from the baseline in licenses.csv:"
echo ""
echo "$DIFF"
echo ""
echo "If this change is intentional, run: make licenses-update"
exit 1
