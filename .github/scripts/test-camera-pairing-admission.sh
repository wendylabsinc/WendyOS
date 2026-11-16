#!/usr/bin/env bash
# Add to the existing media job; this tag exposes only local transport seams.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
output="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wendy-camera-pairing-admission"
mkdir -p "$output"
export WENDY_GSTREAMER_REQUIRED=1
test_name=TestCameraPairingControlsRealONVIFAndRTSPConnections
go test -race -tags=regression_camera_fixture ./go/internal/agent/services -run "^${test_name}$" -v -count=1 -timeout 60s | tee "$output/normal.log"
grep -q "^--- PASS: $test_name " "$output/normal.log"
if grep -q '^--- SKIP:' "$output/normal.log"; then
  echo 'The required camera admission regression was skipped' >&2
  exit 1
fi

python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1]).resolve()
source = Path('go/internal/agent/ipcam/discover.go').resolve()
original = source.read_text()
mutations = {
    'onvif-before-pairing': ('if !paired {', 'if false && !paired {'),
    'liveness-before-pairing': (
        'if _, paired := d.login(cam.MAC); !paired && !cam.DHCPServed {',
        'if _, paired := d.login(cam.MAC); false && !paired && !cam.DHCPServed {',
    ),
}
for name, (before, after) in mutations.items():
    if original.count(before) != 1:
        raise SystemExit(f'{name}: source mutation no longer identifies exactly one site')
    replacement = out / (name + '.go')
    replacement.write_text(original.replace(before, after))
    (out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY

for mutation in onvif-before-pairing liveness-before-pairing; do
  if go test -race -tags=regression_camera_fixture -overlay "$output/$mutation.json" ./go/internal/agent/services -run "^${test_name}$" -v -count=1 -timeout 60s > "$output/$mutation.log" 2>&1; then
    cat "$output/$mutation.log"
    echo "Regression did not reject $mutation" >&2
    exit 1
  fi
  cat "$output/$mutation.log"
  grep -q "^--- FAIL: $test_name " "$output/$mutation.log"
  grep -q 'unpaired camera opened network connections:' "$output/$mutation.log"
done
