#!/usr/bin/env bash
# Reuse the network job for actual relay traffic, renewal stalls, and path moves.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: test-relay-path-traffic.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
regression=TestRelayTrafficSurvivesSlowRenewalAndPathMove
package=./go/internal/shared/cloudrelay

go test -race "$package" -run "^${regression}$" -count=10 -v -timeout=90s 2>&1 | tee "$output/traffic.log"
[[ $(grep -c "^--- PASS: $regression " "$output/traffic.log") == 10 ]]
if grep -q -- '--- SKIP:' "$output/traffic.log"; then exit 1; fi

python3 - "$output" <<'PY'
import json
import sys
from pathlib import Path

output = Path(sys.argv[1])
source = Path('go/internal/shared/cloudrelay/agent.go').resolve()
text = source.read_text()

def overlay(name, original, replacement):
    assert text.count(original) == 1, (name, 'mutation anchor changed')
    mutated = output / (name + '.go')
    mutated.write_text(text.replace(original, replacement))
    (output / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(mutated)}}))

# Presence reconnect must not own the already admitted session's lifetime.
overlay('session-context', 'a.serve(parent, broker, s.o, s.c, s.d)',
        'a.serve(ctx, broker, s.o, s.c, s.d)')
# A local path move says nothing about a signed lease's validity.
overlay('discard-lease', '\t\t\tkeepLease = true\n\t\t\treturn err\n\t\tcase <-renewal.C:',
        '\t\t\tkeepLease = false\n\t\t\treturn err\n\t\tcase <-renewal.C:')
# Restore synchronous renewal: the receive loop cannot process a path change
# while the real Cloud socket is stalled, even though its data session is live.
overlay('blocking-renewal', '\t\t\tgo func(done chan struct{}) {\n\t\t\t\tdefer close(done)',
        '\t\t\tfunc(done chan struct{}) {\n\t\t\t\tdefer close(done)')
PY

mutation() {
  local name=$1 scenario=$2 diagnostic=$3
  if go test -race -overlay="$output/$name.json" "$package" -run "^${regression}$/$scenario" -count=1 -v -timeout=20s 2>&1 | tee "$output/$name.log"; then
    echo "relay path mutation unexpectedly passed: $name" >&2
    exit 1
  fi
  grep -q "$diagnostic" "$output/$name.log"
  grep -q "^--- FAIL: $regression " "$output/$name.log"
}
mutation session-context pending-renewal 'same session after path move:'
mutation discard-lease acknowledged-renewal 'replacement presence did not prove the retained lease'
mutation blocking-renewal pending-renewal 'path move did not reconnect presence while renewal was pending'

# Real production 30s keepalive, 35s idle path expiry, 40s application silence.
# Its disabled-keepalive child runs concurrently, so both controls cost ~40s.
WENDY_E2E_RELAY_IDLE=1 go test -race "$package" -run '^TestRelayIdleKeepalive$' -count=1 -v -timeout=70s 2>&1 | tee "$output/idle-keepalive.log"
grep -q '^--- PASS: TestRelayIdleKeepalive ' "$output/idle-keepalive.log"
grep -q 'IDLE_KEEPALIVE_PRESERVED' "$output/idle-keepalive.log"
grep -q 'IDLE_CONTROL_TRANSPORT_EXPIRED' "$output/idle-keepalive.log"
if grep -q -- '--- SKIP:' "$output/idle-keepalive.log"; then exit 1; fi
