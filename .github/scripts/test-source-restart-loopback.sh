#!/usr/bin/env bash
# Invoke inside the existing audio fixture, with its owned snd-aloop module
# and the media job's owned v4l2loopback module still loaded. Loads no modules.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != true || "${RUNNER_ENVIRONMENT:-}" != github-hosted || "$(uname -s)" != Linux ]]; then
  echo 'Source restart fixture requires the disposable GitHub-hosted Linux media runner.' >&2
  exit 2
fi
cd "$(git rev-parse --show-toplevel)"
if [[ ! -d /sys/module/snd_aloop || ! -c /dev/v4l2loopback ]]; then
  echo 'Source restart fixture requires both owned media modules already loaded.' >&2
  exit 2
fi
module_version="$(cat /sys/module/v4l2loopback/version)"
if [[ "$module_version" != 0.15.4 ]]; then
  printf 'Source restart fixture requires v4l2loopback 0.15.4; found <%s>.\n' "$module_version" >&2
  exit 2
fi
if [[ -e /dev/video96 || -L /dev/video96 ]]; then
  echo 'Refusing to reserve an existing video96 node.' >&2
  exit 2
fi
for command in go python3 sudo aplay arecord gst-launch-1.0; do command -v "$command" >/dev/null; done
for element in v4l2src jpegdec videoconvert fdsink; do gst-inspect-1.0 "$element" >/dev/null; done
output="$(mktemp -d "${RUNNER_TEMP:?}/wendy-source-restart.XXXXXX")"
camera_label="ci-lite-$(python3 -c 'import uuid; print(uuid.uuid4().hex[:12])')"
cleanup() {
  result=$?
  trap - EXIT INT TERM
  set +e
  # The guardian has drained the owned process group before this cleanup.
  sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted \
    python3 .github/scripts/cleanup-owned-lite-camera.py --label "$camera_label:camera" || result=1
  rm -f "$output/"*.test
  echo "Source restart fixture logs: $output"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

go test -race -c -o "$output/mcusource.test" ./go/internal/agent/mcusource
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out = Path(sys.argv[1])
source = Path('go/internal/agent/mcusource/supervisor.go').resolve()
original = source.read_text()
before = '\t\t\tdialAddrs = resolved\n'
if original.count(before) != 1:
    raise SystemExit('source resolver no longer has exactly one address assignment')
# Reproduce persisting the first discovered address instead of discovering the
# saved principal again after the source leaves its old endpoint.
after = before + '\t\t\taddr = resolved[0]\n\t\t\tpinned = true\n'
replacement = out / 'stale-source-address.go'
replacement.write_text(original.replace(before, after))
(out / 'stale-source-address.json').write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
go test -race -overlay "$output/stale-source-address.json" -c -o "$output/stale-source-address.test" ./go/internal/agent/mcusource

test_name=TestSourceRestartMovesBothKernelStreamsWithoutChangingIdentity
run_fixture() {
  sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted \
    WENDY_SOURCE_RESTART_LOOPBACK=1 WENDY_TEST_SOURCE_RESTART_LABEL="$camera_label" \
    python3 .github/scripts/run-owned-media-test.py --timeout 45 -- \
    "$1" -test.run "^${test_name}$" -test.v -test.count=1 -test.timeout=0
}
run_fixture "$output/mcusource.test" | tee "$output/normal.log"
grep -q "^--- PASS: $test_name " "$output/normal.log"
if grep -q '^--- SKIP:' "$output/normal.log"; then exit 1; fi
if run_fixture "$output/stale-source-address.test" > "$output/stale-source-address.log" 2>&1; then
  cat "$output/stale-source-address.log"
  echo 'Source restart fixture accepted a cached stale source address.' >&2
  exit 1
fi
cat "$output/stale-source-address.log"
grep -q "^--- FAIL: $test_name " "$output/stale-source-address.log"
grep -q 'source restart did not reconnect both channels' "$output/stale-source-address.log"
