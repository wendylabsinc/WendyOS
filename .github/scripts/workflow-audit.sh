#!/usr/bin/env bash
# Workflow trust-boundary gate: regression tests, then a blocking zizmor audit.
# Both CI gates and local runs use this script; see .github/CI_SECURITY.md.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"

python3 .github/scripts/ci_security_test.py

if [[ -n "${RUNNER_TEMP:-}" ]]; then
  venv="$RUNNER_TEMP/workflow-audit"
else
  venv="$(mktemp -d)"
  trap 'rm -rf "$venv"' EXIT
fi
python3 -m venv --clear "$venv"
"$venv/bin/pip" install --disable-pip-version-check --quiet \
  --require-hashes --only-binary=:all: \
  -r .github/scripts/workflow-audit-requirements.txt

# --no-config and --no-ignores stop config files and inline comments from
# suppressing findings in this gate.
"$venv/bin/zizmor" --offline --no-config --no-ignores \
  --min-severity high --min-confidence high .github
