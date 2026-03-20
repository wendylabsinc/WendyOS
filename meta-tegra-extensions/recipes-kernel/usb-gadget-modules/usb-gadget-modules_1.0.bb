
SUMMARY = "Autoload USB gadget function modules"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

FILESEXTRAPATHS:prepend := "${THISDIR}/${PN}:"

SRC_URI = "file://usb-gadget.conf"

S = "${WORKDIR}"

do_install() {
    install -d ${D}${sysconfdir}/modules-load.d
    install -m 0644 ${WORKDIR}/usb-gadget.conf ${D}${sysconfdir}/modules-load.d/
}

# Ensure the module packages are present.
RDEPENDS:${PN} += " \
    kernel-modules \
    "

# kernel-module-libcomposite
# kernel-module-u-ether
# kernel-module-usb-f-ncm
# kernel-module-usb-f-ecm
