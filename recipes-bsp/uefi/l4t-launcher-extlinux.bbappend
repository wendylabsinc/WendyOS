
# Override the mender-community setting which incorrectly uses /boot/ prefix
UBOOT_EXTLINUX_FDT:jetson-orin-nano-devkit = "tegra234-p3768-0000+p3767-0005-nv-super.dtb"

# AGX Thor device tree (from upstream conf/machine/jetson-agx-thor-devkit.conf KERNEL_DEVICETREE)
UBOOT_EXTLINUX_FDT:jetson-agx-thor-devkit = "tegra264-p4071-0000+p3834-0008-nv.dtb"

