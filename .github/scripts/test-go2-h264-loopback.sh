#!/usr/bin/env bash
# Existing Linux media job only, with its already-owned v4l2loopback module.
# Synthetic H264 proves decoder/kernel/OCI mechanics, not physical firmware.
set -euo pipefail
if [[ ${GITHUB_ACTIONS:-} != true || ${RUNNER_ENVIRONMENT:-} != github-hosted || $(uname -s) != Linux ]]; then
  echo 'Go2 H264 fixture requires the disposable hosted Linux media job.' >&2
  exit 2
fi
cd "$(git rev-parse --show-toplevel)"
if [[ ${1:-} == --inside ]]; then
  [[ $EUID == 0 && $# == 4 ]]
  [[ $(readlink /proc/self/ns/pid) != "$4" ]]
  # Private runtime paths prevent the disposable daemon/shims from sharing
  # host sockets. No host service or network configuration is changed.
  mount --make-rprivate /
  mount -t tmpfs -o mode=755 tmpfs /run
  export XDG_RUNTIME_DIR=/run/go2-runtime
  mkdir -m700 "$XDG_RUNTIME_DIR"
  export WENDY_GO2_H264_KERNEL=1 WENDY_GO2_CAPTURE_BINARY=$3
  bash .github/scripts/test-containerd-regressions.sh "$2" '^TestGo2H264KernelServiceAndContainerDelivery$'
  exit
fi
[[ $# == 0 ]]
[[ -c /dev/v4l2loopback && $(cat /sys/module/v4l2loopback/version) == 0.15.4 ]]
[[ ! -e /dev/video128 && ! -L /dev/video128 ]]
for command in go python3 sudo unshare timeout containerd ctr runc busybox gst-launch-1.0; do command -v "$command" >/dev/null; done
for element in videotestsrc x264enc h264parse avdec_h264 videoconvert fdsrc fdsink; do gst-inspect-1.0 "$element" >/dev/null; done
output=$(mktemp -d "${RUNNER_TEMP:?}/wendy-go2-h264.XXXXXX")
trap 'rm -f "$output/"*.test; echo "Go2 H264 logs: $output"' EXIT
export WENDY_GO2_DECODE_FIXTURE=1
python3 .github/scripts/run-owned-media-test.py --timeout 180 -- \
  go test -race ./go/internal/agent/ros2camera -run '^TestGo2ResolutionH264DecodesGeneratedPixels$' -v -count=1 | tee "$output/generated.log"
grep -q '^--- PASS: TestGo2ResolutionH264DecodesGeneratedPixels ' "$output/generated.log"
CGO_ENABLED=0 go test -tags regression_go2_h264 -c ./go/internal/agent/services -o "$output/capture.test"
CGO_ENABLED=0 go test -tags regression_go2_h264 -c ./go/internal/agent/containerd -o "$output/normal.test"
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out=Path(sys.argv[1])
for name,path,before,after in [
 ('old-cdr','go/internal/agent/ros2camera/frame.go','return decodeGo2Frame(payload)', 'data, width, height, err := decodeGo2FrontVideo(payload)\n\t\treturn Frame{Data: data, Width: width, Height: height, Codec: CodecMJPEG}, err'),
 ('wrong-format','go/internal/agent/ros2camera/writer_linux.go','return v4l2PixFmtH264, nil','return v4l2PixFmtMJPEG, nil'),
 ('empty-camera-label','go/internal/shared/appconfig/annotation.go','value = "enabled=true"','value = ""')]:
 source=Path(path).resolve();original=source.read_text()
 if original.count(before)!=1:raise SystemExit('mutation site changed: '+name)
 replacement=out/(name+'.go');replacement.write_text(original.replace(before,after))
 (out/(name+'.json')).write_text(json.dumps({'Replace':{str(source):str(replacement)}}))
PY
if python3 .github/scripts/run-owned-media-test.py --timeout 180 -- \
  go test -race -overlay "$output/old-cdr.json" ./go/internal/agent/ros2camera -run '^TestGo2ResolutionH264DecodesGeneratedPixels$' -v -count=1 > "$output/old-cdr.log" 2>&1; then
  cat "$output/old-cdr.log"; echo 'Old CDR decoder unexpectedly passed' >&2; exit 1
fi
grep -q 'Go2 resolution H264 decode failed:' "$output/old-cdr.log"
grep -q '^--- FAIL: TestGo2ResolutionH264DecodesGeneratedPixels ' "$output/old-cdr.log"
for variant in wrong-format empty-camera-label; do
  CGO_ENABLED=0 go test -tags regression_go2_h264 -overlay "$output/$variant.json" -c ./go/internal/agent/containerd -o "$output/$variant.test"
done
host_pid_namespace=$(readlink /proc/self/ns/pid)
run_kernel() {
  local label result=0
  label="ci-go2-$(python3 -c 'import uuid; print(uuid.uuid4().hex[:12])')"
  [[ ! -e /dev/video128 && ! -L /dev/video128 ]] || return 2
  sudo env PATH="$PATH" GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted WENDY_GO2_CAMERA_LABEL="$label" \
    python3 .github/scripts/run-owned-media-test.py --timeout 90 -- \
    unshare --mount --pid --fork --kill-child=KILL --mount-proc \
    bash .github/scripts/test-go2-h264-loopback.sh --inside "$1" "$output/capture.test" "$host_pid_namespace" || result=$?
  # A normal waited namespace exit drained its PID1 and children. On a signal
  # or watchdog, keep the owned node for disposable-runner teardown instead of
  # racing uncertain pending users of its device descriptor.
  if (( result >= 124 )); then echo 'Go2 namespace drain uncertain; refusing node cleanup.' >&2; return "$result"; fi
  sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted \
    python3 .github/scripts/cleanup-owned-go2-camera.py --label "$label" || return 125
  return "$result"
}
run_kernel "$output/normal.test" 2>&1 | tee "$output/normal.log"
grep -q '^--- PASS: TestGo2H264KernelServiceAndContainerDelivery ' "$output/normal.log"
grep -q 'GO2_KERNEL_CONTAINER_DECODE_VERIFIED' "$output/normal.log"
for variant in wrong-format empty-camera-label; do
  if run_kernel "$output/$variant.test" > "$output/$variant.log" 2>&1; then
    cat "$output/$variant.log"; echo "Go2 mutation unexpectedly passed: $variant" >&2; exit 1
  fi
  cat "$output/$variant.log"
  grep -q '^--- FAIL: TestGo2H264KernelServiceAndContainerDelivery ' "$output/$variant.log"
  if [[ $variant == wrong-format ]]; then
    grep -q 'Go2 writer kernel format is not H264 640x360' "$output/$variant.log"
  else
    grep -q 'Go2 camera entitlement did not retain active consumer' "$output/$variant.log"
  fi
  [[ ! -e /dev/video128 && ! -L /dev/video128 ]]
done
if grep -q '^--- SKIP:' "$output/"*.log; then echo 'Go2 required test skipped' >&2; exit 1; fi
