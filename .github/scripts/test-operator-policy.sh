#!/usr/bin/env bash
# Run real kernel policy and warm-rollback tests only in disposable CI namespaces.
set -euo pipefail
if [[ ${1:-} != --inside ]]; then
  [[ $EUID == 0 && $# == 2 ]] || { echo 'usage: sudo test-operator-policy.sh CURRENT_TEST_BINARY PRE_OPERATOR_TEST_BINARY' >&2; exit 2; }
  current=$(realpath "$1")
  baseline=$(realpath "$2")
  [[ -x $current && -x $baseline ]]
  export WENDY_POLICY_PARENT_MNT WENDY_POLICY_PARENT_NET WENDY_POLICY_PARENT_PID
  WENDY_POLICY_PARENT_MNT=$(readlink /proc/self/ns/mnt)
  WENDY_POLICY_PARENT_NET=$(readlink /proc/self/ns/net)
  WENDY_POLICY_PARENT_PID=$(readlink /proc/self/ns/pid)
  exec timeout 180 unshare --mount --net --pid --fork --propagation private --mount-proc \
    "$(realpath "$0")" --inside "$current" "$baseline"
fi
[[ $EUID == 0 && $# == 3 ]]
current=$2; baseline=$3
# Refuse to touch interfaces, forwarding rules or mounts in the caller's namespaces.
[[ $(readlink /proc/self/ns/mnt) != "$WENDY_POLICY_PARENT_MNT" ]]
[[ $(readlink /proc/self/ns/net) != "$WENDY_POLICY_PARENT_NET" ]]
[[ $(readlink /proc/self/ns/pid) != "$WENDY_POLICY_PARENT_PID" ]]
mount --make-rprivate /
mount -t tmpfs tmpfs /run
mount -t sysfs -o ro,nosuid,nodev,noexec sysfs /sys
ip link set lo up
export WENDY_LOCALMESH_ISOLATED_TEST=1 WENDY_MESH_NAMESPACE_TEST=1
export WENDY_LOCALMESH_ROLLBACK_BINARY="$baseline"
unset WENDY_LOCALMESH_KERNEL_TEST_MODE WENDY_DNS_POLICY_PROCESS
"$current" -test.run '^(TestKernelOperatorPolicyIsolatedNamespace|TestKernelOperatorWarmRollbackIsolatedNamespace|TestHostDNSPolicyCleanupAcrossMountNamespace)$' \
  -test.v -test.count=1 -test.timeout=150s
