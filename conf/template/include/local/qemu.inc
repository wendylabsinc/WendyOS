# QEMU defaults: virtual machine, single ext4 partition, no Mender OTA.

# Volatile journal logs in tmpfs (no /data partition)
WENDYOS_PERSIST_JOURNAL_LOGS = "0"

# No UEFI/Mender bootloader updates on QEMU
WENDYOS_UPDATE_BOOTLOADER = "0"
