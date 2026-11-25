#!/usr/bin/env bash
# Runs the real E2EAnalyze scripts against stub stage scripts, so it needs neither
# a Swift toolchain nor review credentials. E2EAnalyze.ps1 is checked when pwsh is
# installed, as it is on GitHub's hosted runners.
set -euo pipefail

SOURCE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

SCRIPTS="$TEST_DIR/swift/Scripts"
OUTPUT_DIR="$TEST_DIR/output"
RUN_ID="swift-e2e-tests.fixture"
export REPORT_LOG="$TEST_DIR/report.log"

mkdir -p "$SCRIPTS" "$TEST_DIR/swift/WendyE2ETests" "$TEST_DIR/bin"
cp "$SOURCE_DIR/E2EAnalyze.sh" "$SOURCE_DIR/E2EAnalyze.ps1" "$SCRIPTS/"

for run in a b; do
  attempt="$OUTPUT_DIR/$RUN_ID.$run.local.0001"
  mkdir -p "$attempt"
  printf '{}\n' > "$attempt/attempt.json"
done

# E2EAnalyze.ps1 aggregates through `swift run`, not a sibling script.
printf '#!/bin/sh\nexit 0\n' > "$TEST_DIR/bin/swift"
printf '#!/bin/sh\nexit 0\n' > "$SCRIPTS/E2EAggregate.sh"
chmod +x "$TEST_DIR/bin/swift"
export PATH="$TEST_DIR/bin:$PATH"

cat > "$SCRIPTS/E2EReview.sh" <<'STUB'
#!/usr/bin/env bash
[[ "${2##*/}" != "$FAILING_RUN" ]] || exit 7
STUB

cat > "$SCRIPTS/E2EReport.sh" <<'STUB'
#!/usr/bin/env bash
mkdir -p "$2"
touch "$2/index.html"
printf '%s %s\n' "${2##*/}" "${WENDY_E2E_REVIEW_OUTCOME:-unset}" >> "$REPORT_LOG"
STUB

cat > "$SCRIPTS/E2EReview.ps1" <<'STUB'
if ((Split-Path -Leaf $args[1]) -eq $env:FAILING_RUN) { exit 7 }
exit 0
STUB

cat > "$SCRIPTS/E2EReport.ps1" <<'STUB'
New-Item -ItemType Directory -Force -Path $args[1] | Out-Null
New-Item -ItemType File -Force -Path (Join-Path $args[1] 'index.html') | Out-Null
$outcome = if ($env:WENDY_E2E_REVIEW_OUTCOME) { $env:WENDY_E2E_REVIEW_OUTCOME } else { 'unset' }
Add-Content -LiteralPath $env:REPORT_LOG "$(Split-Path -Leaf $args[1]) $outcome"
exit 0
STUB

failures=0

fail() {
  echo "FAIL: $1" >&2
  failures=$((failures + 1))
}

# check NAME STAGE FAILING_RUN EXPECTED_REPORTS EXPECT_FAILURE -- COMMAND...
check() {
  local name="$1" stage="$2" failing_run="$3" expected="$4" expect_failure="$5"
  shift 6
  local status=0
  : > "$REPORT_LOG"
  FAILING_RUN="$failing_run" "$@" --output-dir "$OUTPUT_DIR" --run-id "$RUN_ID" \
    --stage "$stage" --no-open > "$TEST_DIR/output.log" 2>&1 || status=$?
  if [[ "$(sort "$REPORT_LOG")" != "$expected" ]]; then
    fail "$name: expected reports [$expected], got [$(sort "$REPORT_LOG")]"
    cat "$TEST_DIR/output.log" >&2
  fi
  if [[ "$expect_failure" == true && "$status" -eq 0 ]]; then
    fail "$name: expected a nonzero exit"
    cat "$TEST_DIR/output.log" >&2
  fi
}

run_cases() {
  local label="$1"
  shift
  local a_failed clean
  a_failed="$(printf '%s.a failure\n%s.b unset' "$RUN_ID" "$RUN_ID")"
  clean="$(printf '%s.a unset\n%s.b unset' "$RUN_ID" "$RUN_ID")"

  check "$label: a failed review marks only its own report" \
    all "$RUN_ID.a" "$a_failed" true -- "$@"
  # Exit status is not compared when the review succeeds: E2EAnalyze.sh's report
  # lookup stops with status 1 under GNU stat whatever the review did.
  check "$label: a finished review leaves the variable unset" \
    all none "$clean" false -- "$@"
  check "$label: a report stage on its own knows of no failure" \
    report "$RUN_ID.a" "$clean" false -- "$@"
}

run_cases E2EAnalyze.sh bash "$SCRIPTS/E2EAnalyze.sh"

if command -v pwsh > /dev/null; then
  run_cases E2EAnalyze.ps1 pwsh -NoLogo -NoProfile -File "$SCRIPTS/E2EAnalyze.ps1"
else
  echo "Skipped E2EAnalyze.ps1: pwsh is not installed"
fi

if [[ "$failures" -ne 0 ]]; then
  echo "$failures E2EAnalyze check(s) failed" >&2
  exit 1
fi
echo "E2EAnalyze checks passed"
