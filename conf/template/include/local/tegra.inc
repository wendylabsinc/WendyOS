# Tegra (NVIDIA Jetson) defaults: persistent /data, Mender OTA, UEFI capsule updates.

# Persistent journal logs stored on /data partition
WENDYOS_PERSIST_JOURNAL_LOGS = "1"

# Include UEFI capsule bootloader updates in Mender OTA deployments
WENDYOS_UPDATE_BOOTLOADER = "1"

# UEFI/EFI debug build mode:
#   "0" - RELEASE (minimal UART output, production default)
#   "1" - DEBUG (verbose boot/capsule debug output)
WENDYOS_EFI_DEBUG = "0"
