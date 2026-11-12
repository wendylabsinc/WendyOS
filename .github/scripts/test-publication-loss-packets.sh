#!/usr/bin/env bash
# Audit2420: four real TLS/packet nodes; retain the full 30-second idle timeout.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: test-publication-loss-packets.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
export WENDY_E2E_PUBLICATION_LOSS=1
package=./go/internal/agent/localmesh
regression=TestPublicationLossDiamondRecoversWithCurrentLeaf
go test -race "$package" -run "^${regression}$" -count=1 -v -timeout=120s > "$output/normal.log" 2>&1
cat "$output/normal.log"
grep -q "^--- PASS: $regression " "$output/normal.log"
grep -q 'production idle timeout recovered exact current-leaf publication and live TCP:' "$output/normal.log"
if grep -q -- '--- SKIP:' "$output/normal.log"; then exit 1; fi
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out = Path(sys.argv[1])
for name, path, before, after in (
    ('retained-timed-out-claim', 'netgossip/gossip.go',
     'if f.owner != 0 && !f.received && now.Sub(f.progress) >= IdleTimeout {',
     'if false && f.owner != 0 && !f.received && now.Sub(f.progress) >= IdleTimeout {'),
    ('obsolete-leaf-authority', 'localmesh/directory.go',
     'return validManifestSignature(e.wire, leaf)', 'return true'),
):
    source = Path('go/internal/agent', path).resolve()
    original = source.read_text()
    assert original.count(before) == 1, f'{name} source site changed'
    replacement = out / (name + '.go')
    replacement.write_text(original.replace(before, after))
    (out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
for control in retained-timed-out-claim obsolete-leaf-authority; do
  if go test -race -overlay="$output/$control.json" "$package" -run "^${regression}$" -count=1 -v -timeout=120s > "$output/$control.log" 2>&1; then
    cat "$output/$control.log"
    echo "Publication-loss packets accepted $control regression" >&2
    exit 1
  fi
  cat "$output/$control.log"
  grep -q "^--- FAIL: $regression " "$output/$control.log"
  case "$control" in
    retained-timed-out-claim)
      grep -q 'new key verified; old publication authority and application TCP withdrawn while unrelated TCP works' "$output/$control.log"
      grep -q 'timed-out primary claim did not recover current-leaf publication through alternate supplier' "$output/$control.log"
      ;;
    obsolete-leaf-authority)
      grep -q 'obsolete publication retained authority after verified key rotation' "$output/$control.log"
      ;;
  esac
done
