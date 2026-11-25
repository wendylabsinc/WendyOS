#!/bin/bash
# Sourced only after the owned guest nonce guard. Tests may supply a private PTY.
driver_vm_open_evidence() {
  local port=${1:-/dev/virtio-ports/wendy.driver.evidence}
  local limit=${2:-15} deadline
  [[ $limit =~ ^[0-9]+$ && $limit -le 15 ]] || return 1
  deadline=$((SECONDS + limit))
  while [[ ! -c $port ]]; do
    if (( SECONDS >= deadline )); then
      printf 'DRIVER_VM_EVIDENCE_UNAVAILABLE: dedicated character port absent\n' >&2
      return 1
    fi
    sleep 0.1
  done
  # udev-created virtserialport is already connected by QEMU before guest boot.
  # O_RDWR avoids FIFO/terminal carrier open waits; regular files never qualify.
  exec 3<>"$port" || return 1
}
