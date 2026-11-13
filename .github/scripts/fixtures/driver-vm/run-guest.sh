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
# shellcheck disable=SC2329 # Invoked through the EXIT trap.
finish() {
  result=$?
  trap - EXIT
  printf '\nDRIVER_VM_EXIT nonce=%s control=%s phase=%s status=%s\n' "$nonce" "$control" "$phase" "$result"
  sync
  systemctl poweroff --no-block
  exit "$result"
}
trap finish EXIT
# A dedicated virtio port is not a controlling terminal: getty/vhangup cannot
# revoke the test or its independent timer diagnostics. Admission is bounded.
# shellcheck source=.github/scripts/fixtures/driver-vm/evidence.sh
source /fixture/evidence.sh
driver_vm_open_evidence
exec 1>&3 2>&3
printf 'DRIVER_VM_EVIDENCE_CHANNEL_READY\n'
# Keep this after the nonce guard and exit trap, before any driver operation.
# A broken guest service must produce diagnostics, not consume the VM deadline.
# shellcheck source=.github/scripts/fixtures/driver-vm/dbus.sh
source /fixture/dbus.sh
driver_vm_require_bus
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
if "$binary" -test.run '^TestDriverLifecycleVM$' -test.v -test.timeout=420s; then
  exit 0
else
  result=$?
  # Preserve the exact failure and shipped script bytes. Read-only observations
  # distinguish missing merged payloads from failed module lookup/activation.
  timeout --kill-after=1s 20s /bin/bash /fixture/diagnostics.sh || true
  exit "$result"
fi
