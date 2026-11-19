#!/usr/bin/env bash
# Audit1595: production BlueZ recovery on a private D-Bus daemon, without a radio.
set -euo pipefail
[[ $(uname -s) == Linux && $EUID != 0 && $# == 1 ]] || {
  echo 'usage: test-stale-bond-dbus.sh NEW_OUTPUT_DIRECTORY (ordinary Linux user)' >&2
  exit 2
}
command -v dbus-daemon >/dev/null
mkdir "$1"
output=$(realpath "$1")
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out = Path(sys.argv[1])
source = Path('go/internal/agent/bluetooth/manager_linux.go').resolve()
original = source.read_text()
for name, before, after in (
    ('no-key-repair', 'ok && pairSkipped && isStaleBondBluetoothError(name, message)',
     'ok && false && pairSkipped && isStaleBondBluetoothError(name, message)'),
    ('no-hid-check', 'if pair && isHIDDevice(props) && !waitForInputDevice',
     'if false && pair && isHIDDevice(props) && !waitForInputDevice'),
    ('lose-trust', 'if trust {\n\t\t// RemoveDevice dropped the Trusted flag',
     'if false && trust {\n\t\t// RemoveDevice dropped the Trusted flag'),
):
    assert original.count(before) == 1, f'{name} source site changed'
    replacement = out / (name + '.go')
    replacement.write_text(original.replace(before, after))
    (out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
for variant in normal no-key-repair no-hid-check lose-trust; do
  overlay=()
  selection='^TestStaleBondDBusLifecycle$'
  if [[ $variant != normal ]]; then
    overlay=(-overlay="$output/$variant.json")
    selection='^TestStaleBondDBusLifecycle$/^key-error$'
    if [[ $variant == no-hid-check ]]; then selection='^TestStaleBondDBusLifecycle$/^hollow-hid$'; fi
  fi
  go test -race -tags regression_bluetooth "${overlay[@]}" -c \
    -o "$output/$variant.test" ./go/internal/agent/bluetooth
  status=0
  python3 .github/scripts/run-owned-media-test.py --timeout 80 -- \
    "$output/$variant.test" -test.run "$selection" -test.v -test.count=1 -test.timeout=60s \
    > "$output/$variant.log" 2>&1 || status=$?
  cat "$output/$variant.log"
  if [[ $variant == normal ]]; then
    [[ $status == 0 ]] || exit 1
    grep -q '^--- PASS: TestStaleBondDBusLifecycle ' "$output/$variant.log"
    [[ $(grep -c 'private D-Bus lifecycle verified:' "$output/$variant.log") == 5 ]]
    if grep -q -- '--- SKIP:' "$output/$variant.log"; then exit 1; fi
  else
    [[ $status != 0 ]] || exit 1
    grep -q '^--- FAIL: TestStaleBondDBusLifecycle ' "$output/$variant.log"
    if [[ $variant == no-hid-check ]]; then
      grep -q 'stale-bond operation order:' "$output/$variant.log"
    else
      grep -q 'stale-bond recovery did not establish fresh trusted connection:' "$output/$variant.log"
    fi
  fi
  rm "$output/$variant.test"
done
