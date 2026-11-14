#!/usr/bin/env bash
# Run complete TLS/UDP admission exchanges and reject a blocking-write mutation.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: test-tls-udp-admission.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
test_name=TestTLSUDPStallPreservesAdmissionAndRecovery
go test -race ./go/internal/agent/meshsession -run "^${test_name}$" -count=20 -v -timeout=90s | tee "$output/normal.log"
[[ $(grep -c "^--- PASS: $test_name " "$output/normal.log") == 20 ]]
if grep -q -- '--- SKIP:' "$output/normal.log"; then exit 1; fi

python3 - "$output" <<'PY'
import json
import sys
from pathlib import Path

output = Path(sys.argv[1])
source = Path('go/internal/agent/meshsession/datagram_tls.go').resolve()
text = source.read_text()
needle = 'func attachTLSUDPTransport(conn net.Conn, endpoint *udpmessage.Endpoint, maximum uint16) (<-chan struct{}, error) {\n'
assert text.count(needle) == 1
# Reintroduce the regression: write the TLS record under the endpoint's source
# authorization guard and shared scheduler, rather than admitting to its queue.
blocking = '''
	if true {
		var writeMu sync.Mutex
		failed := make(chan struct{})
		return failed, endpoint.SetTransport(MaxUDPTransportChunk, true, func(b []byte) error {
			return writeTLSUDPFrame(conn, &writeMu, b, maximum)
		})
	}
'''
mutated = output / 'blocking-tls-write.go'
mutated.write_text(text.replace(needle, needle + blocking))
(output / 'blocking-tls-write.json').write_text(json.dumps({'Replace': {str(source): str(mutated)}}))
PY
if go test -race -overlay="$output/blocking-tls-write.json" ./go/internal/agent/meshsession -run "^${test_name}$" -count=1 -v -timeout=30s 2>&1 | tee "$output/mutation.log"; then
  echo 'TLS writes under the shared admission guard unexpectedly passed' >&2
  exit 1
fi
grep -q 'TLS UDP echo stalled or changed: healthy flow while unrelated TLS consumer is stalled' "$output/mutation.log"
grep -q "^--- FAIL: $test_name " "$output/mutation.log"
