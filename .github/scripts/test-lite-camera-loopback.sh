#!/usr/bin/env bash
# Use the v4l2loopback 0.15.4 module already loaded by the Linux media job.
# This script creates/removes only its test node; it does not load any module.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != true || "${RUNNER_ENVIRONMENT:-}" != github-hosted || "$(uname -s)" != Linux ]]; then
  echo 'Lite kernel camera fixture requires the disposable GitHub-hosted Linux runner.' >&2
  exit 2
fi
cd "$(git rev-parse --show-toplevel)"
test -c /dev/v4l2loopback
test "$(cat /sys/module/v4l2loopback/version)" = 0.15.4
for element in v4l2src jpegdec videoconvert fdsink; do
  gst-inspect-1.0 "$element" > /dev/null
done
output="$(mktemp -d "${RUNNER_TEMP:?}/wendy-lite-camera.XXXXXX")"
test_name=TestLiteCameraDeliversDecodedPixelsThroughKernelLoopback
go test -race -c -o "$output/mcusource.test" ./go/internal/agent/mcusource
trap 'python3 -c '\''from pathlib import Path; import sys; [p.unlink(missing_ok=True) for p in Path(sys.argv[1]).glob("*.test")]'\'' "$output"' EXIT

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
  sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted WENDY_LITE_CAMERA_LOOPBACK=1 \
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
