#!/usr/bin/env bash
# Audit 1736: actual Go watch -> Swift file sync/native process/telemetry.
set -euo pipefail
[[ $(uname -s) == Darwin ]] || { echo 'This fixture requires the native macOS agent runtime.' >&2; exit 2; }
[[ $# == 2 || $# == 3 ]] || { echo 'usage: test-watch-session.sh NEW_OUTPUT_DIRECTORY SWIFT_SCRATCH_DIRECTORY [default|native]' >&2; exit 2; }
build_system=${3:-default}
swift_build=(swift build)
swift_test=(swift test)
case "$build_system" in
  default) ;;
  native) swift_build+=(--build-system native); swift_test+=(--build-system native) ;;
  *) echo 'unsupported Swift build system' >&2; exit 2 ;;
esac
mkdir "$1"
output=$(cd "$1" && pwd -P)
mkdir -p "$2"
scratch=$(cd "$2" && pwd -P)
root=$(pwd -P)
guardian="$root/.github/scripts/run-owned-media-test.py"
package="$root/swift/WendyAgentCore"

python3 "$guardian" --timeout 600 -- go build -o "$output/wendy" ./go/cmd/wendy
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1])
source = Path('go/internal/cli/commands/watch.go').resolve()
original = source.read_text()
before = 'func withWatchInvariants(opts runOptions) runOptions {\n\topts.yes = true\n'
assert original.count(before) == 1, 'watch invariant site changed'
# Exact old behavior in the parent of fix ec672304a (PR1736).
mutant = out / 'forced-detach.go'
mutant.write_text(original.replace(before, before.replace('\topts.yes', '\topts.detach = true\n\topts.yes')))
(out / 'forced-detach.json').write_text(json.dumps({'Replace': {str(source): str(mutant)}}))
PY
python3 "$guardian" --timeout 600 -- go build -overlay="$output/forced-detach.json" -o "$output/wendy-forced-detach" ./go/cmd/wendy
# No plugin-trust bypass, no global package/cache edits. The caller owns scratch.
python3 "$guardian" --timeout 600 -- "${swift_build[@]}" --package-path "$package" --scratch-path "$scratch" --build-tests
run_fixture() {
  python3 "$guardian" --timeout 150 -- env WENDY_WATCH_CLI="$1" \
    "${swift_test[@]}" --package-path "$package" --scratch-path "$scratch" --skip-build \
      --filter WatchSessionTransportTests
}
run_fixture "$output/wendy" 2>&1 | tee "$output/normal.log"
grep -q 'WATCH_SESSION_VERIFIED ' "$output/normal.log"
grep -q 'Test run with 1 test in 1 suite passed' "$output/normal.log"
if grep -qi 'skipped' "$output/normal.log"; then exit 1; fi
if run_fixture "$output/wendy-forced-detach" > "$output/forced-detach.log" 2>&1; then
  cat "$output/forced-detach.log"
  echo 'Forced-detach regression unexpectedly passed' >&2
  exit 1
fi
cat "$output/forced-detach.log"
grep -q 'watch boundary failed: initial native output' "$output/forced-detach.log"
grep -q 'running in detached mode' "$output/forced-detach.log"
grep -q 'Test run with 1 test in 1 suite failed' "$output/forced-detach.log"
if grep -q 'WATCH_SESSION_VERIFIED ' "$output/forced-detach.log"; then exit 1; fi
