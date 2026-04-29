
SUMMARY = "UEFI boot-priority overlay for Jetson"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

# NVMe machines boot from NVMe; all others (eMMC/SD) keep sd first.
DTSO_FILE = "boot-priority.dtso"
DTSO_FILE:jetson-agx-orin-devkit-nvme-wendyos = "boot-priority-nvme.dtso"
DTSO_FILE:jetson-orin-nano-devkit-nvme-wendyos = "boot-priority-nvme.dtso"

SRC_URI = " \
    file://boot-priority.dtso \
    file://boot-priority-nvme.dtso \
    "
S = "${WORKDIR}"

inherit allarch

# Ensure the native dtc is available for do_compile
DEPENDS += "dtc-native"

# Where to deploy the artifact (what tegraflash expects)
DEPLOYDIR = "${DEPLOY_DIR_IMAGE}"

do_compile() {
    ${STAGING_BINDIR_NATIVE}/dtc -I dts -O dtb \
        -o ${B}/boot-priority.dtbo \
        ${WORKDIR}/${DTSO_FILE}
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
