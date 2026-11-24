#!/usr/bin/env bash
# Use the v4l2loopback 0.15.4 module already loaded by the Linux media job.
# The tests create/remove only their own nodes: the camera fixture below 96 and
# the share nodes the agent's own allocator picks. They do not load any module.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != true || "${RUNNER_ENVIRONMENT:-}" != github-hosted || "$(uname -s)" != Linux ]]; then
  echo 'Camera sharing fixture requires the disposable GitHub-hosted Linux runner.' >&2
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
# The second list is what deriving pictures and H.264 from a camera's own frames
# needs; without it an app's native or h264 node is served as pixels.
for element in v4l2src jpegdec videoconvert queue fdsink \
  fdsrc gdpdepay gdppay tee x264enc h264parse; do
  gst-inspect-1.0 "$element" > /dev/null
done
if ! command -v unshare > /dev/null; then
  echo 'The devcover tests need unshare (util-linux).' >&2
  exit 2
fi
output="$(mktemp -d "${RUNNER_TEMP:?}/wendy-camera-sharing.XXXXXX")"
trap 'rm -f "$output/"*.test' EXIT
services=./go/internal/agent/services
devcover=./go/internal/agent/devcover
share_test=TestCameraShareTwoReadersDecodeRealFrames
share_tests=(TestCameraShareTwoReadersDecodeRealFrames TestShareNodeWriter_DeclaresModeAndReportsReaders TestShareNode_StaysACameraWhileTheWriterIsAway TestShareNode_FreshStartForLateAndRestartedReaders TestShareNode_FreshStartSuppressesSyntheticFrames TestShareNode_FreshStartForReadIO TestCameraShareNodesFollowTheirEntitlements TestShareNodeWriter_CompressedNodeCarriesWholeFramesOfAnyLength TestShareNodeWriter_BufferTimestampReadsBackAsTheCaptureTimeOrTheSampleID)
cover_test=TestCover_BindsSourceInsideTargetNamespaceOnly

python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1]).resolve()
mutations = {
    # The policy refuses every camera: no container shares one.
    'sharing-disabled': ('go/internal/agent/services/camera_share_policy.go',
                         'return shareDecision{Shared: true, Mode: mode}',
                         'return shareDecision{Reason: "sharing disabled"}'),
    # Without fresh_start, reopen exposes the prior session's retained picture.
    'fresh-start-disabled': ('go/internal/agent/services/camera_share_node.go',
                             'fresh := v4l2Control{ID: v4l2LoopbackCidFreshStart, Value: 1}',
                             'fresh := v4l2Control{ID: v4l2LoopbackCidFreshStart, Value: 0}'),
    # A cover never enters the container's mount namespace.
    'no-setns': ('go/internal/agent/devcover/cover_linux.go',
                 'unix.Setns(n.fd, unix.CLONE_NEWNS)',
                 'error(nil)'),
}
for name, (path, before, after) in mutations.items():
    source = Path(path).resolve()
    original = source.read_text()
    if original.count(before) != 1:
        raise SystemExit(f'{name}: source mutation no longer identifies exactly one site in {path}: '
                         f'found {original.count(before)} of {before!r}')
    replacement = out / f'{name}.go'
    replacement.write_text(original.replace(before, after))
    (out / f'{name}.json').write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
go test -race -tags regression_camera_fixture -c -o "$output/services.test" "$services"
go test -race -tags regression_camera_fixture -overlay "$output/sharing-disabled.json" -c -o "$output/sharing-disabled.test" "$services"
go test -race -tags regression_camera_fixture -overlay "$output/fresh-start-disabled.json" -c -o "$output/fresh-start-disabled.test" "$services"
go test -race -tags regression_camera_fixture -c -o "$output/devcover.test" "$devcover"
go test -race -tags regression_camera_fixture -overlay "$output/no-setns.json" -c -o "$output/no-setns.test" "$devcover"

