# Disable polkit support in systemd
# We enable polkit distro feature for rtkit, but systemd doesn't actually need
# polkit support compiled in. Disabling it avoids build issues with polkitd user
# not existing during systemd's install phase.
#
# Note: This only disables PolicyKit integration in systemd itself. The polkit
# daemon will still run on the target system for rtkit and other services.

PACKAGECONFIG:remove = "polkit"

# Disable debug source package splitting to avoid pseudo uid lookup failures.
# do_package hardlinks source files (owned by uid 1000 / build user) into PKGD.
# OEOuthashBasic then calls getpwuid(1000) via pseudo, which redirects to the
# target rootfs /etc/passwd — which has no uid 1000 — causing a KeyError fatal.
# Debug packages are not deployed to the target image, so this has no runtime impact.
INHIBIT_PACKAGE_DEBUG_SPLIT = "1"

# RPi5-specific systemd extensions — isolated so Tegra/QEMU builds are unaffected
require ${@'rpi-systemd.inc' if 'rpi' in d.getVar('MACHINEOVERRIDES').split(':') else ''}
