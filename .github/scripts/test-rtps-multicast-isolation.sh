#!/usr/bin/env bash
# Audit2022/95f6c1c52: actual RTPS packet exclusion on separate Linux interfaces.
set -euo pipefail
if [[ ${GITHUB_ACTIONS:-} != true || ${RUNNER_ENVIRONMENT:-} != github-hosted || $(uname -s) != Linux ]]; then
  echo 'RTPS interface fixture requires the disposable GitHub-hosted Linux runner.' >&2
  exit 2
fi
if [[ ${1:-} == --namespace ]]; then
  [[ $EUID == 0 && $# == 2 && -x $2 ]] || exit 2
  export WENDY_RTPS_PARENT_mnt WENDY_RTPS_PARENT_net WENDY_RTPS_PARENT_pid
  WENDY_RTPS_PARENT_mnt=$(readlink /proc/self/ns/mnt)
  WENDY_RTPS_PARENT_net=$(readlink /proc/self/ns/net)
  WENDY_RTPS_PARENT_pid=$(readlink /proc/self/ns/pid)
  exec timeout --signal=TERM --kill-after=3s 25s unshare --mount --net --pid --fork --kill-child=KILL --propagation private --mount-proc \
    "$(realpath "$0")" --inside "$(realpath "$2")"
fi
if [[ ${1:-} == --inside ]]; then
  [[ $EUID == 0 && $# == 2 ]] || exit 2
  for kind in mnt net pid; do
    variable="WENDY_RTPS_PARENT_$kind"
    [[ -n ${!variable:-} && $(readlink "/proc/self/ns/$kind") != "${!variable}" ]] || exit 2
  done
  mount --make-rprivate /
  mount -t tmpfs tmpfs /run
  ip link set lo up multicast on
  sysctl -q -w net.ipv4.ip_forward=0
  ip netns add wire-peer
  ip link add wire0 type veth peer name peer0
  ip link set peer0 netns wire-peer
  ip address add 10.83.0.1/24 dev wire0
  ip link set wire0 up multicast on
  ip netns exec wire-peer ip link set lo up
  ip netns exec wire-peer ip address add 10.83.0.2/24 dev peer0
  ip netns exec wire-peer ip link set peer0 up multicast on
  export WENDY_RTPS_ISOLATION=1
  unset WENDY_RTPS_WIRE_PEER
  # PID namespace teardown owns the peer helper even after test panic/watchdog.
  exec "$2" -test.run '^TestMulticastPacketsStayOnTheirJoinedInterface$' -test.v -test.count=1 -test.timeout=20s
fi
[[ $# == 1 && $EUID != 0 ]] || { echo 'usage: test-rtps-multicast-isolation.sh NEW_OUTPUT_DIRECTORY (runner user)' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
script=$(realpath "$0")
python3 - "$output" <<'PY'
import hashlib,json
from pathlib import Path
import sys
out=Path(sys.argv[1])
source=Path('go/internal/rtps/participant.go').resolve()
before='\tif err := restrictMulticastToJoinedInterfaces(mc); err != nil {\n\t\t_ = mc.Close()\n\t\treturn fmt.Errorf("rtps: restricting multicast memberships: %w", err)\n\t}\n'
text=source.read_text()
assert text.count(before)==1,'review production multicast admission site'
mutant=out/'participant.go'
mutant.write_text(text.replace(before,''))
(out/'unrestricted.json').write_text(json.dumps({'Replace':{str(source):str(mutant)}}))
(out/'source.json').write_text(json.dumps({'historical_fix':'95f6c1c52','path':str(source),'sha256':hashlib.sha256(source.read_bytes()).hexdigest(),'mutation':'remove only multicast restriction in production openSockets','mutant_sha256':hashlib.sha256(mutant.read_bytes()).hexdigest()},indent=2)+'\n')
PY
for variant in normal unrestricted; do
  overlay=()
  if [[ $variant == unrestricted ]]; then overlay=(-overlay="$output/unrestricted.json"); fi
  go test -race "${overlay[@]}" -c -o "$output/$variant.test" ./go/internal/rtps
  status=0
  # shellcheck disable=SC2024
  sudo -n env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted "$script" --namespace "$output/$variant.test" > "$output/$variant.log" 2>&1 || status=$?
  cat "$output/$variant.log"
  if grep -Eq -- '--- SKIP:|WARNING: DATA RACE' "$output/$variant.log"; then exit 1; fi
  if [[ $variant == normal ]]; then
    [[ $status == 0 ]] || exit 1
    grep -q '^--- PASS: TestMulticastPacketsStayOnTheirJoinedInterface ' "$output/$variant.log"
    grep -q 'RTPS_INTERFACE_PACKET_ISOLATION_VERIFIED' "$output/$variant.log"
  else
    [[ $status == 1 ]] || { echo "control must fail normally, not timeout or setup: $status" >&2; exit 1; }
    grep -q '^--- FAIL: TestMulticastPacketsStayOnTheirJoinedInterface ' "$output/$variant.log"
    grep -q 'CROSS_INTERFACE_DISCOVERY:' "$output/$variant.log"
  fi
  # Only a normal awaited namespace exit is accepted above. Namespace-owned
  # interfaces/mounts and descendants are gone; no host cleanup command runs.
  rm "$output/$variant.test"
done
