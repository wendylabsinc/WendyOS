#!/bin/bash
# Read-only dependency checks inside the guest image build. Sourced by build-guest.
# The caller supplies the actual kernel build root; tests use an owned directory.
driver_vm_preflight() {
  local kernel_root=$1 kernel_devel systemd_package sysext_version sysext_help option major
  if ! systemd_package=$(rpm -q --qf '%{VERSION}-%{RELEASE}.%{ARCH}' systemd); then
    echo 'Driver VM preflight: cannot query the installed systemd package.' >&2
    return 1
  fi
  if ! driver_vm_kernel_release=$(rpm -q --qf '%{VERSION}-%{RELEASE}.%{ARCH}\n' kernel-core); then
    echo 'Driver VM preflight: kernel-core is missing or its RPM query failed.' >&2
    return 1
  fi
  if ! kernel_devel=$(rpm -q --qf '%{VERSION}-%{RELEASE}.%{ARCH}' kernel-devel); then
    echo 'Driver VM preflight: kernel-devel is missing or its RPM query failed.' >&2
    return 1
  fi
  printf 'Driver VM packages: systemd=%s kernel-core=%s kernel-devel=%s\n' \
    "$systemd_package" "$driver_vm_kernel_release" "$kernel_devel"
  if [[ -z $driver_vm_kernel_release || $driver_vm_kernel_release == *$'\n'* || $kernel_devel != "$driver_vm_kernel_release" ]]; then
    echo 'Driver VM preflight: require one exactly matching kernel-core/kernel-devel pair; inspect the package versions above.' >&2
    return 1
  fi
  if [[ ! -d $kernel_root/$driver_vm_kernel_release ]]; then
    printf 'Driver VM preflight: matching kernel build tree is absent: %s/%s\n' "$kernel_root" "$driver_vm_kernel_release" >&2
    return 1
  fi
  if ! sysext_version=$(SYSTEMD_COLORS=0 systemd-sysext --version); then
    printf 'Driver VM preflight: systemd-sysext --version failed (RPM systemd=%s).\n' "$systemd_package" >&2
    return 1
  fi
  printf 'Driver VM systemd-sysext: %s\n' "$sysext_version"
  if [[ ! $sysext_version =~ ^systemd[[:space:]]+([0-9]+)([[:space:].~-]|$) ]]; then
    echo 'Driver VM preflight: cannot identify the systemd-sysext binary version.' >&2
    return 1
  fi
  major=${BASH_REMATCH[1]}
  if (( 10#$major < 260 )); then
    printf 'Driver VM preflight: systemd >=260 is required for production --always-refresh; found %s (RPM %s). Use the pinned Fedora 45 guest, not Fedora 44; do not remove the production flag.\n' "$major" "$systemd_package" >&2
    return 1
  fi
  if ! sysext_help=$(SYSTEMD_PAGER=cat SYSTEMD_COLORS=0 systemd-sysext --help); then
    printf 'Driver VM preflight: systemd-sysext --help failed (RPM systemd=%s).\n' "$systemd_package" >&2
    return 1
  fi
  # Capture help once, rather than grep -q a producer pipe under pipefail.
  for option in --mutable --always-refresh; do
    if [[ $sysext_help != *"$option="* ]]; then
      printf 'Driver VM preflight: installed systemd-sysext lacks required %s (RPM systemd=%s). Keep the shipped Builder apply script unchanged and select a supporting guest package.\n' "$option" "$systemd_package" >&2
      return 1
    fi
  done
  printf 'Driver VM preflight verified: kernel=%s, systemd=%s, --mutable and --always-refresh available.\n' \
    "$driver_vm_kernel_release" "$systemd_package"
}
