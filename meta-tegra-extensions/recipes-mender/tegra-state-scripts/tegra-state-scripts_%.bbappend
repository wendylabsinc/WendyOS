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

do_compile:prepend() {
    # Verify our custom switch-rootfs is being used
    if grep -q "^WENDYOS_SWITCH_ROOTFS_VERSION=" ${WORKDIR}/switch-rootfs
    then
        version=$(grep "^WENDYOS_SWITCH_ROOTFS_VERSION=" ${WORKDIR}/switch-rootfs | cut -d'"' -f2)
        bbnote "Using WendyOS custom switch-rootfs v${version} (conditional capsule staging)"
    else
        bbfatal "FILESEXTRAPATHS not working! Upstream switch-rootfs detected - this breaks conditional updates."
    fi
}

do_compile:append() {
    # Bundle verify-bootloader-update as an artifact-level ArtifactCommit_Enter script.
    # - Follows the same pattern as upstream's ArtifactCommit_Leave_50_verify-slot
    # - The mender-state-scripts bbclass picks up Artifact* scripts from
    #   MENDER_STATE_SCRIPTS_DIR during do_deploy and bundles them in the .mender artifact
    # - ArtifactCommit_Enter runs before commit (after reboot to new slot); non-zero
    #   return triggers Mender rollback
    cp ${WORKDIR}/verify-bootloader-update \
        ${MENDER_STATE_SCRIPTS_DIR}/ArtifactCommit_Enter_50_verify-bootloader-update
}
