#!/bin/bash
# Sourced only during the owned image build or the identified QEMU guest boot.
driver_vm_enable_bus() {
  local root=${1:-/}
  # Fedora's daemon unit declares Alias=dbus.service; package installation with
  # weak dependencies disabled does not select this provider for dbus.socket.
  systemctl --root="$root" enable dbus-daemon.service || return 1
  if [[ ! "$root/etc/systemd/system/dbus.service" -ef "$root/usr/lib/systemd/system/dbus-daemon.service" ]]; then
    printf 'DRIVER_VM_DBUS_SETUP_FAILED: dbus.service is not the installed daemon provider\n' >&2
    return 1
  fi
  printf 'DRIVER_VM_DBUS_PROVIDER=dbus-daemon.service\n'
}

driver_vm_bus_diagnostics() {
  printf 'DRIVER_VM_DBUS_PREFLIGHT_FAILED: system bus unavailable before driver activation\n' >&2
  timeout --kill-after=2s 5s systemctl --no-pager --full status dbus.socket dbus.service dbus-daemon.service >&2 || true
  timeout --kill-after=2s 5s journalctl --no-pager -b -n 80 -u dbus.socket -u dbus.service -u dbus-daemon.service >&2 || true
  timeout --kill-after=2s 5s systemctl --no-pager cat dbus.socket dbus.service >&2 || true
}

driver_vm_require_bus() {
  # Socket presence alone does not prove either bus dispatch or the systemd
  # endpoint used by the real sysext activation path. Exercise both services.
  if ! timeout --kill-after=2s 15s systemctl start dbus.service ||
     ! timeout --kill-after=2s 8s busctl --system --timeout=5 call org.freedesktop.DBus /org/freedesktop/DBus org.freedesktop.DBus GetId ||
     ! timeout --kill-after=2s 8s busctl --system --timeout=5 call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.DBus.Peer Ping; then
    driver_vm_bus_diagnostics
    return 1
  fi
  printf 'DRIVER_VM_DBUS_READY: bus identity and systemd RPC answered\n'
}
