#!/usr/bin/env bash
# Audit 2676: real cold broker failure, packet traffic and spare-carrier recovery.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: test-operator-spare-recovery.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
package=./go/internal/cli/sessionbroker
regression=TestOperatorColdFailureRecoversWithSpareCarrierTraffic
export WENDY_E2E_OPERATOR_SPARE=1
# Production carrier/Babel timers remain unchanged. Cold caller deadline is 4s;
# each route barrier is bounded to 12s, and the entire journey to 90s.
go test -race "$package" -run "^${regression}$" -count=1 -v -timeout=100s 2>&1 | tee "$output/normal.log"
grep -q "^--- PASS: $regression " "$output/normal.log"
grep -q 'original TCP stream survived faster admission and standby fallback' "$output/normal.log"
if grep -q -- '--- SKIP:' "$output/normal.log"; then exit 1; fi
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1])

def overlay(name, source, before, after):
    source = Path(source).resolve()
    original = source.read_text()
    assert original.count(before) == 1, f'{name} mutation site changed'
    mutant = out / (name + '.go')
    mutant.write_text(original.replace(before, after))
    (out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(mutant)}}))

overlay('lost-cold-diagnostic', 'go/internal/cli/sessionbroker/operator_attachment.go',
        'options.ReportCarrierFailure = operatorCarrierReporter(ctx)',
        'options.ReportCarrierFailure = nil')
# Restore the pre-2676 policy: cancel a verified slower carrier as soon as a
# cheaper route forwards, even while physical carrier slots remain available.
before = '\tplan := func(now time.Time) {\n\t\tview := snapshot()\n'
overlay('retired-spare', 'go/internal/cli/netmesh/carriers/run.go', before,
        before + '''\t\tfor _, a := range active {
\t\t\tif a.verified && !a.canceled && view.HasCheaperLink(a.peer, a.candidate.Cost) {
\t\t\t\ta.canceled = true
\t\t\t\ta.cancel()
\t\t\t}
\t\t}
''')
PY
for control in lost-cold-diagnostic retired-spare; do
  if go test -race -overlay="$output/$control.json" "$package" -run "^${regression}$" -count=1 -v -timeout=100s > "$output/$control.log" 2>&1; then
    cat "$output/$control.log"
    echo "Spare-carrier journey accepted $control regression" >&2
    exit 1
  fi
  cat "$output/$control.log"
  grep -q "^--- FAIL: $regression " "$output/$control.log"
  case "$control" in
    lost-cold-diagnostic)
      grep -q 'cold real TLS failure lost bounded broker diagnostic:' "$output/$control.log"
      ;;
    retired-spare)
      grep -q 'real slow packet stream and authenticated RPC recovered the cold broker' "$output/$control.log"
      grep -Eq 'standby was retired despite spare capacity|faster carrier selected with slow standby retained|spare admission replaced a live incumbent' "$output/$control.log"
      ;;
  esac
done
