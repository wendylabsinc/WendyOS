#!/usr/bin/env bash
# Only the existing disposable Linux media runner may load this kernel fixture.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != true || "${RUNNER_ENVIRONMENT:-}" != github-hosted || "$(uname -s)" != Linux ]]; then
  echo 'SensorLink kernel audio fixture requires the disposable GitHub-hosted Linux runner.' >&2
  exit 2
fi
cd "$(git rev-parse --show-toplevel)"
for command in go python3 sudo modprobe pipewire pw-dump aplay arecord timeout; do
  command -v "$command" >/dev/null
done
if getent passwd wendy >/dev/null || getent group wendy >/dev/null || [[ -d /sys/module/snd_aloop ]]; then
  echo 'Refusing to reuse an existing wendy account/group or snd-aloop module.' >&2
  exit 2
fi
output="$(mktemp -d "${RUNNER_TEMP:?}/wendy-sensorlink-audio.XXXXXX")"
test_name=TestSensorLinkMicrophonesRecordKernelLoopbackWithLivePipeWire

# sudo must fail without prompting. GNU timeout owns its command group and
# escalates even when a query ignores TERM. Never leave a startup/cleanup
# command waiting for the outer job's 25-minute timeout.
run_root_bounded() {
  local duration="$1"
  shift
  sudo -n timeout --kill-after=1s "$duration" "$@"
}

# Compile before allocating any host resources. No production source is edited.
echo "Compiling SensorLink audio normal and regression binaries: $output"
go test -race -c -o "$output/services.test" ./go/internal/agent/services
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1])
source = Path('go/internal/agent/services/audio_service.go').resolve()
original = source.read_text()
early = 'if src, ok := sensorLinkCaptureSource(deviceID); ok {'
if original.count(early) != 2:
    raise SystemExit('SensorLink capture routing no longer has the two expected sites')
route = original.replace(early, 'if src, ok := sensorLinkCaptureSource(deviceID); false && ok {', 1)
before = 'return captureSource{alsaDevice: fmt.Sprintf("plughw:%d,%d,%d", card, device, sub)}, true'
if original.count(before) != 1:
    raise SystemExit('SensorLink subdevice selection no longer has one expected site')
crossed = original.replace(before, 'return captureSource{alsaDevice: fmt.Sprintf("plughw:%d,%d,%d", card, device, (sub+1)%2)}, true')
for name, content in [('pipewire-only', route), ('crossed-microphones', crossed)]:
    replacement = out / f'{name}.go'
    replacement.write_text(content)
    (out / f'{name}.json').write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
for control in pipewire-only crossed-microphones; do
  go test -race -overlay "$output/$control.json" -c -o "$output/$control.test" ./go/internal/agent/services
done

# The same owned snd-aloop fixture also verifies the missing authenticated
# Lite microphone hop: WendyCom -> Supervisor -> aplay -> kernel -> arecord.
microphone_test=TestLiteMicrophoneDeliversPCMThroughAuthenticatedKernelLoopback
go test -race -c -o "$output/lite-microphone.test" ./go/internal/agent/mcusource
python3 - "$output" <<'PY_MICROPHONE'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1])
source = Path('go/internal/agent/mcusource/supervisor.go').resolve()
original = source.read_text()
controls = {
    'lite-combined-budget': (
        'if connector, ok := tr.(interface{ Connect(context.Context) error }); ok {',
        'if connector, ok := tr.(interface{ Connect(context.Context) error }); false && ok {'),
    'lite-wrong-channel': (
        '} else if aw := audioWriters[f.ChannelID]; aw != nil {',
        '} else if aw := audioWriters[f.ChannelID+1]; aw != nil {'),
    'lite-no-pcm': ('aw.WritePCM(f.Payload)', 'aw.WritePCM(nil)'),
}
for name, (before, after) in controls.items():
    if original.count(before) != 1:
        raise SystemExit(f'{name} no longer identifies one source site')
    replacement = out / f'{name}.go'
    replacement.write_text(original.replace(before, after))
    (out / f'{name}.json').write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY_MICROPHONE
for control in lite-combined-budget lite-wrong-channel lite-no-pcm; do
  go test -race -overlay "$output/$control.json" -c -o "$output/$control.test" ./go/internal/agent/mcusource
done

