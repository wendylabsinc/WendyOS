#!/usr/bin/env bash
# Audit 2261: real broker TLS streams during a kernel preferred-source change.
# veth links model uplinks. They do not emulate radio roaming or physical sysfs.
set -euo pipefail
if [[ ${GITHUB_ACTIONS:-} != true || ${RUNNER_ENVIRONMENT:-} != github-hosted || $(uname -s) != Linux ]]; then
  echo 'Broker uplink fixture requires the disposable GitHub-hosted Linux runner.' >&2
  exit 2
fi
if [[ ${1:-} == --namespace ]]; then
  [[ $EUID == 0 && $# == 2 ]] || exit 2
  binary=$(realpath "$2")
  [[ -x $binary ]] || exit 2
  export WENDY_UPLINK_PARENT_mnt WENDY_UPLINK_PARENT_net WENDY_UPLINK_PARENT_pid
  WENDY_UPLINK_PARENT_mnt=$(readlink /proc/self/ns/mnt)
  WENDY_UPLINK_PARENT_net=$(readlink /proc/self/ns/net)
  WENDY_UPLINK_PARENT_pid=$(readlink /proc/self/ns/pid)
  exec timeout --kill-after=3s 100s unshare --mount --net --pid --fork --propagation private --mount-proc \
    "$(realpath "$0")" --inside "$binary"
fi
if [[ ${1:-} == --inside ]]; then
  [[ $EUID == 0 && $# == 2 ]] || exit 2
  [[ -n ${WENDY_UPLINK_PARENT_mnt:-} && $(readlink /proc/self/ns/mnt) != "$WENDY_UPLINK_PARENT_mnt" ]] || exit 2
  [[ -n ${WENDY_UPLINK_PARENT_net:-} && $(readlink /proc/self/ns/net) != "$WENDY_UPLINK_PARENT_net" ]] || exit 2
  [[ -n ${WENDY_UPLINK_PARENT_pid:-} && $(readlink /proc/self/ns/pid) != "$WENDY_UPLINK_PARENT_pid" ]] || exit 2
  mount --make-rprivate /
  mount -t tmpfs tmpfs /run
  mount -t sysfs -o ro,nosuid,nodev,noexec sysfs /sys
  ip link set lo up
  sysctl -q -w net.ipv4.ip_forward=0 net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0
  ip netns add broker
  ip netns exec broker ip link set lo up
  ip netns exec broker ip address add 203.0.113.1/32 dev lo
  ip netns exec broker sysctl -q -w net.ipv4.ip_forward=0 net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0
  for entry in 'wifi0 1' 'wired0 2'; do
    read -r name subnet <<< "$entry"
    ip link add "$name" type veth peer name "peer$subnet"
    ip link set "peer$subnet" netns broker
    ip address add "10.81.$subnet.1/24" dev "$name"
    ip link set "$name" up
    sysctl -q -w "net.ipv4.conf.$name.rp_filter=0"
    ip netns exec broker ip address add "10.81.$subnet.2/24" dev "peer$subnet"
    ip netns exec broker ip link set "peer$subnet" up
    ip netns exec broker sysctl -q -w "net.ipv4.conf.peer$subnet.rp_filter=0"
  done
  ip route add 203.0.113.1/32 via 10.81.1.2 dev wifi0 src 10.81.1.1 metric 10
  ip route add 203.0.113.1/32 via 10.81.2.2 dev wired0 src 10.81.2.1 metric 20
  export WENDY_UPLINK_NAMESPACE=1
  unset WENDY_UPLINK_PEER WENDY_UPLINK_KEYS
  # PID-namespace destruction also drains the broker helper after a panic or
  # watchdog expiry. /run/netns and every created interface exist only here.
  exec "$2" -test.run '^TestBrokerMovesToPreferredUplinkAfterSessionDrains$' -test.v -test.count=1 -test.timeout=90s
fi
[[ $# == 1 && $EUID != 0 ]] || { echo 'usage: test-broker-uplink-namespace.sh NEW_OUTPUT_DIRECTORY (normal runner user)' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
script=$(realpath "$0")
package=./go/internal/agent/services
regression=TestBrokerMovesToPreferredUplinkAfterSessionDrains
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out = Path(sys.argv[1])
for name, before, after in (
    ('no-migration', 'return src, !src.Equal(local.IP)', 'return src, !src.Equal(local.IP) && false'),
    ('ignore-active-session', 'return sessions > 0 || queued > 0 || sinceHeartbeat < brokerHeartbeatSettle', 'return sinceHeartbeat < brokerHeartbeatSettle'),
):
    source = Path('go/internal/agent/services/broker_path.go').resolve()
    original = source.read_text()
    assert original.count(before) == 1, f'{name} source site changed'
    replacement = out / (name + '.go')
    replacement.write_text(original.replace(before, after))
    (out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
for variant in normal no-migration ignore-active-session; do
  overlay=()
  if [[ $variant != normal ]]; then overlay=(-overlay="$output/$variant.json"); fi
  go test -race -tags regression_broker_uplink "${overlay[@]}" -c -o "$output/$variant.test" "$package"
  status=0
  # The invoking runner owns logs; elevated commands operate only in fresh namespaces.
  # shellcheck disable=SC2024
  sudo -n env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted "$script" --namespace "$output/$variant.test" > "$output/$variant.log" 2>&1 || status=$?
  cat "$output/$variant.log"
  if [[ $variant == normal ]]; then
    [[ $status == 0 ]] || exit 1
    grep -q "^--- PASS: $regression " "$output/$variant.log"
    grep -q 'held exact-byte TLS tunnel survived preferred uplink change' "$output/$variant.log"
    if grep -q -- '--- SKIP:' "$output/$variant.log"; then exit 1; fi
  else
    [[ $status != 0 ]] || exit 1
    grep -q "^--- FAIL: $regression " "$output/$variant.log"
    case "$variant" in
      no-migration) grep -q 'idle broker did not migrate to kernel-preferred wired source' "$output/$variant.log" ;;
      ignore-active-session) grep -Eq 'held-across-route-change: established tunnel lost exact bytes|busy uplink migration interrupted established session' "$output/$variant.log" ;;
    esac
  fi
  rm "$output/$variant.test"
done
