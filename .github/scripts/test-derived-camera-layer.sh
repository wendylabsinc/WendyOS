#!/usr/bin/env bash
# Extend the existing Linux media job. The fixtures feed JPEG frames and H.264
# access units to the derived layer's real GStreamer child; they open no camera.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# avdec_h264 is the H.264 decoder this image has: the layer takes the first of
# nvv4l2decoder, v4l2h264dec, avdec_h264 and openh264dec that is installed.
for element in fdsrc fdsink gdpdepay gdppay jpegdec videoconvert tee queue x264enc h264parse avdec_h264; do
  gst-inspect-1.0 "$element" > /dev/null
done
output="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wendy-derived-camera-layer"
mkdir -p "$output"
export WENDY_GSTREAMER_REQUIRED=1
test_name=TestDerivedLayerGivesEveryPictureAndAccessUnitItsSourceIdentity
h264_tests=(TestDerivedLayerDecodesEachH264AccessUnitToThePictureOfItsOwnFrame TestDerivedLayerPairsNoPictureOfAnH264StreamItsDecoderReorders)
go test -race -c -o "$output/services.test" ./go/internal/agent/services
trap 'rm -f "$output/services.test"' EXIT
required="$(IFS='|'; echo "$test_name|${h264_tests[*]}")"
"$output/services.test" -test.run "^(${required})$" -test.v -test.count=1 -test.timeout=240s | tee "$output/normal.log"
for required_test in "$test_name" "${h264_tests[@]}"; do
  grep -q "^--- PASS: $required_test " "$output/normal.log"
done
grep -q 'H.264 decoder: h264parse ! avdec_h264' "$output/normal.log"
if grep -q '^--- SKIP:' "$output/normal.log"; then
  echo 'A required derived camera layer regression was skipped' >&2
  exit 1
fi
rm -f "$output/services.test"

python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1]).resolve()
source = Path('go/internal/agent/services/video_derived_tags.go').resolve()
original = source.read_text()
# Pair every output with the newest frame fed instead of the frame its tag names.
before, after = 't.sources[nearestDerivedTag(outputTimestamp-shift)]', 't.sources[t.last]'
if original.count(before) != 1:
    raise SystemExit('newest-frame: source mutation no longer identifies exactly one site')
replacement = out / 'newest-frame.go'
replacement.write_text(original.replace(before, after))
(out / 'newest-frame.json').write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
if go test -race -overlay "$output/newest-frame.json" ./go/internal/agent/services -run "^${test_name}$" -v -count=1 -timeout 120s > "$output/newest-frame.log" 2>&1; then
  cat "$output/newest-frame.log"
  echo 'Regression did not reject pairing outputs with the newest frame' >&2
  exit 1
fi
cat "$output/newest-frame.log"
grep -q "^--- FAIL: $test_name " "$output/newest-frame.log"
grep -q 'but shows luma' "$output/newest-frame.log"
