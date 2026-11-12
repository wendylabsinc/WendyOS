#!/usr/bin/env bash
# Extend the existing media job with actual app-owned HTTP/GStreamer capture.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
output="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wendy-shared-camera-capture"
mkdir -p "$output"
export WENDY_GSTREAMER_REQUIRED=1
test_name=TestSharedCameraSubscribersDecodeFreshOwnerPixelsWithoutCameraOpen
go test -race -c -o "$output/services.test" ./go/internal/agent/services
trap 'rm -f "$output/services.test"' EXIT
if [[ "$(uname -s)" == Linux ]]; then
  # The reopen control must fail on the absent camera, not a missing plugin.
  gst-inspect-1.0 v4l2src > "$output/v4l2src.log"
  strace -f -e trace=open,openat,openat2 -o "$output/opens.log" \
    "$output/services.test" -test.run "^${test_name}$" -test.v -test.count=1 -test.timeout=90s | tee "$output/normal.log"
  if grep -Eq '(open|openat|openat2)\([^\"]*"/dev/video[0-9]+"' "$output/opens.log"; then
    echo 'Shared app-owned capture opened a physical camera' >&2
    exit 1
  fi
  grep -q 'libgst' "$output/opens.log"
else
  "$output/services.test" -test.run "^${test_name}$" -test.v -test.count=1 -test.timeout=90s | tee "$output/normal.log"
  echo 'Physical-camera syscall assertion requires the existing Linux media job.'
fi
grep -q "^--- PASS: $test_name " "$output/normal.log"
if grep -q '^--- SKIP:' "$output/normal.log"; then
  echo 'The required shared-camera capture regression was skipped' >&2
  exit 1
fi
rm -f "$output/services.test"

python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1]).resolve()
source = Path('go/internal/agent/services/camera_shared_http.go').resolve()
original = source.read_text()
controls = {
    'reopen-camera': ('stages := []string{"fdsrc fd=0 do-timestamp=true",',
                      'stages := []string{fmt.Sprintf("v4l2src device=%s", path),'),
    'repeat-old-pixels': ('\t\t\tframe = next\n', '\t\t\tnext.data = first.data\n\t\t\tframe = next\n'),
}
for name, (before, after) in controls.items():
    if original.count(before) != 1:
        raise SystemExit(f'{name} no longer identifies exactly one source site')
    replacement = out / f'{name}.go'
    replacement.write_text(original.replace(before, after))
    (out / f'{name}.json').write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
for control in reopen-camera repeat-old-pixels; do
  if go test -race -overlay "$output/$control.json" ./go/internal/agent/services \
      -run "^${test_name}$" -v -count=1 -timeout 90s > "$output/$control.log" 2>&1; then
    cat "$output/$control.log"
    echo "Shared-camera regression did not reject $control" >&2
    exit 1
  fi
  cat "$output/$control.log"
  grep -q "^--- FAIL: $test_name " "$output/$control.log"
done
grep -q 'app-owned subscribers did not receive decodable frames:' "$output/reopen-camera.log"
grep -q 'decoded stale/incorrect image' "$output/repeat-old-pixels.log"
