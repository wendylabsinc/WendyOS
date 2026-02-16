# Add ADB support for initrd-flash
# Required for flash tool to connect to device during flashing
# USB gadget kernel modules should already be available in the kernel

TEGRA_INITRD_FLASH_INSTALL:append = " \
    android-tools-adbd \
    android-tools-conf \
"
