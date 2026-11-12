#!/usr/bin/env bash
# LAST kernel-media fixture, immediately before the job's always() cleanup of
# its owned v4l2loopback module. Loads/unloads no modules. A failed/watchdog-killed
# process is drained by the guardian before that module cleanup removes nodes.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != true || "${RUNNER_ENVIRONMENT:-}" != github-hosted || "$(uname -s)" != Linux ]]; then
  echo 'IP-camera lifecycle fixture requires the disposable GitHub-hosted Linux media runner.' >&2
  exit 2
fi
cd "$(git rev-parse --show-toplevel)"
if [[ ! -c /dev/v4l2loopback || "$(cat /sys/module/v4l2loopback/version)" != 0.15.4 ]]; then
  echo 'IP-camera lifecycle fixture requires the job-owned v4l2loopback 0.15.4 module.' >&2
  exit 2
fi
if [[ -e /dev/video200 || -L /dev/video200 ]]; then
  echo 'Refusing to reserve an existing video200 node.' >&2
  exit 2
fi
for element in v4l2src v4l2sink rtspsrc rtph264depay h264parse avdec_h264 x264enc jpegparse jpegdec videoconvert fdsink; do gst-inspect-1.0 "$element" >/dev/null; done
output="$(mktemp -d "${RUNNER_TEMP:?}/wendy-ipcam-lifecycle.XXXXXX")"
trap 'rm -f "$output/"*.test' EXIT
go test -race -tags regression_camera_fixture -c -o "$output/services.test" ./go/internal/agent/services
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out = Path(sys.argv[1])
source = Path('go/internal/agent/ipcam/loopback.go').resolve()
original = source.read_text()
before = 'demand := len(l.containerOwners) > 0 || l.viewRefs[camID] > 0'
if original.count(before) != 1:
    raise SystemExit('container demand mutation no longer identifies one site')
replacement = out / 'ignore-container-demand.go'
replacement.write_text(original.replace(before, 'demand := l.viewRefs[camID] > 0'))
(out / 'ignore-container-demand.json').write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
go test -race -tags regression_camera_fixture -overlay "$output/ignore-container-demand.json" -c -o "$output/ignore-container-demand.test" ./go/internal/agent/services
kernel_test=TestIPCameraKernelPumpRetainsConsumersAndStopsAfterFinalRelease
protocol_test=TestIPCameraAuthenticatedStreamChangesDecodedPixels
run_fixture() {
  sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted WENDY_IPCAM_LIFECYCLE_LOOPBACK=1 WENDY_GSTREAMER_REQUIRED=1 \
    python3 .github/scripts/run-owned-media-test.py --timeout 95 -- \
    "$1" -test.run "$2" -test.v -test.count=1 -test.timeout=0
}
run_fixture "$output/services.test" "^($protocol_test|$kernel_test)$" | tee "$output/normal.log"
for test_name in "$protocol_test" "$kernel_test"; do grep -q "^--- PASS: $test_name " "$output/normal.log"; done
if grep -q '^--- SKIP:' "$output/normal.log"; then exit 1; fi
if run_fixture "$output/ignore-container-demand.test" "^$kernel_test$" > "$output/ignore-container-demand.log" 2>&1; then
  cat "$output/ignore-container-demand.log"
  echo 'Kernel lifecycle accepted loss of container-only demand.' >&2
  exit 1
fi
cat "$output/ignore-container-demand.log"
grep -q "^--- FAIL: $kernel_test " "$output/ignore-container-demand.log"
grep -q 'container-only demand after idle-grace duration lost or duplicated the shared pump' "$output/ignore-container-demand.log"
echo "IP-camera lifecycle logs: $output"
