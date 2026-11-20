#!/usr/bin/env bash
# All interfaces and senders live in disposable CI namespaces. The test observes
# verified time proofs through the clock-write seam and never changes wall time.
set -euo pipefail
if [[ ${1:-} != --inside ]]; then
  [[ $EUID == 0 && $# == 1 ]] || { echo 'usage: sudo test-multicast-interfaces.sh TEST_BINARY' >&2; exit 2; }
  binary=$(realpath "$1")
  [[ -x $binary ]]
  export WENDY_MULTICAST_PARENT_mnt WENDY_MULTICAST_PARENT_net WENDY_MULTICAST_PARENT_pid
  WENDY_MULTICAST_PARENT_mnt=$(readlink /proc/self/ns/mnt)
  WENDY_MULTICAST_PARENT_net=$(readlink /proc/self/ns/net)
  WENDY_MULTICAST_PARENT_pid=$(readlink /proc/self/ns/pid)
  exec timeout 120 unshare --mount --net --pid --fork --propagation private --mount-proc \
    "$(realpath "$0")" --inside "$binary"
fi
[[ $EUID == 0 && $# == 2 ]]
[[ $(readlink /proc/self/ns/mnt) != "$WENDY_MULTICAST_PARENT_mnt" ]]
[[ $(readlink /proc/self/ns/net) != "$WENDY_MULTICAST_PARENT_net" ]]
[[ $(readlink /proc/self/ns/pid) != "$WENDY_MULTICAST_PARENT_pid" ]]
mount --make-rprivate /
mount -t tmpfs tmpfs /run
mount -t sysfs -o ro,nosuid,nodev,noexec sysfs /sys
ip link set lo up
export WENDY_MULTICAST_NAMESPACE=1
unset WENDY_MULTICAST_PACKET WENDY_MULTICAST_READY WENDY_MULTICAST_INTERFACE WENDY_MULTICAST_FLOOD
"$2" -test.run '^TestMulticastInterfaceLifecycle$' -test.v -test.count=1 -test.timeout=100s
