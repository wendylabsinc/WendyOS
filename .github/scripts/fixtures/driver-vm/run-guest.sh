#!/bin/bash
# Only PID1 in the owned QEMU guest starts this unit.
set -euo pipefail
nonce=$(cat /fixture/nonce)
[[ $(cat /sys/class/dmi/id/product_name) == "WendyDriverFixture-$nonce" ]]
export WENDY_DRIVER_VM="$nonce"
control=normal
phase=first
read -r -a boot_arguments < /proc/cmdline
for arg in "${boot_arguments[@]}"; do
  case "$arg" in wendy.control=*) control=${arg#*=} ;; wendy.phase=*) phase=${arg#*=} ;; esac
done
case "$control" in normal|omit-unload|removal-rollback) ;; *) exit 2 ;; esac
case "$phase" in first|reboot) ;; *) exit 2 ;; esac
finish() {
  result=$?
  trap - EXIT
  printf '\nDRIVER_VM_EXIT nonce=%s control=%s phase=%s status=%s\n' "$nonce" "$control" "$phase" "$result"
  sync
  systemctl poweroff --no-block
  exit "$result"
}
trap finish EXIT
if [[ $control == omit-unload ]]; then
  cp /fixture/omit-unload.sh /usr/sbin/wendyos-sysext-apply.sh
fi
if [[ $phase == reboot ]]; then
  [[ $control == normal && -f /fixture/first-boot-id ]]
  export WENDY_DRIVER_AFTER_REBOOT=1
fi
# This is the actual OS boot application, not a synthesized receipt.
/usr/sbin/wendyos-sysext-apply.sh
binary=/fixture/services.test
[[ $control != removal-rollback ]] || binary=/fixture/removal-rollback.test
"$binary" -test.run '^TestDriverLifecycleVM$' -test.v -test.timeout=420s
