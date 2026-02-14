# Fix for Yocto 5.3 (whinlatter) UNPACKDIR changes
# In whinlatter, the tarball unpacks to ${UNPACKDIR}/Linux_for_Tegra
# Set UNPACKDIR explicitly and update S to use it
UNPACKDIR = "${WORKDIR}/sources"
S = "${UNPACKDIR}/Linux_for_Tegra"
