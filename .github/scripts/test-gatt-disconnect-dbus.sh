#!/usr/bin/env bash
# Audit1903: actual Linux GATT D-Bus transport, with no physical Bluetooth socket.
set -euo pipefail
[[ $(uname -s) == Linux && $EUID != 0 && $# == 1 ]] || {
  echo 'usage: test-gatt-disconnect-dbus.sh NEW_OUTPUT_DIRECTORY (ordinary Linux user)' >&2
  exit 2
}
command -v dbus-daemon >/dev/null
mkdir "$1"
output=$(realpath "$1")
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out=Path(sys.argv[1])
source=Path('go/internal/shared/ble/central/bluez_linux.go').resolve()
original=source.read_text()
for name,before,after in (
    ('missing-disconnect-wakeup', 'if connected, _ := v.Value().(bool); !connected {\n\t\t\t\tg.markLost()',
     'if connected, _ := v.Value().(bool); !connected {\n\t\t\t\t// Regression control: omit disconnect wakeup.'),
    ('disconnect-borrowed-link', 'if g.weConnected && !l2capOpen {', 'if !l2capOpen {'),
):
    assert original.count(before)==1, f'{name} source site changed'
    replacement=out/(name+'.go')
    replacement.write_text(original.replace(before,after))
    (out/(name+'.json')).write_text(json.dumps({'Replace':{str(source):str(replacement)}}))
PY
for variant in normal missing-disconnect-wakeup disconnect-borrowed-link; do
  overlay=()
  selection='^TestGATTPrivateBusDisconnectLifecycle$'
  if [[ $variant != normal ]]; then
    overlay=(-overlay="$output/$variant.json")
    if [[ $variant == disconnect-borrowed-link ]]; then
      selection='^TestGATTPrivateBusDisconnectLifecycle$/^borrowed-true$'
    fi
  fi
  go test -race -tags regression_bluetooth "${overlay[@]}" -c \
    -o "$output/$variant.test" ./go/internal/shared/ble/central
  status=0
  python3 .github/scripts/run-owned-media-test.py --timeout 45 -- \
    "$output/$variant.test" -test.run "$selection" -test.v -test.count=1 -test.timeout=30s \
    > "$output/$variant.log" 2>&1 || status=$?
  cat "$output/$variant.log"
  if grep -Eq "WARNING: DATA RACE|--- SKIP:" "$output/$variant.log"; then exit 1; fi
  if [[ $variant == normal ]]; then
    [[ $status == 0 ]] || exit 1
    grep -q '^--- PASS: TestGATTPrivateBusDisconnectLifecycle ' "$output/$variant.log"
    [[ $(grep -c 'private GATT disconnect lifecycle verified:' "$output/$variant.log") == 2 ]]
  else
    [[ $status == 1 ]] || exit 1
    grep -q '^--- FAIL: TestGATTPrivateBusDisconnectLifecycle ' "$output/$variant.log"
    if [[ $variant == missing-disconnect-wakeup ]]; then
      grep -q 'disconnect did not wake both pending GATT waiters within one second' "$output/$variant.log"
    else
      grep -q 'ACL ownership violated:' "$output/$variant.log"
    fi
  fi
  rm "$output/$variant.test"
done
