#!/usr/bin/env bash
# Use the v4l2loopback 0.15.4 module already loaded by the Linux media job.
# This script creates/removes only its test node; it does not load any module.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != true || "${RUNNER_ENVIRONMENT:-}" != github-hosted || "$(uname -s)" != Linux ]]; then
  echo 'Lite kernel camera fixture requires the disposable GitHub-hosted Linux runner.' >&2
  exit 2
fi
cd "$(git rev-parse --show-toplevel)"
if [[ ! -c /dev/v4l2loopback ]]; then
  echo 'Expected the owned v4l2loopback control device at /dev/v4l2loopback; the media module setup did not complete.' >&2
  exit 2
fi
if ! module_version="$(cat /sys/module/v4l2loopback/version 2>/dev/null)"; then
  echo 'Cannot read the loaded v4l2loopback module version; refusing the kernel camera fixture.' >&2
  exit 2
fi
if [[ "$module_version" != 0.15.4 ]]; then
  printf 'Expected v4l2loopback module version 0.15.4; found <%s>. Build the exact pinned checkout without its .gitversion development marker before loading it.\n' "$module_version" >&2
  exit 2
fi
for element in v4l2src jpegdec videoconvert fdsink; do
  gst-inspect-1.0 "$element" > /dev/null
done
output="$(mktemp -d "${RUNNER_TEMP:?}/wendy-lite-camera.XXXXXX")"
test_name=TestLiteCameraDeliversDecodedPixelsThroughKernelLoopback
# This invocation may reserve only an absent node. A unique, <=31-byte kernel
# label lets outer cleanup distinguish it from an unrelated/replacement camera.
if [[ -e /dev/video96 || -L /dev/video96 ]]; then
  echo 'Refusing to reserve an existing video96 node.' >&2
  exit 2
fi
camera_label="ci-lite-$(python3 -c 'import uuid; print(uuid.uuid4().hex[:12])')"
cleanup() {
  result=$?
  trap - EXIT INT TERM
  set +e
  sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted \
    python3 .github/scripts/cleanup-owned-lite-camera.py --label "$camera_label:camera" || result=1
  rm -f "$output/"*.test
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
controls = [
    ('combined-setup-budget', Path('go/internal/agent/mcusource/supervisor.go'),
     'if connector, ok := tr.(interface{ Connect(context.Context) error }); ok {',
     'if connector, ok := tr.(interface{ Connect(context.Context) error }); false && ok {'),
    ('no-kernel-pixels', Path('go/internal/agent/ros2camera/writer_linux.go'),
     '\tdata := frame.Data\n', '\tdata := []byte(nil)\n'),
]
for name, source, before, after in controls:
    source = source.resolve()
    original = source.read_text()
    if original.count(before) != 1:
        raise SystemExit(f'{name} no longer identifies one source site')
    replacement = out / f'{name}.go'
    replacement.write_text(original.replace(before, after))
    (out / f'{name}.json').write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
for control in combined-setup-budget no-kernel-pixels; do
  go test -race -overlay "$output/$control.json" -c -o "$output/$control.test" ./go/internal/agent/mcusource
done

run_fixture() {
  sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted WENDY_LITE_CAMERA_LOOPBACK=1 WENDY_TEST_LITE_CAMERA_LABEL="$camera_label" \
    python3 .github/scripts/run-owned-media-test.py --timeout 45 -- \
    "$1" -test.run "^${test_name}$" -test.v -test.count=1 -test.timeout=0
}
run_fixture "$output/mcusource.test" | tee "$output/normal.log"
grep -q "^--- PASS: $test_name " "$output/normal.log"
if grep -q '^--- SKIP:' "$output/normal.log"; then exit 1; fi
for control in combined-setup-budget no-kernel-pixels; do
  if run_fixture "$output/$control.test" > "$output/$control.log" 2>&1; then
    cat "$output/$control.log"
    echo "Lite kernel camera fixture did not reject $control" >&2
    exit 1
  fi
  cat "$output/$control.log"
  grep -q "^--- FAIL: $test_name " "$output/$control.log"
done
grep -q 'Lite stream ended before kernel camera became live:' "$output/combined-setup-budget.log"
grep -q 'V4L2 consumer did not decode ten frames:' "$output/no-kernel-pixels.log"
echo "Lite camera fixture logs: $output"
