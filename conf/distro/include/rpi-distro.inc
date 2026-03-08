# RPi5-specific distro configuration
# This file is included when building for RPi machines

# No Mender for RPi5
MENDER_CONNECT_ENABLE = "0"
# No EFI/UEFI on RPi5
WENDYOS_UPDATE_BOOTLOADER = "0"
WENDYOS_EFI_DEBUG = "0"
WENDYOS_DEEPSTREAM = "0"
WENDYOS_PERSIST_JOURNAL_LOGS ?= "0"
# WiFi support
DISTRO_FEATURES:append = " wifi"
# WKS search path
WKS_SEARCH_PATH:prepend = "${TD_DISTRO_LAYERDIR}/wic:"

# PARTUUID defaults (overwritten at parse-time by partuuid.bbclass)
WENDYOS_BOOT_PARTUUID ?= "00000000-0000-0000-0000-000000000000"
WENDYOS_ROOT_PARTUUID ?= "00000000-0000-0000-0000-000000000000"
