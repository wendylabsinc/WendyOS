#!/bin/bash
# Read-only observations after a failed assertion, only in the admitted guest.
set -u
nonce=$(cat /fixture/nonce) || exit 2
[[ ${WENDY_DRIVER_VM:-} == "$nonce" && ${#nonce} == 32 ]] || exit 2
[[ $(cat /sys/class/dmi/id/product_name) == "WendyDriverFixture-$nonce" ]] || exit 2
probe() {
  printf '\nDRIVER_VM_PAYLOAD_PROBE'
  printf ' %q' "$@"
  printf '\n'
  # Both time and output are bounded. A failing probe never replaces the
  # original test failure; this script performs no load, refresh or repair.
  timeout --kill-after=1s 3s "$@" 2>&1 | head -c 65536
}
probe uname -r
probe cat /proc/self/mountinfo
probe systemd-sysext --json=short status
kver=$(uname -r) || exit 2
for name in wendy-fixture-healthy wendy-fixture-target wendy-fixture-broken; do
  module=${name//-/_}
  payload=/usr/lib/wendyos-driver-payloads/$name
  probe ls -ld "/run/extensions/$name.raw" "/data/extensions/enabled/$kver/$name.raw" "$payload" "$payload/modules/$kver/$module.ko" "/usr/lib/modules/$kver/updates/wendyos/$name/$module.ko" "/sys/module/$module"
  for path in "$payload/modules-load.conf" "/usr/lib/wendyos-driver-activation.d/$name.conf" "/usr/lib/extension-release.d/extension-release.$name" "/data/extensions/modules-load.d/$kver/$name.conf" "/usr/lib/wendyos-driver-state/$name/generation" "/usr/lib/wendyos-driver-state/$name/modules-load.conf" "/usr/lib/wendyos-driver-state/$name/files"; do
    probe head -c 65536 "$path"
  done
  probe modinfo -F filename "$module"
  probe modinfo -F vermagic "$payload/modules/$kver/$module.ko"
  probe modprobe --show-depends -- "$module"
done
printf '\nDRIVER_VM_PAYLOAD_PROBES_FINISHED\n'
