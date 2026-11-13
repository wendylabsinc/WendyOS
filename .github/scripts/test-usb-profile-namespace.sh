#!/usr/bin/env bash
# Audit2124: private NetworkManager/D-Bus and real veth replug, no physical USB.
set -euo pipefail
if [[ ${GITHUB_ACTIONS:-} != true || ${RUNNER_ENVIRONMENT:-} != github-hosted || $(uname -s) != Linux ]]; then
  echo 'USB profile fixture requires the disposable GitHub-hosted Linux runner.' >&2
  exit 2
fi
if [[ ${1:-} == --namespace ]]; then
  [[ $EUID == 0 && $# == 3 ]] || exit 2
  export WENDY_USB_PARENT_mnt WENDY_USB_PARENT_net WENDY_USB_PARENT_pid WENDY_USB_PARENT_uts
  for kind in mnt net pid uts; do
    printf -v "WENDY_USB_PARENT_$kind" '%s' "$(readlink "/proc/self/ns/$kind")"
  done
  exec timeout --kill-after=3s 150s unshare --mount --net --pid --uts --fork --propagation private --mount-proc \
    "$(realpath "$0")" --inside "$(realpath "$2")" "$(realpath "$3")"
fi
if [[ ${1:-} == --inside ]]; then
  # Namespace guard first: it needs no privilege, so the unprivileged offline
  # test reaches it instead of stopping at the EUID check.
  for kind in mnt net pid uts; do
    variable="WENDY_USB_PARENT_$kind"
    [[ -n ${!variable:-} && $(readlink "/proc/self/ns/$kind") != "${!variable}" ]] || {
      echo 'USB profile fixture must run outside its parent namespaces.' >&2
      exit 2
    }
  done
  [[ $EUID == 0 && $# == 3 ]] || exit 2
  mount --make-rprivate /
  # Never create directories on the host. Package provisioning must supply
  # these mountpoints; every write below goes into fresh namespace mounts.
  for directory in /run /etc/NetworkManager /var/lib/NetworkManager /etc/udev/rules.d /etc/netplan; do
    [[ -d $directory ]] || exit 2
    mount -t tmpfs tmpfs "$directory"
  done
  # Ubuntu's NM keyfile writer delegates persistence to Netplan. Hide vendor
  # Netplan inputs as well, so generating the fixture cannot import host links.
  if [[ -d /lib/netplan ]]; then mount -t tmpfs tmpfs /lib/netplan; fi
  if [[ -d /usr/lib/NetworkManager/system-connections ]]; then
    mount -t tmpfs tmpfs /usr/lib/NetworkManager/system-connections
  fi
  mount -t sysfs -o ro,nosuid,nodev,noexec sysfs /sys
  mkdir -p /run/dbus /run/usb-profile/bin /run/usb-profile/conf.d /etc/NetworkManager/system-connections
  sysctl -q -w net.ipv6.conf.all.disable_ipv6=0 net.ipv6.conf.default.disable_ipv6=0
  ip link set lo up
  ip netns add gadget
  ip netns exec gadget ip link set lo up
  ip link add usbport0 address 02:00:00:00:21:24 type veth peer name gadget0
  ip link set gadget0 netns gadget
  ip link set usbport0 up
  ip netns exec gadget ip address add 169.254.200.2/16 dev gadget0
  ip netns exec gadget ip link set gadget0 up
  # An unrelated dongle must never acquire this device's profile.
  ip link add unrelated0 address 02:00:00:00:21:25 type veth peer name otherpeer
  ip link set unrelated0 up
  ip link set otherpeer up
  # Only udev is a recorded boundary: triggering the host's whole physical
  # device tree is outside this network-profile test. nmcli remains real.
  cat > /run/usb-profile/bin/udevadm <<'UDEV'
#!/bin/sh
case "$*" in
  'control --reload-rules'|'trigger') printf '%s\n' "$*" >> /run/usb-profile/udev-boundary.log ;;
  *) exit 91 ;;
esac
UDEV
  chmod 700 /run/usb-profile/bin/udevadm
  export PATH="/run/usb-profile/bin:$PATH" WENDY_USB_PROFILE_NAMESPACE=1
  export WENDY_USB_PROFILE_EVIDENCE="$3"
  unset DBUS_SYSTEM_BUS_ADDRESS DBUS_SESSION_BUS_ADDRESS WENDY_USB_PROFILE_PEER
  # Namespace init lifetime is the final drain for daemon/helper descendants.
  exec "$2" -test.run '^TestUSBProfileFollowsDeviceAcrossInterfaceRecreation$' -test.v -test.count=1 -test.timeout=140s
fi
[[ $# == 1 && $EUID != 0 ]] || { echo 'usage: test-usb-profile-namespace.sh NEW_OUTPUT_DIRECTORY (runner user)' >&2; exit 2; }
for tool in go NetworkManager nmcli dbus-daemon ip unshare timeout; do command -v "$tool" >/dev/null; done
mkdir "$1"
output=$(realpath "$1")
script=$(realpath "$0")
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out=Path(sys.argv[1])
source=Path('go/internal/cli/commands/device_usbsetup_linux.go').resolve()
before='"ethernet.mac-address", mac.String(), "connection.autoconnect", "yes",'
text=source.read_text()
assert text.count(before)==1, 'USB profile mutation site changed'
replacement=out/'pinned-interface.go'
replacement.write_text(text.replace(before, '"connection.interface-name", "usbport0", '+before))
(out/'pinned-interface.json').write_text(json.dumps({'Replace':{str(source):str(replacement)}}))
PY
for variant in normal pinned-interface; do
  overlay=()
  if [[ $variant != normal ]]; then overlay=(-overlay="$output/pinned-interface.json"); fi
  go test -race -tags regression_usb_profile "${overlay[@]}" -c -o "$output/$variant.test" ./go/internal/cli/commands
  mkdir "$output/$variant"
  result=0
  # The runner owns artifacts; elevated mutations stay in the fresh namespaces.
  # shellcheck disable=SC2024
  sudo -n env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted "$script" --namespace "$output/$variant.test" "$output/$variant" > "$output/$variant.log" 2>&1 || result=$?
  cat "$output/$variant.log"
  if [[ $variant == normal ]]; then
    [[ $result == 0 ]] || exit 1
    grep -q '^--- PASS: TestUSBProfileFollowsDeviceAcrossInterfaceRecreation ' "$output/$variant.log"
    grep -q 'same saved USB profile reconnected across new interface and daemon restart with exact TCP bytes' "$output/$variant.log"
    if grep -q -- '--- SKIP:' "$output/$variant.log"; then exit 1; fi
  else
    [[ $result != 0 ]] || exit 1
    grep -q '^--- FAIL: TestUSBProfileFollowsDeviceAcrossInterfaceRecreation ' "$output/$variant.log"
    grep -q 'saved USB profile did not reconnect after port/interface change' "$output/$variant.log"
  fi
  rm "$output/$variant.test"
done
