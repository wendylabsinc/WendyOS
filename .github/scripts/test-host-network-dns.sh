#!/usr/bin/env bash
# Real resolved, D-Bus, network links and containerd in private CI namespaces.
set -euo pipefail
if [[ ${1:-} != --inside ]]; then
  [[ $EUID == 0 && $# == 1 ]] || { echo 'usage: sudo test-host-network-dns.sh TEST_BINARY' >&2; exit 2; }
  binary=$(realpath "$1")
  [[ -x $binary ]]
  export WENDY_DNS_PARENT_mnt WENDY_DNS_PARENT_net WENDY_DNS_PARENT_pid
  WENDY_DNS_PARENT_mnt=$(readlink /proc/self/ns/mnt)
  WENDY_DNS_PARENT_net=$(readlink /proc/self/ns/net)
  WENDY_DNS_PARENT_pid=$(readlink /proc/self/ns/pid)
  exec timeout 120 unshare --mount --net --pid --fork --propagation private --mount-proc \
    bash "$(realpath "$0")" --inside "$binary"
fi
[[ $EUID == 0 && $# == 2 ]]
[[ $(readlink /proc/self/ns/mnt) != "$WENDY_DNS_PARENT_mnt" ]]
[[ $(readlink /proc/self/ns/net) != "$WENDY_DNS_PARENT_net" ]]
[[ $(readlink /proc/self/ns/pid) != "$WENDY_DNS_PARENT_pid" ]]
mount --make-rprivate /
mount -t tmpfs tmpfs /run
# A fresh sysfs exposes this network namespace's links, but hides the existing
# cgroup mount. Preserve it so runc can create and clean up its private tasks.
mkdir -p /run/dns-cgroup
mount --rbind /sys/fs/cgroup /run/dns-cgroup
mount -t sysfs -o ro,nosuid,nodev,noexec sysfs /sys
mount --rbind /run/dns-cgroup /sys/fs/cgroup
mkdir -p /run/dbus /run/systemd
# The CI login session's /run/user directory is hidden by the private /run.
# runc writes exec process specifications under XDG_RUNTIME_DIR.
export XDG_RUNTIME_DIR=/run/wendy-runtime
mkdir -m 700 "$XDG_RUNTIME_DIR"
# resolved creates its own runtime directory with the service user's ownership.
# Precreating /run/systemd/resolve as root makes the real daemon refuse startup.
# Replacing a resolver symlink under the host /etc would escape mount
# isolation. Bind an independent /etc tree before creating either config.
mkdir -p /run/dns-etc/systemd
cp /etc/passwd /etc/group /etc/nsswitch.conf /etc/machine-id /run/dns-etc/
printf '127.0.0.1 localhost\n' > /run/dns-etc/hosts
mount --bind /run/dns-etc /etc
ip link set lo up
ip link add uplink0 type veth peer name dns0
ip addr add 192.0.2.1/24 dev uplink0
ip addr add 192.0.2.53/24 dev uplink0
ip addr add 192.0.2.54/24 dev uplink0
ip link set uplink0 up
ip link set dns0 up
cat > /etc/systemd/resolved.conf <<'CONF'
[Resolve]
Cache=no
FallbackDNS=
LLMNR=no
MulticastDNS=no
DNSSEC=no
DNSOverTLS=no
ReadEtcHosts=no
DNSStubListener=yes
CONF
# Force the fallback host file to an upstream address. A regression that binds
# this snapshot instead of the stable stub must fail after upstream replacement.
printf 'nameserver 192.0.2.53\n' > /etc/resolv.conf
export DBUS_SYSTEM_BUS_ADDRESS=unix:path=/run/dbus/system_bus_socket
dbus-daemon --config-file=/usr/share/dbus-1/system.conf --nofork --nopidfile > /run/dns-dbus.log 2>&1 &
bus_pid=$!
resolved_pid=
cleanup() {
  status=$?
  trap - EXIT
  [[ $status == 0 ]] || cat /run/dns-dbus.log /run/dns-resolved.log >&2 || true
  [[ -z $resolved_pid ]] || kill "$resolved_pid" 2>/dev/null || true
  kill "$bus_pid" 2>/dev/null || true
  wait || true
  exit "$status"
}
trap cleanup EXIT
for ((i=0;i<100;i++)); do
  [[ -S /run/dbus/system_bus_socket ]] && break
  kill -0 "$bus_pid"
  sleep .05
done
[[ -S /run/dbus/system_bus_socket ]]
SYSTEMD_LOG_TARGET=console /usr/lib/systemd/systemd-resolved > /run/dns-resolved.log 2>&1 &
resolved_pid=$!
export WENDY_HOST_DNS_NAMESPACE=1
bash "$(dirname "$0")/test-containerd-regressions.sh" "$2" '^TestHostNetworkDNSOutage$'
