#!/usr/bin/env bash
# Use the existing media job and its GStreamer plugins. No camera is required.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
output="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wendy-thermal-raw-capture"
mkdir -p "$output"
export WENDY_GSTREAMER_REQUIRED=1
test_name=TestThermalRawSubscribersReceiveUnmodified16BitMeasurements
go test -race ./go/internal/agent/services -run "^${test_name}$" -v -count=1 -timeout 60s | tee "$output/normal.log"
grep -q "^--- PASS: $test_name " "$output/normal.log"
if grep -q '^--- SKIP:' "$output/normal.log"; then
  echo 'The required thermal measurement regression was skipped' >&2
  exit 1
fi

python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1]).resolve()
services = Path('go/internal/agent/services').resolve()
mutations = {
    # Restore the original first-advertised-format behavior from audit 1987.
    'prefer-preview': (
        services / 'video_raw_tap.go',
        'if best == nil || f.sampleBits > best.sampleBits {',
        'if best == nil {',
    ),
    # Preserve Y16 metadata but destroy the measurement precision in the real
    # GStreamer raw branch. Subscriber sample assertions must reject the bytes.
    'reduce-depth': (
        services / 'video_service.go',
        'pipeline += fmt.Sprintf(" %s. ! %s ! fdsink fd=%d", rawTeeName, rawTapQueue, rawTapFD)',
        'pipeline += fmt.Sprintf(" %s. ! videoconvert ! video/x-raw,format=GRAY8 ! %s ! fdsink fd=%d", rawTeeName, rawTapQueue, rawTapFD)',
    ),
}
for name, (source, before, after) in mutations.items():
    original = source.read_text()
    if original.count(before) != 1:
        raise SystemExit(f'{name}: source mutation no longer identifies exactly one site')
    replacement = out / (name + '.go')
    replacement.write_text(original.replace(before, after))
    (out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY

for mutation in prefer-preview reduce-depth; do
  case "$mutation" in
    prefer-preview) expected='thermal raw subscriber received preview or wrong layout' ;;
    reduce-depth) expected='thermal raw subscriber samples changed' ;;
  esac
  if go test -race -overlay "$output/$mutation.json" ./go/internal/agent/services -run "^${test_name}$" -v -count=1 -timeout 60s > "$output/$mutation.log" 2>&1; then
    cat "$output/$mutation.log"
    echo "Regression did not reject $mutation" >&2
    exit 1
  fi
  cat "$output/$mutation.log"
  grep -q "^--- FAIL: $test_name " "$output/$mutation.log"
  grep -q "$expected" "$output/$mutation.log"
done
