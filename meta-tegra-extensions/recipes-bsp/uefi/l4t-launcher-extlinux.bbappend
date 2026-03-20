
# Override the mender-community setting which incorrectly uses /boot/ prefix
UBOOT_EXTLINUX_FDT:jetson-orin-nano-devkit = "tegra234-p3768-0000+p3767-0005-nv-super.dtb"

# AGX Orin DevKit (64GB variant uses P3701-0005 module on P3737-0000 carrier)
UBOOT_EXTLINUX_FDT:jetson-agx-orin-devkit = "tegra234-p3737-0000+p3701-0005-nv.dtb"

