#!/usr/bin/env bash
# Extend the existing Linux media job. The fixture has no loopback node to open.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
output="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wendy-sensorlink-capture"
mkdir -p "$output"
export WENDY_GSTREAMER_REQUIRED=1
test_name=TestSensorLinkSubscribersDecodeWithoutOpeningLoopback
go test -race -c -o "$output/services.test" ./go/internal/agent/services
trap 'rm -f "$output/services.test"' EXIT
strace -f -e trace=open,openat,openat2 -o "$output/opens.log" \
  "$output/services.test" -test.run "^${test_name}$" -test.v -test.count=1 -test.timeout=90s | tee "$output/normal.log"
grep -q "^--- PASS: $test_name " "$output/normal.log"
if grep -Eq '(open|openat|openat2)\([^\"]*"/dev/video96"' "$output/opens.log"; then
  echo 'SensorLink capture opened its loopback node' >&2
  exit 1
fi
# Prove that the trace actually observed the test process and codec children.
grep -q 'libgst' "$output/opens.log"
if grep -q '^--- SKIP:' "$output/normal.log"; then
  echo 'The required SensorLink capture regression was skipped' >&2
  exit 1
fi
rm -f "$output/services.test"

python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1]).resolve()
source = Path('go/internal/agent/services/video_service.go').resolve()
original = source.read_text()
before = '} else if s.sensorFrames != nil && isSensorLinkCameraPath(path) {'
if original.count(before) != 1:
    raise SystemExit('SensorLink source mutation no longer identifies exactly one site')
replacement = out / 'open-loopback.go'
replacement.write_text(original.replace(before, '} else if false && s.sensorFrames != nil && isSensorLinkCameraPath(path) {'))
(out / 'open-loopback.json').write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
if go test -race -overlay "$output/open-loopback.json" ./go/internal/agent/services -run "^${test_name}$" -v -count=1 -timeout 90s > "$output/open-loopback.log" 2>&1; then
  cat "$output/open-loopback.log"
  echo 'Regression did not reject capturing through the loopback node' >&2
  exit 1
fi
cat "$output/open-loopback.log"
grep -q "^--- FAIL: $test_name " "$output/open-loopback.log"
grep -q 'SensorLink subscribers did not receive decodable frames:' "$output/open-loopback.log"
