#!/usr/bin/env bash
# Use the v4l2loopback 0.15.4 module already loaded by the Linux media job.
# The test creates/removes only its own node below 96; it does not load any module.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != true || "${RUNNER_ENVIRONMENT:-}" != github-hosted || "$(uname -s)" != Linux ]]; then
  echo 'MJPEG camera pixels fixture requires the disposable GitHub-hosted Linux runner.' >&2
  exit 2
fi
cd "$(git rev-parse --show-toplevel)"
if [[ ! -c /dev/v4l2loopback ]]; then
  echo 'Expected the owned v4l2loopback control device at /dev/v4l2loopback; the media module setup did not complete.' >&2
  exit 2
fi
if ! module_version="$(cat /sys/module/v4l2loopback/version 2>/dev/null)"; then
  echo 'Cannot read the loaded v4l2loopback module version; refusing the kernel camera fixture.' >&2
  exit 2
fi
if [[ "$module_version" != 0.15.4 ]]; then
  printf 'Expected v4l2loopback module version 0.15.4; found <%s>. Build the exact pinned checkout without its .gitversion development marker before loading it.\n' "$module_version" >&2
  exit 2
fi
for element in v4l2src jpegdec videoconvert tee queue x264enc h264parse avdec_h264 fdsrc fdsink; do
  gst-inspect-1.0 "$element" > /dev/null
done
output="$(mktemp -d "${RUNNER_TEMP:?}/wendy-camera-pixels.XXXXXX")"
trap 'rm -f "$output/"*.test' EXIT
test_name=TestMJPEGCameraPixelsAndEncoderOnDemand
go test -race -tags regression_camera_fixture -c -o "$output/services.test" ./go/internal/agent/services

python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1]).resolve()
source = Path('go/internal/agent/services/video_pipeline_shape.go').resolve()
original = source.read_text()
# Every launch keeps the encoder, as before the producer read encoder demand.
before, after = 'pipelineShape{encode: demand.wantEncoded()}', 'pipelineShape{encode: true}'
if original.count(before) != 1:
    raise SystemExit('encoder-always: source mutation no longer identifies exactly one site')
replacement = out / 'encoder-always.go'
replacement.write_text(original.replace(before, after))
(out / 'encoder-always.json').write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
go test -race -tags regression_camera_fixture -overlay "$output/encoder-always.json" -c -o "$output/encoder-always.test" ./go/internal/agent/services

run_fixture() {
  sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted WENDY_GSTREAMER_REQUIRED=1 \
    python3 .github/scripts/run-owned-media-test.py --timeout 120 -- \
    "$1" -test.run "^${test_name}$" -test.v -test.count=1 -test.timeout=0
}
run_fixture "$output/services.test" | tee "$output/normal.log"
grep -q "^--- PASS: $test_name " "$output/normal.log"
if grep -q '^--- SKIP:' "$output/normal.log"; then
  echo 'The required MJPEG camera pixels regression was skipped' >&2
  exit 1
fi

if run_fixture "$output/encoder-always.test" > "$output/encoder-always.log" 2>&1; then
  cat "$output/encoder-always.log"
  echo 'Regression did not reject encoder-always' >&2
  exit 1
fi
cat "$output/encoder-always.log"
grep -q "^--- FAIL: $test_name " "$output/encoder-always.log"
grep -q 'pipeline is encoding with no encoded subscriber' "$output/encoder-always.log"
echo "MJPEG camera pixels fixture logs: $output"
