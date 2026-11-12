#!/usr/bin/env bash
# Audit 2675: complete routers, persisted clock anchors and actual TCP packets.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: test-clock-packet-restart.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
package=./go/internal/agent/localmesh
regression=TestClockTrustRuntimeRestartPreservesPacketExpiryAndRecovery
export WENDY_E2E_CLOCK_PACKET_RESTART=1
# The actual 15-minute lease advances through existing injectable clocks;
# neither the duration nor any host clock changes. Packet/TLS actors run live.
go test -race "$package" -run "^${regression}$" -count=2 -v -timeout=90s 2>&1 | tee "$output/normal.log"
[[ $(grep -c "^--- PASS: $regression " "$output/normal.log") == 2 ]]
if grep -q -- '--- SKIP:' "$output/normal.log"; then exit 1; fi
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1])
source = Path('go/internal/agent/localmesh/directory.go').resolve()
original = source.read_text()
before = 'if d.monotonicNow != nil {\n\t\treturn d.monotonicNow()\n\t}'
assert original.count(before) == 1, 'receiver monotonic lease clock site changed'
# Reintroduce coupling between certificate UTC and receiver lease scheduling.
# A harmless wall jump must now remove a route carrying real TCP application
# bytes, rather than merely change a helper's calculated timestamp.
after = 'if d.certificateNow != nil {\n\t\treturn d.certificateNow()\n\t}'
replacement = out / 'wall-clock-lease.go'
replacement.write_text(original.replace(before, after))
(out / 'wall-clock-lease.json').write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
if go test -race -overlay="$output/wall-clock-lease.json" "$package" -run "^${regression}$" -count=1 -v -timeout=60s > "$output/wall-clock-lease.log" 2>&1; then
  cat "$output/wall-clock-lease.log"
  echo 'Wall-clock lease regression unexpectedly preserved packet traffic' >&2
  exit 1
fi
cat "$output/wall-clock-lease.log"
grep -q 'wall clock jump disrupted live mesh TCP: after-wall-jump' "$output/wall-clock-lease.log"
grep -q "^--- FAIL: $regression " "$output/wall-clock-lease.log"
