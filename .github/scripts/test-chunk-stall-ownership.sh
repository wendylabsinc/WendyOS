#!/usr/bin/env bash
# Audit2086: actual root/user ownership, confined to fresh hosted fixture paths.
set -euo pipefail
if [[ ${GITHUB_ACTIONS:-} != true || ${RUNNER_ENVIRONMENT:-} != github-hosted || $(uname -s) != Linux ]]; then
  echo 'Chunk ownership fixture requires the disposable GitHub-hosted Linux runner.' >&2
  exit 2
fi
[[ $# == 1 && $EUID != 0 ]] || { echo 'Run as the ordinary hosted runner with NEW_OUTPUT_DIRECTORY.' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
# Keep the socket path short even when RUNNER_TEMP/artifact paths are long.
scratch=$(mktemp -d /tmp/wco.XXXXXXXX)
owner_uid=$(id -u)
owner_gid=$(id -g)
[[ $owner_uid != 0 && $owner_gid != 0 ]] || exit 2
cleanup() { sudo -n rm -rf -- "$scratch"; }
trap cleanup EXIT
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out = Path(sys.argv[1])
for name, path, before, after in (
    ('private-data', 'commands/chunkcompression.go', 'tmp.Chmod(0o644)', 'tmp.Chmod(0o600)'),
    ('private-lock', 'commands/chunkcompression.go', 'f.Chmod(0o644)', 'f.Chmod(0o600)'),
    ('elevated-start', 'sessionbroker/broker.go', 'runtime.GOOS == "windows" || !configDirOwnershipSafe()', 'runtime.GOOS == "windows"'),
    ('elevated-run', 'sessionbroker/broker.go', 'if !configDirOwnershipSafe() {', 'if false && !configDirOwnershipSafe() {'),
):
    source = Path('go/internal/cli', path).resolve()
    original = source.read_text()
    assert original.count(before) == 1, f'{name}: production site changed'
    replacement = out / (name + '.go')
    replacement.write_text(original.replace(before, after))
    (out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
for variant in cache-normal private-data private-lock broker-normal elevated-start elevated-run; do
  case "$variant" in
    cache-normal|private-*) package=commands; regression=TestChunkStallCacheRootThenUser ;;
    *) package=sessionbroker; regression=TestRootBrokerLeavesUserStateForRealUnprivilegedRPC ;;
  esac
  overlay=()
  if [[ $variant != *-normal ]]; then overlay=(-overlay="$output/$variant.json"); fi
  binary="$output/$variant.test"
  go test -race -tags regression_chunk_ownership "${overlay[@]}" -c "./go/internal/cli/$package" -o "$binary"
  if [[ $variant == broker-normal ]]; then
    timeout --kill-after=3s 25s env WENDY_OWNER_SCRATCH="$scratch" "$binary" -test.run '^TestBrokerUnprivilegedRoundTripControl$' -test.v -test.timeout=20s > "$output/user-positive.log" 2>&1
    cat "$output/user-positive.log"
    grep -q '^--- PASS: TestBrokerUnprivilegedRoundTripControl ' "$output/user-positive.log"
  fi
  status=0
  # The runner owns logs/binaries. Root owns only a newly-created fixture tree;
  # children drop to this runner's real UID/GID and inherit no supplementary groups.
  # shellcheck disable=SC2024
  sudo -n timeout --kill-after=3s 30s env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted \
    WENDY_CHUNK_OWNERSHIP=1 WENDY_OWNER_SCRATCH="$scratch" WENDY_OWNER_UID="$owner_uid" WENDY_OWNER_GID="$owner_gid" \
    "$binary" -test.run "^${regression}$" -test.v -test.timeout=25s > "$output/$variant.log" 2>&1 || status=$?
  cat "$output/$variant.log"
  if [[ $variant == *-normal ]]; then
    [[ $status == 0 ]] || exit 1
    grep -q "^--- PASS: $regression " "$output/$variant.log"
    if grep -q -- '--- SKIP:' "$output/$variant.log"; then exit 1; fi
  else
    [[ $status != 0 ]] || exit 1
    grep -q "^--- FAIL: $regression " "$output/$variant.log"
    case "$variant" in
      private-data) grep -q 'root-owned stall data was not readable by its user' "$output/$variant.log" ;;
      private-lock) grep -q 'root-owned stall lock prevented user update' "$output/$variant.log" ;;
      elevated-start) grep -q 'elevated Start launched a helper for a foreign-owned user directory' "$output/$variant.log" ;;
      elevated-run) grep -Eq 'elevated Run bypassed ownership refusal|elevated broker contacted upstream after ownership refusal|elevated broker planted session state' "$output/$variant.log" ;;
    esac
  fi
  rm "$binary"
done
