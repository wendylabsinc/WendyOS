#!/usr/bin/env bash
# Audit2575 and stale-certificate carrier regression: real service packet I/O.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: test-service-packet-recovery.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
export WENDY_E2E_SERVICE_PACKET_RECOVERY=1 WENDY_SERVICE_PACKET_EVIDENCE="$output"
package=./go/internal/agent/localmesh
regression=TestServicePublishFailureRecoversTrafficWithRenewedIdentity
go test -race "$package" -run "^${regression}$" -count=1 -v -timeout=90s 2>&1 | tee "$output/normal.log"
grep -q "^--- PASS: $regression " "$output/normal.log"
grep -q 'accepted advertised service and established TCP survived signer recovery' "$output/normal.log"
if grep -q -- '--- SKIP:' "$output/normal.log"; then exit 1; fi
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out = Path(sys.argv[1])
for name, path, before, after in (
    ('lost-service-rollback', 'node.go', 'n.advertisement.Services = previous', '_ = previous'),
    ('fatal-stale-descriptor', 'certificate_exchange.go',
     'if errors.Is(err, errIdentityNotRenewal) {', 'if false && errors.Is(err, errIdentityNotRenewal) {'),
):
    source = Path('go/internal/agent/localmesh', path).resolve()
    original = source.read_text()
    assert original.count(before) == 1, f'{name} source site changed'
    replacement = out / (name + '.go')
    replacement.write_text(original.replace(before, after))
    (out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
for control in lost-service-rollback fatal-stale-descriptor; do
  if go test -race -overlay="$output/$control.json" "$package" -run "^${regression}$" -count=1 -v -timeout=90s > "$output/$control.log" 2>&1; then
    cat "$output/$control.log"
    echo "Service recovery accepted $control regression" >&2
    exit 1
  fi
  cat "$output/$control.log"
  grep -q "^--- FAIL: $regression " "$output/$control.log"
  case "$control" in
    lost-service-rollback)
      grep -q 'renewed TLS identity and complete manifest reached the original receiver; advertised service TCP works' "$output/$control.log"
      grep -q 'accepted service became unreachable after failed publication: advertised port=18476:' "$output/$control.log"
      ;;
    fatal-stale-descriptor)
      grep -q 'directory-control: identity bundle is not a renewal' "$output/$control.log"
      grep -q 'live receiver did not accept the service and exact renewed signing leaf' "$output/$control.log"
      ;;
  esac
done
