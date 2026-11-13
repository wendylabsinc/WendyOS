#!/usr/bin/env bash
# Compile CLI controls once; the existing containerd.test contains the fixture.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: prepare-watch-container.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
mkdir "$1"
output=$(cd "$1" && pwd -P)
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out = Path(sys.argv[1])
watch = Path('go/internal/cli/commands/watch.go').resolve()
compose = Path('go/internal/cli/commands/compose.go').resolve()
changes = {
    'forced-detach': (watch, 'func withWatchInvariants(opts runOptions) runOptions {\n', 'func withWatchInvariants(opts runOptions) runOptions {\n\topts.detach = true\n'),
    'restart-sibling': (compose, 'preserve = selectPreservedWatchServices(opts.watchState, deviceKey, candidates, states)', 'preserve = map[string]bool{}'),
}
for name, (source, before, after) in changes.items():
    text = source.read_text()
    assert text.count(before) == 1, name + ': production site changed'
    mutant = out / (name + '.go')
    mutant.write_text(text.replace(before, after))
    (out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(mutant)}}))
PY
[[ $(uname -s) == Linux ]] || { echo 'Build the production CLI on Linux with libusb/ALSA development headers.' >&2; exit 2; }
export CGO_ENABLED=1 GOOS=linux
go build -o "$output/normal" ./go/cmd/wendy
for control in forced-detach restart-sibling; do
  go build -overlay="$output/$control.json" -o "$output/$control" ./go/cmd/wendy
done
