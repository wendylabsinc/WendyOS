# Tegra-specific packagegroup-wendyos-base configuration
# This file is included when building for Tegra machines

# Tegra-specific tools and hardware support
RDEPENDS:${PN}:append = " \
    tegra-flash-reboot \
    tegra-tools-tegrastats \
    "
