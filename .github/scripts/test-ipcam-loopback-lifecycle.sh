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
  local binary="$1" selection="$2" label="$3" mode="${4:-}"
  # The existing guardian drains the exact test process group. Bound each
  # native GStreamer debug file to 16 MiB; stdout remains the normal test log.
  # Only v4l2/basesink categories are enabled, never credential-bearing RTSP.
  sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted WENDY_IPCAM_LIFECYCLE_LOOPBACK=1 WENDY_GSTREAMER_REQUIRED=1 \
    GST_DEBUG_NO_COLOR=1 GST_DEBUG='v4l2*:5,basesink:5' GST_DEBUG_FILE="$output/$label-producer.log" \
    WENDY_IPCAM_DELIVERY_DIAGNOSTIC="$mode" \
    bash -c 'ulimit -f 16384; exec "$@"' bash \
    python3 .github/scripts/run-owned-media-test.py --timeout "${5:-95}" -- \
    "$binary" -test.run "$selection" -test.v -test.count=1 -test.timeout=0
}
normal_status=0
run_fixture "$output/services.test" "^($protocol_test|$kernel_test)$" normal | tee "$output/normal.log" || normal_status=$?
if (( normal_status != 0 )); then
  # Only an ordinary reported test failure with its node already removed is a
  # known drain. A watchdog/interruption/cleanup uncertainty cannot launch a
  # second writer or remove an unknown node. Keep the original failure code.
  if (( normal_status == 1 )) && [[ ! -e /dev/video200 && ! -L /dev/video200 ]]; then
    for mode in consumer-unsynced producer-write; do
      diagnostic_status=0
      run_fixture "$output/services.test" '^TestIPCameraKernelDeliveryDiagnostic$' "$mode" "$mode" 25 \
        > "$output/$mode.log" 2>&1 || diagnostic_status=$?
      cat "$output/$mode.log"
      if (( diagnostic_status >= 124 )) || [[ -e /dev/video200 || -L /dev/video200 ]]; then
        echo 'IP-camera diagnostic drain is uncertain; refusing further writers.' >&2
        break
      fi
    done
  else
    echo 'Skipping IP-camera alternatives because normal drain is uncertain.' >&2
  fi
  # These are synthetic credentials; retain small useful tails in the job log
  # and full size-bounded files in its existing artifact for source diagnosis.
  for log in "$output/"*-producer.log; do
    [[ -f "$log" ]] || continue
    printf '\nGStreamer producer diagnostic: %s\n' "${log##*/}"
    tail -c 32768 "$log"
  done
  exit "$normal_status"
fi
for test_name in "$protocol_test" "$kernel_test"; do grep -q "^--- PASS: $test_name " "$output/normal.log"; done
if grep -q '^--- SKIP:' "$output/normal.log"; then exit 1; fi
if run_fixture "$output/ignore-container-demand.test" "^$kernel_test$" ignore-container-demand > "$output/ignore-container-demand.log" 2>&1; then
  cat "$output/ignore-container-demand.log"
  echo 'Kernel lifecycle accepted loss of container-only demand.' >&2
  exit 1
fi
cat "$output/ignore-container-demand.log"
grep -q "^--- FAIL: $kernel_test " "$output/ignore-container-demand.log"
grep -q 'container-only demand after idle-grace duration lost or duplicated the shared pump' "$output/ignore-container-demand.log"
echo "IP-camera lifecycle logs: $output"
