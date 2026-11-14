#!/usr/bin/env bash
# Runs only on a disposable Linux CI host. All hostname, mount and mDNS changes
# occur inside new mount/UTS/network/PID namespaces, never on the host network.
set -euo pipefail
if [[ ${1:-} != --inside ]]; then
  [[ $EUID == 0 && $# == 2 ]] || { echo 'usage: sudo test-hostname-restart.sh TEST_BINARY BUILDER_CHECKOUT' >&2; exit 2; }
  binary=$(realpath "$1")
  builder=$(realpath "$2")
  [[ $(git -c safe.directory="$builder" -C "$builder" rev-parse HEAD) == 954d4e8680c3e5ef6c5fc7b6179902149cebd830 ]]
  root=$(mktemp -d /tmp/wendy-hostname-regression.XXXXXXXX)
  trap 'rm -rf -- "$root"' EXIT
  mkdir -p "$root/etc/avahi/services" "$root/etc/wendyos" "$root/etc/wendy-agent" "$root/sbin"
  cp /etc/passwd /etc/group /etc/nsswitch.conf "$root/etc/"
  printf '127.0.0.1 localhost\n' > "$root/etc/hosts"
  cp "$(command -v avahi-daemon)" "$root/sbin/avahi-daemon"
  cp "$(command -v ip)" "$root/sbin/ip"
  cp "$builder/recipes-connectivity/avahi/files/generate-hostname.sh" "$root/sbin/generate-hostname.sh"
  chmod +x "$root/sbin/generate-hostname.sh"
  cat > "$root/etc/avahi/avahi-daemon.conf" <<'CONF'
[server]
use-ipv4=yes
use-ipv6=no
allow-interfaces=publisher0
enable-dbus=no
[wide-area]
enable-wide-area=no
[publish]
publish-workstation=no
publish-hinfo=no
CONF
  cat > "$root/systemctl" <<'SYSTEMCTL'
#!/usr/bin/env bash
# Only the service-manager boundary is adapted. Avahi itself runs and restarts.
set -euo pipefail
[[ $# == 2 && $2 == avahi-daemon ]] || exit 90
case "$1" in restart|try-reload-or-restart|stop) ;; *) exit 91 ;; esac
if [[ -f /run/avahi-daemon/pid ]]; then
  /usr/sbin/avahi-daemon --kill
  for ((i=0;i<50;i++)); do [[ ! -f /run/avahi-daemon/pid ]] && break; sleep .1; done
  [[ ! -f /run/avahi-daemon/pid ]]
fi
[[ $1 == stop ]] && exit 0
exec /usr/sbin/avahi-daemon --daemonize --no-drop-root --no-chroot --no-rlimits
SYSTEMCTL
  chmod +x "$root/systemctl"
  export WENDY_HOSTNAME_PARENT_MNT WENDY_HOSTNAME_PARENT_UTS WENDY_HOSTNAME_PARENT_NET WENDY_HOSTNAME_PARENT_PID
  WENDY_HOSTNAME_PARENT_MNT=$(readlink /proc/self/ns/mnt)
  WENDY_HOSTNAME_PARENT_UTS=$(readlink /proc/self/ns/uts)
  WENDY_HOSTNAME_PARENT_NET=$(readlink /proc/self/ns/net)
  WENDY_HOSTNAME_PARENT_PID=$(readlink /proc/self/ns/pid)
  exec_script=$(realpath "$0")
  timeout 180 unshare --mount --uts --net --pid --fork --propagation private --mount-proc "$exec_script" --inside "$binary" "$builder" "$root"
  exit
fi
[[ $EUID == 0 && $# == 4 ]]
binary=$2; builder=$3; root=$4
# Check separation before performing even the first mount or hostname operation.
[[ $(readlink /proc/self/ns/mnt) != "$WENDY_HOSTNAME_PARENT_MNT" ]]
[[ $(readlink /proc/self/ns/uts) != "$WENDY_HOSTNAME_PARENT_UTS" ]]
[[ $(readlink /proc/self/ns/net) != "$WENDY_HOSTNAME_PARENT_NET" ]]
[[ $(readlink /proc/self/ns/pid) != "$WENDY_HOSTNAME_PARENT_PID" ]]
mount --make-rprivate /
mount --bind "$root/etc" /etc
mount --bind "$root/sbin" /usr/sbin
mount --bind "$root/systemctl" /usr/bin/systemctl
mount -t tmpfs tmpfs /run
ip link set lo up
ip link add publisher0 type veth peer name probe0
ip address add 192.0.2.1/24 dev publisher0
ip netns add probe
ip link set probe0 netns probe
ip link set publisher0 up
ip netns exec probe ip address add 192.0.2.2/24 dev probe0
ip netns exec probe ip link set lo up
ip netns exec probe ip link set probe0 up
export IDENTITY_LIB="$builder/recipes-core/wendyos-identity/files/wendyos-identity-lib.sh"
export WENDY_MDNS_BOOT_SCRIPT="$builder/recipes-core/wendyos-identity/files/update-mdns-uuid.sh"
export WENDY_HOSTNAME_NAMESPACE=1
"$binary" -test.run '^TestDeviceName(RestartIntegration|BootProcess|DNSProbe)$' -test.v -test.count=1 -test.timeout=150s
