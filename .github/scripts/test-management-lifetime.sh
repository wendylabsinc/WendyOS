#!/usr/bin/env bash
# Exercise live registry/RPC shutdown and reject lock and client-lifetime regressions.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: test-management-lifetime.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
registry=TestRegistryLifetimeTrafficAndLateRegistration
broker=TestOperatorBrokerNormalReconnectRetainsIPCAndEndpoint

go test -race ./go/internal/cli/commands -run "^${registry}$" -count=20 -v -timeout=90s 2>&1 | tee "$output/registry.log"
[[ $(grep -c "^--- PASS: $registry " "$output/registry.log") == 20 ]]
go test -race ./go/internal/cli/sessionbroker -run "^${broker}$" -count=3 -v -timeout=120s 2>&1 | tee "$output/broker.log"
[[ $(grep -c "^--- PASS: $broker " "$output/broker.log") == 3 ]]
if grep -q -- '--- SKIP:' "$output/registry.log" "$output/broker.log"; then exit 1; fi

python3 - "$output" <<'PY'
import json
import sys
from pathlib import Path

output = Path(sys.argv[1])

def overlay(name, source, original, replacement):
    source = Path(source).resolve()
    text = source.read_text()
    assert text.count(original) == 1, (name, 'mutation anchor changed')
    mutated = output / (name + '.go')
    mutated.write_text(text.replace(original, replacement))
    (output / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(mutated)}}))

# The synchronously rejected late AddCloser tries to take the same cache lock.
overlay('registration-lock', 'go/internal/cli/commands/docker.go',
        '\tdockerRegistryProxyCacheMu.Unlock()\n\tconn.AddCloser(closeFunc(func() {',
        '\tconn.AddCloser(closeFunc(func() {')
locked = output / 'registration-lock.go'
text = locked.read_text()
needle = '\t\tstopProxy()\n\t}))\n\tclose(entry.ready)'
assert text.count(needle) == 1
locked.write_text(text.replace(needle, '\t\tstopProxy()\n\t}))\n\tdockerRegistryProxyCacheMu.Unlock()\n\tclose(entry.ready)'))
# Removing socket cancellation restores the observed live-request shutdown leak.
overlay('relay-lifetime', 'go/internal/cli/commands/docker.go',
        '\tstopClient := context.AfterFunc(ctx, func() { client.Close() })',
        '\tstopClient := func() bool { return false }')
relay = output / 'relay-lifetime.go'
text = relay.read_text()
needle = '\tstopRemote := context.AfterFunc(ctx, func() { remote.Close() })'
assert text.count(needle) == 1
relay.write_text(text.replace(needle, '\tstopRemote := func() bool { return false }'))
# Physical socket loss already ends the RPC. The extra Shutdown assertion must
# also reject an idle gRPC reconnect loop leaked after the carrier is removed.
overlay('retained-client', 'go/internal/cli/operatormesh/carriers.go',
        '\t\tif retired != nil {\n\t\t\t_ = retired.Close()\n\t\t}',
        '\t\t_ = retired // mutation: leave the management reconnect loop alive')
PY

mutation() {
  local name=$1 package=$2 test_name=$3 diagnostic=$4
  if go test -race -overlay="$output/$name.json" "$package" -run "^${test_name}$" -count=1 -v -timeout=45s 2>&1 | tee "$output/$name.log"; then
    echo "management lifetime mutation unexpectedly passed: $name" >&2
    exit 1
  fi
  grep -q "$diagnostic" "$output/$name.log"
  grep -q "^--- FAIL: $test_name " "$output/$name.log"
}
mutation registration-lock ./go/internal/cli/commands "$registry" 'late proxy registration deadlocked against cache eviction'
mutation relay-lifetime ./go/internal/cli/commands "$registry" 'connection close did not finish proxy cleanup'
mutation retained-client ./go/internal/cli/sessionbroker "$broker" 'lost carrier retained its management client'
