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

# Exercise the full wrapper chain with separate tooling and measured checkouts.
# Only the final Swift executable is replaced, so a missing hop is visible.
for script in E2EAggregate.sh E2EReview.{sh,ps1} E2EReport.{sh,ps1} E2ESanitizeXUnit.{sh,ps1}; do
  cp "$SOURCE_DIR/$script" "$SCRIPTS/"
done
export TOOLING_PACKAGE="$TEST_DIR/swift/WendyE2ETests"
export MEASURED_REPO="$TEST_DIR/tested source"
mkdir -p "$MEASURED_REPO"
# Keep report selection deterministic across BSD and GNU stat.
printf '#!/bin/sh\necho 100\n' > "$TEST_DIR/bin/stat"
chmod +x "$TEST_DIR/bin/stat"
cat > "$TEST_DIR/bin/swift" <<'STUB'
#!/usr/bin/env python3
import os
import sys
from pathlib import Path

command, product, stage, *args = sys.argv[1:]
assert (command, product) == ("run", "swift-e2e-testing")
assert Path.cwd().samefile(os.environ["TOOLING_PACKAGE"])
has_repo = os.environ["WITH_REPO"] == "yes"
assert args.count("--repo") == int(has_repo), (stage, args)
if has_repo:
    tested = Path(args[args.index("--repo") + 1])
    assert tested.is_absolute() and tested.samefile(os.environ["MEASURED_REPO"])
uses_diff = stage in ("aggregate", "review")
assert args.count("--diff") == int(uses_diff), (stage, args)
if uses_diff:
    assert args[args.index("--diff") + 1] == "base...tested"
with open(os.environ["REPORT_LOG"], "a") as log:
    print(stage, file=log)
STUB

check_forwarding() {
  local label="$1"
  shift
  local WITH_REPO status
  for WITH_REPO in yes no; do
    export WITH_REPO
    local repo_args=()
    [[ "$WITH_REPO" == no ]] || repo_args=(--repo "tested source")
    : > "$REPORT_LOG"
    # Existing reports from run_cases also exercise the normal report lookup.
    status=0
    (cd "$TEST_DIR" && "$@" --stage all --diff base...tested "${repo_args[@]}" \
      --output-dir "$OUTPUT_DIR" --run-id "$RUN_ID" --no-open) > "$TEST_DIR/output.log" 2>&1 || status=$?
    if [[ "$status" -ne 0 || "$(cat "$REPORT_LOG")" != "$(printf 'aggregate\nreview\nreview\nreport\nreport')" ]]; then
      fail "$label: stages did not receive the expected arguments (with repo: $WITH_REPO)"
      cat "$TEST_DIR/output.log" >&2
    fi
  done
}

output_alias="$TEST_DIR/output alias"
ln -s "$OUTPUT_DIR" "$output_alias"
OUTPUT_DIR="$output_alias" check_forwarding E2EAnalyze.sh bash "$SCRIPTS/E2EAnalyze.sh"
if command -v pwsh > /dev/null; then
  check_forwarding E2EAnalyze.ps1 pwsh -NoLogo -NoProfile -File "$SCRIPTS/E2EAnalyze.ps1"
fi

# Exercise run discovery through the same alias when only the saved report matches.
mv "$OUTPUT_DIR/$RUN_ID.a" "$OUTPUT_DIR/$RUN_ID.saved"
: > "$REPORT_LOG"
if ! WITH_REPO=no bash "$SCRIPTS/E2EAnalyze.sh" --output-dir "$output_alias" \
  --run-id "$RUN_ID.saved" --stage report --no-open > "$TEST_DIR/output.log" 2>&1 \
  || [[ "$(cat "$REPORT_LOG")" != report ]]; then
  fail "E2EAnalyze.sh: did not render the saved run through the output symlink"
  cat "$TEST_DIR/output.log" >&2
fi

if [[ "$failures" -ne 0 ]]; then
  echo "$failures E2EAnalyze check(s) failed" >&2
  exit 1
fi
echo "E2EAnalyze checks passed"
