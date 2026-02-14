
# UEFI/EFI Debug Mode Configuration
# Set WENDYOS_EFI_DEBUG = "1" in local.conf or distro config to enable verbose UEFI debug output
# This is useful for troubleshooting UEFI boot issues, capsule update problems, etc.
#
# Debug mode adds:
# - Verbose debug messages to UART console
# - Capsule processing details (FMP protocol, ProcessCapsules, etc.)
# - BDS (Boot Device Selection) phase information
# - Detailed error messages
#
# Note: Debug builds are slightly larger and may boot slightly slower

# Override EDK2 build mode based on WENDYOS_EFI_DEBUG variable
# Default: "0" (RELEASE mode - production, minimal output)
# Set to "1" for DEBUG mode (verbose UEFI debug output)
EDK2_BUILD_RELEASE = "${@'0' if d.getVar('WENDYOS_EFI_DEBUG') == '1' else '1'}"

# Fix for Yocto 5.3 (whinlatter) UNPACKDIR changes
# UNPACKDIR defaults to ${WORKDIR}/sources in whinlatter
# S must reference UNPACKDIR, not WORKDIR directly
UNPACKDIR = "${WORKDIR}/sources"
S = "${UNPACKDIR}/edk2-tegra/edk2"

# Ignore patch-fuzz and patch-status warnings for R38 patches on whinlatter
WARN_QA:remove = "patch-fuzz patch-status"
ERROR_QA:remove = "patch-fuzz patch-status"
