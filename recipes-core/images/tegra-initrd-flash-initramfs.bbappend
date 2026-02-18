# R38.4 flash initramfs: no additions needed
# The base recipe already includes all required packages:
#   - tegra-target-flash-scripts (RDEPENDS adb-prebuilt)
#   - nv-kernel-module-pcie-tegra264, nv-kernel-module-ufs-tegra
# MACHINE_ESSENTIAL_EXTRA_RRECOMMENDS adds kernel-module-ucsi-ccg, tegra-xudc
