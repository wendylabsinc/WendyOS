#!/usr/bin/env bash
# Audit2224 Linux only: actual veth cables and overlapping kernel routes.
set -euo pipefail
if [[ ${GITHUB_ACTIONS:-} != true || ${RUNNER_ENVIRONMENT:-} != github-hosted || $(uname -s) != Linux ]]; then
  echo 'Link-local multi-interface fixture requires the disposable GitHub-hosted Linux runner.' >&2
  exit 2
fi
if [[ ${1:-} == --namespace ]]; then
  [[ $EUID == 0 && $# == 2 ]] || exit 2
  binary=$(realpath "$2")
  [[ -x $binary ]] || exit 2
  export WENDY_LINKLOCAL_PARENT_mnt WENDY_LINKLOCAL_PARENT_net WENDY_LINKLOCAL_PARENT_pid
  WENDY_LINKLOCAL_PARENT_mnt=$(readlink /proc/self/ns/mnt)
  WENDY_LINKLOCAL_PARENT_net=$(readlink /proc/self/ns/net)
  WENDY_LINKLOCAL_PARENT_pid=$(readlink /proc/self/ns/pid)
  exec timeout --kill-after=3s 45s unshare --mount --net --pid --fork --propagation private --mount-proc \
    "$(realpath "$0")" --inside "$binary"
fi
if [[ ${1:-} == --inside ]]; then
  # Compared before the EUID check so the unprivileged admission test reaches
  # this refusal; reading /proc/self/ns needs no privilege.
  shared() { echo 'Link-local multi-interface fixture refuses namespaces shared with its parent.' >&2; exit 2; }
  [[ -n ${WENDY_LINKLOCAL_PARENT_mnt:-} && $(readlink /proc/self/ns/mnt) != "$WENDY_LINKLOCAL_PARENT_mnt" ]] || shared
  [[ -n ${WENDY_LINKLOCAL_PARENT_net:-} && $(readlink /proc/self/ns/net) != "$WENDY_LINKLOCAL_PARENT_net" ]] || shared
  [[ -n ${WENDY_LINKLOCAL_PARENT_pid:-} && $(readlink /proc/self/ns/pid) != "$WENDY_LINKLOCAL_PARENT_pid" ]] || shared
  [[ $EUID == 0 && $# == 2 ]] || exit 2
  mount --make-rprivate /
  mount -t tmpfs tmpfs /run
  mount -t sysfs -o ro,nosuid,nodev,noexec sysfs /sys
  ip link set lo up
  sysctl -q -w net.ipv4.ip_forward=0 net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0
  # Each isolated peer owns exactly one cable. The host has overlapping /16
  # routes with different metrics; no /32 route can hide the original bug.
  for entry in 'a 0 100 10' 'b 1 200 20'; do
    read -r name number subnet metric <<< "$entry"
    ip netns add "peer-$name"
    ip link add "wusb$number" type veth peer name "device$number"
    ip link set "device$number" netns "peer-$name"
    ip address add "169.254.$subnet.1/16" dev "wusb$number" noprefixroute
    ip link set "wusb$number" up
    ip route add 169.254.0.0/16 dev "wusb$number" src "169.254.$subnet.1" metric "$metric"
    sysctl -q -w "net.ipv4.conf.wusb$number.rp_filter=0"
    ip netns exec "peer-$name" sysctl -q -w net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0
    ip netns exec "peer-$name" ip link set lo up
    ip netns exec "peer-$name" ip address add "169.254.$subnet.2/16" dev "device$number"
    ip netns exec "peer-$name" ip link set "device$number" up
    ip netns exec "peer-$name" sysctl -q -w "net.ipv4.conf.device$number.rp_filter=0"
  done
  export WENDY_LINKLOCAL_NAMESPACE=1
  unset WENDY_LINKLOCAL_PEER WENDY_LINKLOCAL_PEER_ADDRESS
  exec "$2" -test.run '^TestLinkLocalTwoInterfacesKeepCorrectDeviceAndReplyPath$' -test.v -test.count=1 -test.timeout=35s
fi
[[ $# == 1 ]] || { echo 'usage: test-linklocal-multi-interface.sh NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
mkdir "$1"
output=$(realpath "$1")
script=$(realpath "$0")
package=./go/internal/cli/linklocal
regression=TestLinkLocalTwoInterfacesKeepCorrectDeviceAndReplyPath
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out = Path(sys.argv[1])
for name, path, before, after in (
    ('unbound-dials', 'bind_linux.go', 'unix.SO_BINDTODEVICE, ifi.Name)', 'unix.SO_BINDTODEVICE, "")'),
    ('unbound-listeners', 'linklocal.go', 'lc := net.ListenConfig{Control: pinTo(l)}', 'lc := net.ListenConfig{}'),
):
    source = Path('go/internal/cli/linklocal', path).resolve()
    original = source.read_text()
    assert original.count(before) == 1, f'{name} source site changed'
    replacement = out / (name + '.go')
    replacement.write_text(original.replace(before, after))
    (out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
for variant in normal unbound-dials unbound-listeners; do
  overlay=()
  if [[ $variant != normal ]]; then overlay=(-overlay="$output/$variant.json"); fi
  go test -race "${overlay[@]}" -c -o "$output/$variant.test" "$package"
  status=0
  # Logs are intentionally written by the runner; sudo owns only the namespace.
  # shellcheck disable=SC2024
  sudo -n env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted "$script" --namespace "$output/$variant.test" > "$output/$variant.log" 2>&1 || status=$?
  cat "$output/$variant.log"
  if [[ $variant == normal ]]; then
    [[ $status == 0 ]] || exit 1
    grep -q "^--- PASS: $regression " "$output/$variant.log"
    grep -q 'two overlapping link-local cables retained exact peers' "$output/$variant.log"
    if grep -q -- '--- SKIP:' "$output/$variant.log"; then exit 1; fi
  else
    [[ $status != 0 ]] || exit 1
    grep -q "^--- FAIL: $regression " "$output/$variant.log"
    case "$variant" in
      unbound-dials) grep -q 'wrong-route device b was unreachable through production link-local dial:' "$output/$variant.log" ;;
      unbound-listeners) grep -q 'wrong-route listener reply did not return on device b interface:' "$output/$variant.log" ;;
    esac
  fi
  rm "$output/$variant.test"
done
