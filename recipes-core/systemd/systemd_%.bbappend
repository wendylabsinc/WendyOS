# Disable polkit support in systemd
# We enable polkit distro feature for rtkit, but systemd doesn't actually need
# polkit support compiled in. Disabling it avoids build issues with polkitd user
# not existing during systemd's install phase.
#
# Note: This only disables PolicyKit integration in systemd itself. The polkit
# daemon will still run on the target system for rtkit and other services.

PACKAGECONFIG:remove = "polkit"

# Note: systemd-networkd-wait-online.service is masked via
# ROOTFS_POSTPROCESS_COMMAND in wendyos-image.bbappend (must run after
# pkg_postinst scriptlets to avoid "unit is masked" errors)

