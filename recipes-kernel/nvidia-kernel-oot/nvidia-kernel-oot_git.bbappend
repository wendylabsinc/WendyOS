FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

# Don't override SRCREV - use the one from meta-tegra base recipe
# The upstream wip-r38.2.x branch has proper kernel 6.8 compatibility

# Fix for R38.2.x: S is incorrectly set - source is in ${WORKDIR}/git
S = "${WORKDIR}/git"

# Fix for R38.2.x: Override EXTRA_OEMAKE to remove -Wno-error=header-guard (not supported in GCC 13.4)
# Also disable unused-variable errors to avoid build failures on legitimate unused code
# This replaces the base recipe's EXTRA_OEMAKE from nvidia-kernel-oot.inc
EXTRA_OEMAKE = '\
    IGNORE_PREEMPT_RT_PRESENCE=1 KERNEL_PATH="${STAGING_KERNEL_BUILDDIR}" \
    CC="${KERNEL_CC} -std=gnu17 -Wno-error=unused-variable -Wno-error=unused-but-set-variable" \
    CXX="${KERNEL_CC} -x c++" LD="${KERNEL_LD}" AR="${KERNEL_AR}" \
    OBJCOPY="${KERNEL_OBJCOPY}" OPENRM=1 kernel_name="noble" \
    IGNORE_CC_MISMATCH=1 \
'

# Fix for R38.2.x: License files referenced in LIC_FILES_CHKSUM don't exist
# This is a bug in meta-tegra R38.2.x - downgrade license-checksum from error to warning
ERROR_QA:remove = "license-checksum"
WARN_QA:append = " license-checksum"
