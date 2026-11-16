#!/usr/bin/env bash
# Require Linux job lifecycle coverage and reject a lost-supervisor cleanup leak.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: test-job-supervisor-lifetime.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
[[ $(uname -s) == Linux ]]
mkdir "$1"
output=$(realpath "$1")
test_name=TestDurableJobSupervisorCrashBoundsOwnedProcessesAndPreservesEvidence
go test -gcflags=all=-c=1 -vet=off -race ./go/internal/cli/jobops -run '^TestDurableJob' -count=1 -v -timeout=120s 2>&1 | tee "$output/normal.log"
[[ $(grep -c '^--- PASS: TestDurableJob' "$output/normal.log") == 5 ]]
for name in GuardRetainsStopAndTimeoutSemantics GuardStartupIsBoundedBeforeIdentity; do
  for stopping in true false; do
    grep -q -- "--- PASS: TestDurableJob$name/supervisor-TERM-$stopping " "$output/normal.log"
  done
done
if grep -q -- '--- SKIP:' "$output/normal.log"; then exit 1; fi

python3 - "$output" <<'PY'
import json
import sys
from pathlib import Path
root = Path.cwd().resolve()
output = Path(sys.argv[1])
worker = (root / 'go/internal/cli/jobops/worker.py').read_text()
needle = 'if stopping or (readable and not os.read(liveness_fd, 1)):'
assert worker.count(needle) == 1, 'supervisor cleanup mutation anchor changed'
worker = worker.replace(needle, 'if stopping:')
source = root / 'go/internal/cli/jobops/submit.go'
text = source.read_text()
needle = '//go:embed worker.py\nvar workerProgram string'
assert text.count(needle) == 1
mutated = output / 'submit_without_supervisor_cleanup.go'
mutated.write_text(text.replace(needle, 'var workerProgram = ' + json.dumps(worker)))
(output / 'overlay.json').write_text(json.dumps({'Replace': {str(source): str(mutated)}}))
PY
if go test -gcflags=all=-c=1 -vet=off -race -overlay="$output/overlay.json" ./go/internal/cli/jobops -run "^${test_name}$" -count=1 -v -timeout=30s 2>&1 | tee "$output/mutation.log"; then
  echo 'Lost-supervisor cleanup mutation unexpectedly passed' >&2
  exit 1
fi
grep -q 'deadline waiting for owned process group cleanup after supervisor SIGKILL' "$output/mutation.log"
grep -q "^--- FAIL: $test_name " "$output/mutation.log"
