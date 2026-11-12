#!/usr/bin/env bash
# Compare userspace packets with real TUN forwarding in disposable CI namespaces.
set -euo pipefail
if [[ ${1:-} != --inside ]]; then
  [[ $EUID == 0 && $# == 1 ]] || { echo 'usage: sudo test-packet-backends.sh TEST_BINARY' >&2; exit 2; }
  binary=$(realpath "$1")
  [[ -x $binary ]]
  export WENDY_PACKET_PARENT_mnt WENDY_PACKET_PARENT_net WENDY_PACKET_PARENT_pid
  WENDY_PACKET_PARENT_mnt=$(readlink /proc/self/ns/mnt)
  WENDY_PACKET_PARENT_net=$(readlink /proc/self/ns/net)
  WENDY_PACKET_PARENT_pid=$(readlink /proc/self/ns/pid)
  exec timeout 90 unshare --mount --net --pid --fork --propagation private --mount-proc \
    "$(realpath "$0")" --inside "$binary"
fi
[[ $EUID == 0 && $# == 2 ]]
[[ $(readlink /proc/self/ns/mnt) != "$WENDY_PACKET_PARENT_mnt" ]]
[[ $(readlink /proc/self/ns/net) != "$WENDY_PACKET_PARENT_net" ]]
[[ $(readlink /proc/self/ns/pid) != "$WENDY_PACKET_PARENT_pid" ]]
mount --make-rprivate /
mount -t tmpfs tmpfs /run
mount -t sysfs -o ro,nosuid,nodev,noexec sysfs /sys
ip link set lo up
export WENDY_PACKET_BACKEND_NAMESPACE=1
"$2" -test.run '^TestPacketBackendContract$' -test.v -test.count=1 -test.timeout=60s
