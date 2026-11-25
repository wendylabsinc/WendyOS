#!/usr/bin/env bash
# Audit 2670: real native embedded host processes, persisted intent, live TLS RPC.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: test-operator-native-restart.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
package=./go/internal/cli/sessionbroker
regression=TestOperatorNativeProcessRestartPreservesExplicitDisconnect
export WENDY_E2E_OPERATOR_RESTART=1
# Retain production route/disconnect timers. Each fresh process has an 18s
# operation deadline; the whole five-process journey is bounded to 90s.
go test -race "$package" -run "^${regression}$" -count=1 -v -timeout=100s 2>&1 | tee "$output/normal.log"
grep -q "^--- PASS: $regression " "$output/normal.log"
if grep -q -- '--- SKIP:' "$output/normal.log"; then exit 1; fi
for phase in enable-exit passive-disconnect blocked explicit-resume passive-cleanup; do
  grep -q "fresh native process completed $phase;" "$output/normal.log"
done
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1])
source = Path('go/internal/cli/sessionbroker/operator.go').resolve()
original = source.read_text()
before = '\trecipe.Disconnected = true\n'
assert original.count(before) == 1, 'explicit disconnect persistence site changed'
# Keep the real live IPC shutdown, but omit the durable stop bit fixed by 2670.
# A fresh native process must catch this by actually attempting a passive join.
mutant = out / 'forgotten-disconnect.go'
mutant.write_text(original.replace(before, '\trecipe.Disconnected = false\n'))
(out / 'forgotten-disconnect.json').write_text(json.dumps({'Replace': {str(source): str(mutant)}}))
PY
if go test -race -overlay="$output/forgotten-disconnect.json" "$package" -run "^${regression}$" -count=1 -v -timeout=100s > "$output/forgotten-disconnect.log" 2>&1; then
  cat "$output/forgotten-disconnect.log"
  echo 'Fresh native process accepted forgotten explicit disconnect intent' >&2
  exit 1
fi
cat "$output/forgotten-disconnect.log"
grep -q 'fresh native client lost persisted disconnect at passive automatic: <nil>' "$output/forgotten-disconnect.log"
grep -q "^--- FAIL: $regression " "$output/forgotten-disconnect.log"