run_share() {
  sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted WENDY_GSTREAMER_REQUIRED=1 \
    python3 .github/scripts/run-owned-media-test.py --timeout 300 -- \
    "$1" -test.run "$2" -test.v -test.count=1 -test.timeout=0
}
# In a private mount namespace of its own: a cover that misses the target
# namespace (the no-setns control) mounts there, never on the runner's host.
# --propagation private matters as much as --mount: the runner's / is a shared
# mount (systemd makes it one), and a new namespace's mounts start as peers of
# it, so a mount there would propagate back to the host's /dev. It is
# unshare's default for --mount since util-linux 2.27; it is spelled out so the
# control's safety does not rest on that default.
run_cover() {
  sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted WENDY_DEVCOVER_REQUIRED=1 \
    python3 .github/scripts/run-owned-media-test.py --timeout 120 -- \
    unshare --mount --propagation private -- "$1" -test.run "$2" -test.v -test.count=1 -test.timeout=0
}
require_passed() {
  local log=$1
  shift
  for test_name in "$@"; do
    if ! grep -q "^--- PASS: $test_name " "$log"; then
      echo "$test_name did not pass" >&2
      exit 1
    fi
  done
  # Subtests report their skips indented.
  if grep -Eq '^[[:space:]]*--- SKIP:' "$log"; then
    echo 'A required camera sharing regression was skipped' >&2
    exit 1
  fi
}
require_rejected() {
  local log=$1 mutation=$2 test_name=$3 message=$4
  cat "$log"
  grep -q "^--- FAIL: $test_name " "$log" || { echo "Regression did not reject $mutation" >&2; exit 1; }
  grep -qF "$message" "$log" || { echo "Regression rejected $mutation without: $message" >&2; exit 1; }
}

run_share "$output/services.test" '^Test(CameraShareTwoReadersDecodeRealFrames|ShareNodeWriter_DeclaresModeAndReportsReaders|ShareNode_StaysACameraWhileTheWriterIsAway|ShareNode_FreshStartForLateAndRestartedReaders|ShareNode_FreshStartSuppressesSyntheticFrames|ShareNode_FreshStartForReadIO|CameraShareNodesFollowTheirEntitlements|ShareNodeWriter_CompressedNodeCarriesWholeFramesOfAnyLength|ShareNodeWriter_BufferTimestampReadsBackAsTheCaptureTimeOrTheSampleID)$' | tee "$output/normal.log"
require_passed "$output/normal.log" "${share_tests[@]}"

if run_share "$output/sharing-disabled.test" "^${share_test}\$" > "$output/sharing-disabled.log" 2>&1; then
  cat "$output/sharing-disabled.log"
  echo 'Regression did not reject sharing-disabled' >&2
  exit 1
fi
require_rejected "$output/sharing-disabled.log" sharing-disabled "$share_test" 'camera was not shared: sharing disabled'

fresh_test=TestShareNodeWriter_DeclaresModeAndReportsReaders
if run_share "$output/fresh-start-disabled.test" "^${fresh_test}\$" > "$output/fresh-start-disabled.log" 2>&1; then
  cat "$output/fresh-start-disabled.log"
  echo 'Regression did not reject fresh-start-disabled' >&2
  exit 1
fi
require_rejected "$output/fresh-start-disabled.log" fresh-start-disabled "$fresh_test" 'FRESH_CAPTURE_CONTRACT:'

mapfile -t cover_tests < <("$output/devcover.test" -test.list '.*' | grep '^Test')
if [[ ${#cover_tests[@]} -eq 0 ]]; then
  echo 'devcover.test lists no tests' >&2
  exit 1
fi
run_cover "$output/devcover.test" '.*' | tee "$output/devcover.log"
require_passed "$output/devcover.log" "${cover_tests[@]}"

if run_cover "$output/no-setns.test" "^${cover_test}\$" > "$output/no-setns.log" 2>&1; then
  cat "$output/no-setns.log"
  echo 'Regression did not reject no-setns' >&2
  exit 1
fi
require_rejected "$output/no-setns.log" no-setns "$cover_test" "cover reached the agent's own mount namespace"
echo "Camera sharing fixture logs: $output"
