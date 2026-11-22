#!/usr/bin/env bash
# Audit 2086: real TCP/HTTP2 and disk staging, using the full 30-second watchdog.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: test-chunk-stall-transport.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
package=./go/internal/cli/commands
regression=TestChunkStallTCPResumesGzipFromDiskWithoutDuplicateContent
go test -race -tags regression_chunk_stall "$package" -run "^${regression}$" -count=1 -timeout=110s -v > "$output/normal.log" 2>&1
cat "$output/normal.log"
grep -q "^--- PASS: $regression " "$output/normal.log"
grep -q 'full production stall recovered over TCP with gzip; exact disk bytes=' "$output/normal.log"
if grep -q -- '--- SKIP:' "$output/normal.log"; then exit 1; fi
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
output = Path(sys.argv[1])
for name, source, before, after in (
    ('no-watchdog', 'go/internal/shared/chunkupload/stall.go',
     'if a.stalled(now, timeout) {', 'if false && a.stalled(now, timeout) {'),
    ('no-gzip-fallback', 'go/internal/cli/commands/chunkresume.go',
     'cfg = gzipChunkUploadConfig', 'cfg.compressor = ""'),
    ('forget-staged-content', 'go/internal/agent/containerd/chunkstore.go',
     'if !c.staging.retain(h, now) {', 'if true || !c.staging.retain(h, now) {'),
):
    source = Path(source).resolve()
    original = source.read_text()
    assert original.count(before) == 1, f'{name}: production site changed'
    replacement = output / (name + '.go')
    replacement.write_text(original.replace(before, after))
    (output / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
for control in no-watchdog no-gzip-fallback forget-staged-content; do
  if go test -race -tags regression_chunk_stall -overlay="$output/$control.json" "$package" -run "^${regression}$" -count=1 -timeout=110s -v > "$output/$control.log" 2>&1; then
    cat "$output/$control.log"
    echo "Chunk stall transport accepted $control regression" >&2
    exit 1
  fi
  cat "$output/$control.log"
  grep -q "^--- FAIL: $regression " "$output/$control.log"
  case "$control" in
    no-watchdog) grep -q 'production watchdog did not resume blocked TCP upload:' "$output/$control.log" ;;
    no-gzip-fallback) grep -q 'blocked transport did not switch to gzip:' "$output/$control.log" ;;
    forget-staged-content) grep -q 'retry must reuse disk content' "$output/$control.log" ;;
  esac
done
