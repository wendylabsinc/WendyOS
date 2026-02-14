# QEMU-specific distro configuration
# This file is included when building for QEMU machines

# Disable Tegra/hardware-specific features for QEMU
WENDYOS_UPDATE_BOOTLOADER = "0"
WENDYOS_EFI_DEBUG = "0"
WENDYOS_USB_GADGET = "0"
WENDYOS_DEEPSTREAM = "0"

# Disable persistent journal logs for QEMU (no /data partition)
# This prevents var-log.mount from being created
WENDYOS_PERSIST_JOURNAL_LOGS = "0"

# Note: Mender is not inherited for QEMU (see wendyos.conf)
# QEMU uses simple single-partition ext4 layout without Mender A/B partitions
