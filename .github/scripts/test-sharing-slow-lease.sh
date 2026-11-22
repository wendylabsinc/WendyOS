#!/usr/bin/env bash
# Audit 2524: one real TLS lease survives Controller steps before kernel traffic.
set -euo pipefail
if [[ ${1:-} != --inside ]]; then
  [[ $(uname -s) == Linux && $EUID == 0 && $# == 3 && ${GITHUB_ACTIONS:-} == true && ${RUNNER_ENVIRONMENT:-} == github-hosted ]] || { echo 'hosted-only: sudo test-sharing-slow-lease.sh NORMAL CONTROL OUTPUT' >&2; exit 2; }
  normal=$(realpath "$1"); control=$(realpath "$2"); output=$(realpath "$3")
  [[ -x $normal && -x $control && -d $output ]]
  export WENDY_LEASE_PARENT_mnt WENDY_LEASE_PARENT_net WENDY_LEASE_PARENT_pid
  WENDY_LEASE_PARENT_mnt=$(readlink /proc/self/ns/mnt)
  WENDY_LEASE_PARENT_net=$(readlink /proc/self/ns/net)
  WENDY_LEASE_PARENT_pid=$(readlink /proc/self/ns/pid)
  for mode in normal control; do
    binary=$normal; [[ $mode == normal ]] || binary=$control
    set +e
    timeout --kill-after=3s 110s unshare --mount --net --pid --fork --kill-child=KILL --propagation private --mount-proc \
      "$(realpath "$0")" --inside "$binary" >"$output/$mode.log" 2>&1
    status=$?
    set -e
    cat "$output/$mode.log"
    if grep -q -- '--- SKIP:\|WARNING: DATA RACE' "$output/$mode.log"; then exit 1; fi
    if [[ $mode == normal ]]; then
      [[ $status == 0 ]]
      grep -q 'SLOW_LEASE_CONTROLLER_VERIFIED' "$output/$mode.log"
      grep -q 'mutual TLS, two-hop IPv6 transit, NAT44/NAT66, DNS and withdrawal passed' "$output/$mode.log"
    else
      [[ $status == 1 ]]
      grep -q 'pending controller withdrew the live TLS lease before recovery' "$output/$mode.log"
      grep -q '^--- FAIL: TestSharingControllerSlowLeaseAcrossNamespaces ' "$output/$mode.log"
    fi
  done
  echo 'SLOW_GATEWAY_LEASE_KERNEL_VERIFIED: generated TLS delay; no physical BLE claim'
  exit
fi
[[ $EUID == 0 && $# == 2 ]]
for kind in mnt net pid; do
  name=WENDY_LEASE_PARENT_$kind
  [[ $(readlink "/proc/self/ns/$kind") != "${!name}" ]]
done
mount --make-rprivate /
mount -t tmpfs tmpfs /run
mount -t sysfs -o ro,nosuid,nodev,noexec sysfs /sys
# HostPolicy cleanup must never see the runner's resolver journal. No external
# name resolution is needed; donor DNS is queried by its owned namespace IP.
# Debian's iptables/ip6tables executables traverse /etc/alternatives. Preserve
# that directory at a stable path BEFORE hiding /etc. mount canonicalizes a
# /proc/self/fd source back to its old pathname, which now names the empty mount.
declare -A expected_tools
for tool in iptables ip6tables; do
  tool_path=$(command -v "$tool")
  expected_tools[$tool]=$(readlink -f "$tool_path")
  [[ -x ${expected_tools[$tool]} ]]
  printf 'Before /etc isolation: %s -> %s\n' "$tool" "${expected_tools[$tool]}"
done
mkdir /run/wendy-lease-alternatives
mount --bind /etc/alternatives /run/wendy-lease-alternatives
mount -o remount,bind,ro /run/wendy-lease-alternatives
mount -t tmpfs tmpfs /etc
mkdir /etc/alternatives
mount --bind /run/wendy-lease-alternatives /etc/alternatives
mount -o remount,bind,ro /etc/alternatives
for tool in iptables ip6tables; do
  tool_path=$(command -v "$tool")
  resolved_tool=$(readlink -f "$tool_path")
  [[ $resolved_tool == "${expected_tools[$tool]}" && -x $resolved_tool ]]
  printf 'After /etc isolation: %s -> %s\n' "$tool" "$resolved_tool"
  "$tool" --version
done
printf 'root:x:0:0:root:/root:/bin/false\nnobody:x:65534:65534:nobody:/:/bin/false\n' >/etc/passwd
printf 'root:x:0:\nnogroup:x:65534:\n' >/etc/group
printf 'passwd: files\ngroup: files\nhosts: files dns\n' >/etc/nsswitch.conf
: >/etc/resolv.conf
ip link set lo up
export WENDY_MESH_NAMESPACE_TEST=1
exec "$2" -test.run '^TestSharingControllerSlowLeaseAcrossNamespaces$' -test.v -test.count=1 -test.timeout=100s