created_user=0
loaded_module=0
daemon_launcher=
daemon_directory=
wendy_uid=
wendy_gid=
cleanup() {
  result=$?
  trap - EXIT
  trap '' INT TERM
  set +e
  echo "Draining owned SensorLink audio resources"
  if [[ -n "$wendy_uid" ]]; then
    # This fresh account owns only fixture processes. Bound graceful shutdown
    # before escalating by this owned UID, never by a reusable launcher PID.
    run_root_bounded 3s pkill -TERM -u "$wendy_uid"
    for _ in $(seq 1 30); do
      if ! run_root_bounded 3s pgrep -u "$wendy_uid" >/dev/null; then break; fi
      sleep 0.1
    done
    if run_root_bounded 3s pgrep -u "$wendy_uid" >/dev/null; then
      run_root_bounded 3s pkill -KILL -u "$wendy_uid"
    fi
  fi
  if [[ -n "$daemon_launcher" ]]; then
    # jobs consults bash's own child table. Do not blindly signal this numeric
    # PID or wait indefinitely for a stuck sudo/daemon launcher.
    for _ in $(seq 1 30); do
      if ! { jobs -pr; jobs -ps; } | grep -Fxq "$daemon_launcher"; then break; fi
      sleep 0.1
    done
    if { jobs -pr; jobs -ps; } | grep -Fxq "$daemon_launcher"; then
      echo 'Owned PipeWire launcher did not finish bounded cleanup.' >&2
      result=1
    else
      wait "$daemon_launcher"
    fi
  fi
  if [[ -n "$daemon_directory" ]]; then
    # mktemp reserved this exact private subtree; logs remain in $output.
    run_root_bounded 5s rm -rf -- "$daemon_directory" || result=1
  fi
  if [[ "$loaded_module" == 1 ]]; then
    run_root_bounded 5s modprobe -r snd-aloop || result=1
    test ! -d /sys/module/snd_aloop || result=1
  fi
  if [[ "$created_user" == 1 ]]; then
    run_root_bounded 5s userdel wendy || result=1
    # userdel may already have removed its private group. If present, require
    # the exact GID created by this fixture before deleting it.
    if current_group=$(getent group wendy); then
      if [[ $(cut -d: -f3 <<< "$current_group") == "$wendy_gid" ]]; then
        run_root_bounded 5s groupdel wendy || result=1
      else
        echo 'Refusing to remove a replacement wendy group.' >&2
        result=1
      fi
    fi
  fi
  rm -f "$output/"*.test
  echo "SensorLink audio fixture logs: $output"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
run_root_bounded 5s useradd --system --no-create-home --user-group --groups audio --shell /usr/sbin/nologin wendy
created_user=1
wendy_uid="$(id -u wendy)"
wendy_gid="$(id -g wendy)"
run_root_bounded 5s modprobe snd-aloop id=Loopback pcm_substreams=8
loaded_module=1
run_root_bounded 5s udevadm settle
# The fresh daemon UID cannot traverse private RUNNER_TEMP ancestors. Reserve
# only its runtime/config under /tmp; never relax runner-directory permissions.
daemon_directory="$(mktemp -d /tmp/wendy-sensorlink-pipewire.XXXXXX)"
run_root_bounded 5s install -d -m 700 -o "$wendy_uid" -g "$wendy_gid" "$daemon_directory/runtime"
mkdir -p "$daemon_directory/config/pipewire/pipewire.conf.d"
# A real ALSA source on the opposite cable is present in the private graph.
# It is deliberately not one of the device-1/subdevice-0,1 mounted microphones.
# No session manager auto-opens any other ALSA subdevice. The correct fix uses
# arecord for these microphones even though the real PipeWire session is up.
# Adapter configuration follows the upstream pipewire.conf ALSA source example:
# https://github.com/PipeWire/pipewire/blob/master/src/daemon/pipewire.conf.in
cat > "$daemon_directory/config/pipewire/pipewire.conf.d/90-loopback-fixture.conf" <<'CONF'
context.objects = [
  { factory = adapter
    args = {
      factory.name = api.alsa.pcm.source
      node.name = wendy-regression-unused-side
      node.description = "Loopback unused capture side"
      media.class = Audio/Source
      api.alsa.path = "hw:Loopback,0,7"
      audio.format = S16LE
      audio.rate = 48000
      audio.channels = 1
      audio.position = [ MONO ]
    }
  }
]
CONF
run_root_bounded 5s chown -R "$wendy_uid:$wendy_gid" "$daemon_directory"
# The runner owns the log file; sudo applies only to the daemon.
# shellcheck disable=SC2024
sudo -n -u wendy env XDG_CONFIG_HOME="$daemon_directory/config" XDG_RUNTIME_DIR="$daemon_directory/runtime" PIPEWIRE_RUNTIME_DIR="$daemon_directory/runtime" \
  pipewire > "$output/pipewire.log" 2>&1 &
daemon_launcher=$!
echo "Waiting for the private PipeWire graph (15-second startup deadline)"
ready=0
startup_deadline=$((SECONDS + 15))
while (( SECONDS < startup_deadline )); do
  # The runner owns this captured JSON; only pw-dump needs daemon access.
  # shellcheck disable=SC2024
  if run_root_bounded 2s test -S "$daemon_directory/runtime/pipewire-0" && \
    run_root_bounded 2s env PIPEWIRE_RUNTIME_DIR="$daemon_directory/runtime" XDG_RUNTIME_DIR="$daemon_directory/runtime" pw-dump > "$output/pw-dump.json" 2>> "$output/pw-dump.log"; then
    ready=1
    break
  fi
  if ! { jobs -pr; jobs -ps; } | grep -Fxq "$daemon_launcher"; then
    daemon_status=0
    wait "$daemon_launcher" || daemon_status=$?
    cat "$output/pipewire.log" >&2
    printf 'Owned PipeWire launcher exited during startup (status %s).\n' "$daemon_status" >&2
    exit 1
  fi
  sleep 0.1
done
if [[ "$ready" != 1 ]]; then
  cat "$output/pipewire.log"
  cat "$output/pw-dump.log" >&2 2>/dev/null || true
  echo 'Private PipeWire session did not become ready within its bounded startup window.' >&2
  exit 1
fi
echo "Private PipeWire graph is ready; enumerating kernel ALSA devices"
# These inventory files are runner-owned artifacts.
# shellcheck disable=SC2024
run_root_bounded 3s arecord -l > "$output/arecord-list.log" 2>&1
# shellcheck disable=SC2024
run_root_bounded 3s aplay -l > "$output/aplay-list.log" 2>&1
run_fixture() {
  echo "Running owned audio fixture: $(basename "$1")" >&2
  sudo -n env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted \
    WENDY_SENSORLINK_AUDIO_LOOPBACK=1 WENDY_TEST_PIPEWIRE_RUNTIME="$daemon_directory/runtime" \
    python3 .github/scripts/run-owned-media-test.py --timeout 60 -- "$1" \
      -test.run "^${2:-$test_name}$" -test.v -test.count=1 -test.timeout=0
}
run_fixture "$output/services.test" | tee "$output/normal.log"
grep -q "^--- PASS: $test_name " "$output/normal.log"
if grep -q '^--- SKIP:' "$output/normal.log"; then exit 1; fi
for control in pipewire-only crossed-microphones; do
  if run_fixture "$output/$control.test" > "$output/$control.log" 2>&1; then
    cat "$output/$control.log"
    echo "SensorLink kernel recording did not reject $control" >&2
    exit 1
  fi
  cat "$output/$control.log"
  grep -q "^--- FAIL: $test_name " "$output/$control.log"
done
grep -q 'live SensorLink recording could not start:' "$output/pipewire-only.log"
grep -Eq 'kernel recording (crossed microphone identities|did not preserve exact microphone PCM)' "$output/crossed-microphones.log"

# No extra module, account, daemon or runner: the authenticated source uses the
# already reserved audio kernel fixture after the direct capture regressions.
run_fixture "$output/lite-microphone.test" "$microphone_test" | tee "$output/lite-microphone.log"
grep -q "^--- PASS: $microphone_test " "$output/lite-microphone.log"
if grep -q '^--- SKIP:' "$output/lite-microphone.log"; then exit 1; fi
for control in lite-combined-budget lite-wrong-channel lite-no-pcm; do
  if run_fixture "$output/$control.test" "$microphone_test" > "$output/$control.log" 2>&1; then
    cat "$output/$control.log"
    echo "Authenticated Lite microphone did not reject $control" >&2
    exit 1
  fi
  cat "$output/$control.log"
  grep -q "^--- FAIL: $microphone_test " "$output/$control.log"
done
grep -q 'Lite microphone ended before kernel capture became live:' "$output/lite-combined-budget.log"
grep -q 'Lite microphone did not deliver to its kernel playback writer' "$output/lite-wrong-channel.log"
grep -Eq 'kernel microphone (consumer did not record two seconds|did not preserve fresh authenticated PCM)' "$output/lite-no-pcm.log"
