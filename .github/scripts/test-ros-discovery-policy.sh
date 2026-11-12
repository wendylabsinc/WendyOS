#!/usr/bin/env bash
# Observe production DDS discovery on isolated wired and simulated Wi-Fi paths.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: test-ros-discovery-policy.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
test_name=TestAutomaticROSDiscoveryNeverUsesWirelessPath
go test -race -c -o "$output/discovery.test" ./go/internal/agent/hoststats/rosbattery
sudo .github/scripts/test-packet-backends.sh "$output/discovery.test" "$test_name" 2>&1 | tee "$output/normal.log"
for phase in explicit-wireless-control automatic-wired-and-wireless automatic-wireless-only; do
  grep -q -- "--- PASS: $test_name/$phase " "$output/normal.log"
done
grep -q -- "^--- PASS: $test_name " "$output/normal.log"
if grep -q -- '--- SKIP:' "$output/normal.log"; then exit 1; fi

python3 - "$output" <<'PY'
import json
import sys
from pathlib import Path
output = Path(sys.argv[1])
source = Path('go/internal/rtps/interfaces.go').resolve()
text = source.read_text()
needle = ' || IsWirelessInterface(iface.Name)'
assert text.count(needle) == 1
mutated = output / 'wireless-allowed.go'
mutated.write_text(text.replace(needle, ''))
(output / 'wireless-allowed.json').write_text(json.dumps({'Replace': {str(source): str(mutated)}}))
PY
go test -race -overlay="$output/wireless-allowed.json" -c -o "$output/wireless-allowed.test" ./go/internal/agent/hoststats/rosbattery
if sudo .github/scripts/test-packet-backends.sh "$output/wireless-allowed.test" "$test_name" 2>&1 | tee "$output/mutation.log"; then
  echo 'Automatic wireless discovery mutation unexpectedly passed' >&2
  exit 1
fi
grep -q 'automatic discovery emitted RTPS on wireless path' "$output/mutation.log"
grep -q -- "--- FAIL: $test_name/automatic-wireless-only " "$output/mutation.log"
