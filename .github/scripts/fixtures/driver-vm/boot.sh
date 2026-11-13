#!/bin/bash
# Read-only admission after real QEMU/systemd boot. Never change propagation.
driver_vm_require_native_boot() {
  local result container vm propagation own_namespace init_namespace
  result=0
  container=$(timeout --kill-after=1s 3s systemd-detect-virt --container) || result=$?
  if [[ $result != 1 || $container != none ]]; then
    printf 'DRIVER_VM_CONTAINER_IDENTITY_REJECTED status=%s value=%s\n' "$result" "$container" >&2
    return 1
  fi
  vm=$(timeout --kill-after=1s 3s systemd-detect-virt --vm) || return 1
  case "$vm" in qemu|kvm) ;; *) printf 'Unexpected actual VM type: %s\n' "$vm" >&2; return 1 ;; esac
  propagation=$(timeout --kill-after=1s 3s findmnt --noheadings --output PROPAGATION --target /usr) || return 1
  if [[ $propagation != shared ]]; then
    printf 'DRIVER_VM_PROPAGATION_REJECTED value=%s\n' "$propagation" >&2
    return 1
  fi
  own_namespace=$(readlink /proc/self/ns/mnt) || return 1
  init_namespace=$(readlink /proc/1/ns/mnt) || return 1
  [[ -n $own_namespace && $own_namespace == "$init_namespace" ]] || return 1
  printf 'DRIVER_VM_NATIVE_BOOT_READY virtualization=%s usr-propagation=%s\n' "$vm" "$propagation"
}
