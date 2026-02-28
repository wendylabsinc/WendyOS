
SUMMARY = "UEFI boot-priority overlay for Jetson"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

FILESEXTRAPATHS:prepend := "${THISDIR}/files:"
SRC_URI = " \
    file://boot-priority.dtso \
    "
S = "${WORKDIR}"

inherit allarch

# Ensure the native dtc is available for do_compile
DEPENDS += "dtc-native"

# Where to deploy the artifact (what tegraflash expects)
DEPLOYDIR = "${DEPLOY_DIR_IMAGE}"

do_compile() {
    # dtc -I dts -O dtb -o ${B}/boot-priority.dtbo ${WORKDIR}/boot-priority.dtso
    # Use the dtc from the native sysroot explicitly (robust across PATHs)
    ${STAGING_BINDIR_NATIVE}/dtc -I dts -O dtb \
        -o ${B}/boot-priority.dtbo \
        ${WORKDIR}/boot-priority.dtso
}

# deploy to tmp/deploy/images/${MACHINE}/ so tegraflash can pick it up
do_deploy() {
    install -d ${DEPLOYDIR}
    install -m 0644 ${B}/boot-priority.dtbo ${DEPLOYDIR}/
}
addtask deploy after do_compile before do_build

do_install() {
    install -d ${D}${sysconfdir}/tegra/bootcontrol/overlays
    install -m 0644 ${B}/boot-priority.dtbo \
        ${D}${sysconfdir}/tegra/bootcontrol/overlays/
}

# Tell packaging we *do* ship this file (prevents installed-vs-shipped QA)
FILES:${PN} += "${sysconfdir}/tegra/bootcontrol/overlays/boot-priority.dtbo"
