# Remove tegra-boot-tools dependency for Thor (tegra264)
# Similar to tegra234 and tegra194, Thor uses UEFI boot and doesn't need boot-tools

RDEPENDS:${PN}:tegra264 = ""
