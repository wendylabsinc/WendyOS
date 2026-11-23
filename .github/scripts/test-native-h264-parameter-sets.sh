#!/usr/bin/env bash
# Run in the existing Linux media job; no camera, display or privileged process.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
output="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wendy-native-h264-parameter-sets"
mkdir -p "$output"
export WENDY_GSTREAMER_REQUIRED=1
test_name=TestNativeH264SendOnceStreamDecodesForLateViewerAndRecording
go test -race ./go/internal/agent/services -run "^${test_name}$" -v -count=1 -timeout 120s | tee "$output/normal.log"
grep -q "^--- PASS: $test_name " "$output/normal.log"
# Both arms must have run: the late joiners that decode, and the control that
# shows what they get when the camera's buffers are forwarded as they came.
grep -q "^    --- PASS: $test_name/with_reinsertion " "$output/normal.log"
grep -q "^    --- PASS: $test_name/without_reinsertion " "$output/normal.log"
if grep -Eq '^ *--- SKIP:' "$output/normal.log"; then
  echo 'The required native H.264 parameter-set regression was skipped' >&2
  exit 1
fi

# Forward a keyframe that lacks parameter sets as the camera sent it, which is
# what the native path did before it remembered them.
mutation=forward-keyframes-as-sent
python3 - "$output" "$mutation" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1]).resolve()
name = sys.argv[2]
source = Path('go/internal/agent/services/video_h264_parameter_sets.go').resolve()
original = source.read_text()
before, after = 'return p.insert(au, insertAt)', 'return au, h264Unchanged'
if original.count(before) != 1:
    raise SystemExit(f'{name}: source mutation no longer identifies exactly one site')
replacement = out / (name + '.go')
replacement.write_text(original.replace(before, after))
(out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY

if go test -race -overlay "$output/$mutation.json" ./go/internal/agent/services -run "^${test_name}$" -v -count=1 -timeout 120s > "$output/$mutation.log" 2>&1; then
  cat "$output/$mutation.log"
  echo "Regression did not reject $mutation" >&2
  exit 1
fi
cat "$output/$mutation.log"
grep -q "^--- FAIL: $test_name " "$output/$mutation.log"
# The arm that decodes is the one that must fail, at both late joiners. The
# control must still pass: it never depended on the product repeating anything.
grep -q "^    --- FAIL: $test_name/with_reinsertion " "$output/$mutation.log"
grep -q 'late viewer decoded 0 frames, want ' "$output/$mutation.log"
grep -q 'late recording did not start: ' "$output/$mutation.log"
grep -q "^    --- PASS: $test_name/without_reinsertion " "$output/$mutation.log"
