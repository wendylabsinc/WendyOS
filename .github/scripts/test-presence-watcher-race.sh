#!/usr/bin/env bash
# Audit 2569: exercise the actual lease capture fixed by 4d61c7735.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: test-presence-watcher-race.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
package=./go/internal/shared/cloudrelay
regression=TestAgentRenewsProvenPresence

# This existing fixture performs actual signed issuance, holder proof, renewal,
# broker acknowledgement, persisted lease replacement and orderly shutdown.
# Leave the watcher at its default idle interval: frequent path probes in the
# traffic fixture add synchronization and can conceal the original capture race.
go test -race "$package" -run "^${regression}$" -count=5 -v -timeout=30s 2>&1 | tee "$output/normal.log"
[[ $(grep -c "^--- PASS: $regression " "$output/normal.log") == 5 ]]
if grep -q -- '--- SKIP:' "$output/normal.log"; then exit 1; fi

python3 - "$output" <<'PY'
import json
import sys
from pathlib import Path

output = Path(sys.argv[1])
source = Path('go/internal/shared/cloudrelay/agent.go').resolve()
text = source.read_text()
before = '\tgo func(endpoint string) {\n\t\tpathChanged <- a.watchPresencePath(ctx, presencePeer, endpoint)\n\t}(lease.Broker.Endpoint)'
# Restore precisely the pre-2569 closure; do not inject an unrelated race or
# delay into production. The mutable lease is now read by the watcher goroutine.
after = '\tgo func() { pathChanged <- a.watchPresencePath(ctx, presencePeer, lease.Broker.Endpoint) }()'
assert text.count(before) == 1, 'presence watcher mutation anchor changed'
mutated = text.replace(before, after)
renewal = '\t\t\t\tlease = renewing\n'
assert mutated.count(renewal) == 1, 'acknowledged lease replacement anchor changed'
replacement = output / 'captured-lease.go'
replacement.write_text(mutated)
(output / 'captured-lease.json').write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
# Require this race, not merely any failing test or incidental race report.
read_line = mutated[:mutated.index(after)].count('\n') + 1
write_line = mutated[:mutated.index(renewal)].count('\n') + 1
(output / 'race-lines.txt').write_text(f'{read_line} {write_line}\n')
PY
read -r captured_line renewed_line < "$output/race-lines.txt"

# Independent processes ensure the detector reports each run rather than
# suppressing a previously observed access pair within one test binary run.
for attempt in 1 2 3; do
  log="$output/captured-lease-$attempt.log"
  if go test -race -overlay="$output/captured-lease.json" "$package" -run "^${regression}$" -count=1 -v -timeout=15s > "$log" 2>&1; then
    cat "$log"
    echo 'Original presence lease capture unexpectedly passed the race detector' >&2
    exit 1
  fi
  cat "$log"
  grep -q 'WARNING: DATA RACE' "$log"
  grep -q "agent.go:$captured_line " "$log"
  grep -q "agent.go:$renewed_line " "$log"
  grep -q "^--- FAIL: $regression " "$log"
done
