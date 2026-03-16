# Tegra-specific packagegroup-wendyos-base configuration
# This file is included when building for Tegra machines

# Tegra-specific tools and hardware support
RDEPENDS:${PN}:append = " \
    efibootmgr \
    tegra-flash-reboot \
    tegra-tools-tegrastats \
    "
