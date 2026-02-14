# Fix for Yocto 5.3 (whinlatter) UNPACKDIR changes with R38 meta-tegra
# In whinlatter, files unpack to ${UNPACKDIR} (which is ${WORKDIR}/sources by default)
# The BSP tarball unpacks to ${UNPACKDIR}/Linux_for_Tegra
# This matches what R38 meta-tegra expects

# Set UNPACKDIR explicitly for this shared workdir recipe
UNPACKDIR = "${WORKDIR}/sources"

# S is already set to L4T_BSP_SHARED_SOURCE_DIR in tegra-binaries-38.2.1.inc
# which expands to ${TMPDIR}/work-shared/.../sources/Linux_for_Tegra
# This is correct for whinlatter, no override needed
