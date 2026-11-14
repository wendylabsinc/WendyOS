#!/usr/bin/env bash
# Run in the existing Linux media job; no camera, display or privileged process.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
output="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wendy-gstreamer-no-parser"
mkdir -p "$output"
export WENDY_GSTREAMER_REQUIRED=1
test_name=TestGStreamerX264WithoutParserDecodesForInitialAndLateViewers
go test -race ./go/internal/agent/services -run "^${test_name}$" -v -count=1 -timeout 90s | tee "$output/normal.log"
grep -q "^--- PASS: $test_name " "$output/normal.log"
if grep -q '^--- SKIP:' "$output/normal.log"; then
  echo 'The required missing-parser regression was skipped' >&2
  exit 1
fi

python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1]).resolve()
source = Path('go/internal/agent/services/video_service.go').resolve()
original = source.read_text()
mutations = {
    'prefer-vp8': ('if hasElem("x264enc") {', 'if false && hasElem("x264enc") {'),
    'omit-byte-stream': ('caps += ",stream-format=byte-stream,alignment=au"', 'caps += ""'),
}
for name, (before, after) in mutations.items():
    if original.count(before) != 1:
        raise SystemExit(f'{name}: source mutation no longer identifies exactly one site')
    replacement = out / (name + '.go')
    replacement.write_text(original.replace(before, after))
    (out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY

for mutation in prefer-vp8 omit-byte-stream; do
  case "$mutation" in
    prefer-vp8) expected='missing-parser host did not select H.264' ;;
    omit-byte-stream) expected='missing-parser output is not independently decodable Annex B' ;;
  esac
  if go test -race -overlay "$output/$mutation.json" ./go/internal/agent/services -run "^${test_name}$" -v -count=1 -timeout 90s > "$output/$mutation.log" 2>&1; then
    cat "$output/$mutation.log"
    echo "Regression did not reject $mutation" >&2
    exit 1
  fi
  cat "$output/$mutation.log"
  grep -q "^--- FAIL: $test_name " "$output/$mutation.log"
  grep -q "$expected" "$output/$mutation.log"
done
