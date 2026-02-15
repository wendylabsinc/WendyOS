SUMMARY = "Tegra-specific Mender state scripts for slot switching and bootloader updates"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

# Add our custom files on top of upstream
# - switch-rootfs: Replaces upstream (via FILESEXTRAPATHS precedence) for conditional capsule staging
# - verify-bootloader-update: Adds comprehensive verification (version + ESRT)
# Upstream provides: verify-slot, abort-blupdate (we keep those)
SRC_URI += " \
    file://verify-bootloader-update \
    "

RDEPENDS:${PN} = "tegra-bootcontrol-overlay"

# Package our additional verification script
FILES:${PN} += "${datadir}/mender/modules/v3/ArtifactVerifyReboot_50_verify-bootloader-update"

do_compile:prepend() {
    # Verify our custom switch-rootfs is being used
    if grep -q "^WENDYOS_SWITCH_ROOTFS_VERSION=" ${UNPACKDIR}/switch-rootfs
    then
        version=$(grep "^WENDYOS_SWITCH_ROOTFS_VERSION=" ${UNPACKDIR}/switch-rootfs | cut -d'"' -f2)
        bbnote "Using WendyOS custom switch-rootfs v${version} (conditional capsule staging)"
    else
        bbfatal "FILESEXTRAPATHS not working! Upstream switch-rootfs detected - this breaks conditional updates."
    fi
}

# Append our verify-bootloader-update installation to upstream's do_install
do_install:append() {
    # Add comprehensive bootloader verification script (ArtifactVerifyReboot)
    # This supplements upstream's verify-slot (ArtifactCommit) with version+ESRT checks
    mkdir -p ${D}${datadir}/mender/modules/v3
    install -m 0755 ${UNPACKDIR}/verify-bootloader-update \
        ${D}${datadir}/mender/modules/v3/ArtifactVerifyReboot_50_verify-bootloader-update
}
