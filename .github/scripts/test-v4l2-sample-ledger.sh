#!/usr/bin/env bash
# Existing Linux media job only, with its already-owned loopback module loaded.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != true || "${RUNNER_ENVIRONMENT:-}" != github-hosted || "$(uname -s)" != Linux ]]; then
  echo 'Sample identity fixture requires the disposable hosted Linux media job.' >&2
  exit 2
fi
cd "$(git rev-parse --show-toplevel)"
if [[ ! -c /dev/v4l2loopback || "$(cat /sys/module/v4l2loopback/version)" != 0.15.4 || -e /dev/video96 || -L /dev/video96 ]]; then
  echo 'Require job-owned v4l2loopback 0.15.4 and absent video96.' >&2
  exit 2
fi
output="$(mktemp -d "${RUNNER_TEMP:?}/wendy-v4l2-ledger.XXXXXX")"
label="ci-lite-$(python3 -c 'import uuid; print(uuid.uuid4().hex[:12])'):camera"
drained=1
cleanup() {
  result=$?
  trap - EXIT INT TERM
  set +e
  if (( drained )); then
    sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted \
      python3 .github/scripts/cleanup-owned-lite-camera.py --label "$label" || result=1
    rm -f "$output/"*.test
  else
    echo "Kernel test drain uncertain; retaining $output and owned node for disposable-job teardown." >&2
    result=1
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
python3 -m venv "$output/python"
python3 .github/scripts/run-owned-media-test.py --timeout 180 -- \
  "$output/python/bin/python" -m pip install --disable-pip-version-check \
  'av==14.2.0' 'numpy==2.2.6' 'opencv-python-headless==4.12.0.88' 'onnx==1.19.1' 'onnxruntime==1.23.2'
python3 .github/scripts/run-owned-media-test.py --timeout 15 -- \
  cc .github/scripts/fixtures/v4l2-ledger/abi.c -o "$output/abi"
python3 .github/scripts/run-owned-media-test.py --timeout 5 -- \
  "$output/python/bin/python" .github/scripts/fixtures/v4l2-ledger/check_abi.py "$output/abi" | tee "$output/abi.log"
python3 .github/scripts/run-owned-media-test.py --timeout 15 -- \
  "$output/python/bin/python" .github/scripts/fixtures/v4l2-ledger/decoder_contract.py | tee "$output/decoder.log"
grep -q H264_IDENTITY_REORDER_VERIFIED "$output/decoder.log"
python3 .github/scripts/run-owned-media-test.py --timeout 20 -- \
  "$output/python/bin/python" .github/scripts/fixtures/v4l2-ledger/journey.py prepare --fixture "$output/fixture"
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out=Path(sys.argv[1])
source=Path('go/internal/agent/services/hub_loopback_pump.go').resolve()
original=source.read_text()
before='writer.WriteFrame(frame.data, frame.sampleID)'
if original.count(before)!=1: raise SystemExit('historical timestamp control site changed')
# b42fdde4c parent wrote canonical nanoseconds into a microsecond timeval.
# The current writer encodes its argument directly, so restore the old wire
# value (receipt/1000) while retaining the actual OUTPUT ioctl implementation.
mutant=out/'receipt-time.go'
mutant.write_text(original.replace(before,'writer.WriteFrame(frame.data, uint64(frame.receiptBootNanos / 1000))'))
(out/'receipt-time.json').write_text(json.dumps({'Replace':{str(source):str(mutant)}}))
PY
tags=regression_campaign_runtime,regression_sample_ledger
go test -race -tags "$tags" -c -o "$output/normal.test" ./go/internal/agent/services
go test -race -tags "$tags" -overlay "$output/receipt-time.json" -c -o "$output/receipt-time.test" ./go/internal/agent/services
name=TestV4L2ExamplePredictionJoinsRecordedCameraPayload
run_fixture() {
  local result=0
  drained=0
  sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted WENDY_V4L2_SAMPLE_LEDGER=1 \
    WENDY_V4L2_LEDGER_FIXTURE="$output/fixture" WENDY_V4L2_LEDGER_PYTHON="$output/python/bin/python" \
    WENDY_V4L2_LEDGER_JOURNEY="$PWD/.github/scripts/fixtures/v4l2-ledger/journey.py" \
    WENDY_V4L2_LEDGER_LABEL="$label" WENDY_V4L2_LEDGER_WORK="$output" \
    python3 .github/scripts/run-owned-media-test.py --timeout 55 -- "$1" -test.run "^$name$" -test.v -test.count=1 -test.timeout=0 || result=$?
  # Only an ordinary awaited test success/failure authorizes explicit cleanup.
  # Signals/watchdog/unknown failures retain the node for job-owned teardown.
  if (( result == 0 || result == 1 )); then drained=1; fi
  return "$result"
}
normal_status=0
run_fixture "$output/normal.test" > "$output/normal.log" 2>&1 || normal_status=$?
cat "$output/normal.log"
[[ "$normal_status" == 0 ]]
grep -q "^--- PASS: $name " "$output/normal.log"
grep -q V4L2_SAMPLE_CAMERA_INDEX_JOIN_VERIFIED "$output/normal.log"
status=0
run_fixture "$output/receipt-time.test" > "$output/receipt-time.log" 2>&1 || status=$?
cat "$output/receipt-time.log"
[[ "$status" == 1 ]]
grep -q "^--- FAIL: $name " "$output/receipt-time.log"
grep -q 'V4L2 sample identity does not resolve in recorded camera index:' "$output/receipt-time.log"
if grep -Eq -- '--- SKIP:|WARNING: DATA RACE' "$output/normal.log" "$output/receipt-time.log"; then exit 1; fi
echo "Sample identity fixture logs: $output"
