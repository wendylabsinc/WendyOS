#!/bin/bash
# Runs only in the ordinary Docker image build, never on the CI host kernel.
set -euo pipefail
cd /fixture
# shellcheck source=.github/scripts/fixtures/driver-vm/preflight.sh
source /fixture/preflight.sh
driver_vm_preflight /usr/src/kernels
kver=$driver_vm_kernel_release
printf '%s\n' "$kver" > kernel-release
rpm -qa --qf '%{NAME} %{VERSION}-%{RELEASE}.%{ARCH}\n' | sort > packages.txt
# The exact packer consumes the same metadata fields as a released devkit.
# This guest's real distro/kernel supplies those values; no target ABI is faked.
printf '\nSYSEXT_LEVEL=wendy-driver-fixture\n' >> /usr/lib/os-release
mkdir -p devkit/kernel-build images build
printf 'CONFIG_X86_64=y\n' > devkit/kernel-build/.config
: > devkit/env.sh
python3 - "$kver" <<'PY'
import json,sys
from pathlib import Path
Path('devkit/devkit.json').write_text(json.dumps({'kernel_version':sys.argv[1], 'arch':'x86', 'os_id':'fedora', 'sysext_level':'wendy-driver-fixture', 'machine':'qemux86-64'}))
PY
for variant in v1 v2 failing healthy broken; do
  name=wendy-fixture-target
  module=wendy_fixture_target
  version=1
  extra=
  case "$variant" in
    v2) version=2 ;;
    failing) version=3; extra=-DFIXTURE_FAIL_INIT ;;
    healthy) name=wendy-fixture-healthy; module=wendy_fixture_healthy ;;
    broken) name=wendy-fixture-broken; module=wendy_fixture_broken; extra=-DFIXTURE_FAIL_INIT ;;
  esac
  mkdir -p "build/$variant/modules" "build/$variant/driver" "images/$variant"
  cp module.c "build/$variant/modules/$module.c"
  printf 'obj-m := %s.o\nccflags-y := -DFIXTURE_VERSION=%s %s\n' "$module" "$version" "$extra" > "build/$variant/modules/Makefile"
  make -s -j2 -C "/usr/src/kernels/$kver" M="$PWD/build/$variant/modules" modules
  [[ $(modinfo -F vermagic "build/$variant/modules/$module.ko") == "$kver "* ]]
  python3 - "$name" "$module" "build/$variant/driver/driver.json" <<'PY'
import json,sys
from pathlib import Path
Path(sys.argv[3]).write_text(json.dumps({'name':sys.argv[1], 'modules_load':[sys.argv[2]], 'activation':{'modules_replace':[sys.argv[2]]}}))
PY
  bash pack-sysext.sh --devkit devkit --driver "build/$variant/driver" --modules "build/$variant/modules" --out "$PWD/images/$variant/$name.raw"
done
# A controlled omission in a second VM must leave real v1 code resident on v2 install.
python3 - <<'PY'
from pathlib import Path
p=Path('/usr/sbin/wendyos-sysext-apply.sh')
s=p.read_text(); needle='if ! unload_targets "$name"; then'
assert s.count(needle)==1,'shipped unload site changed'
Path('/fixture/omit-unload.sh').write_text(s.replace(needle,'if ! true; then'))
PY
chmod 755 /fixture/omit-unload.sh
cp "/usr/lib/modules/$kver/vmlinuz" /fixture/vmlinuz
# No host-only discovery, network, KVM or device access is needed to build this.
dracut --force --no-hostonly --no-hostonly-cmdline --kver "$kver" \
  --add-drivers 'virtio_pci virtio_blk ext4 squashfs overlay loop' \
  --filesystems ext4 /fixture/initrd
sha256sum /fixture/vmlinuz /fixture/initrd /usr/sbin/wendyos-sysext-apply.sh /fixture/pack-sysext.sh /fixture/images/*/*.raw > /fixture/artifacts.sha256
# Keep compiler outputs out of the runtime image, without removing runtime modules.
rm -rf /fixture/build /fixture/devkit /usr/src/kernels
dnf clean all
