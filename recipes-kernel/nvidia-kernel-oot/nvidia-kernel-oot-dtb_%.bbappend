FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

# For Thor/R38: nvidia-kernel-oot has kernel 6.8 API issues
# Use prebuilt DTB from working Thor device instead
SRC_URI += "file://tegra264-p4071-0000+p3834-0008-nv.dtb"

# Skip compilation - we provide prebuilt DTB
do_compile() {
    :
}

# Install our prebuilt DTB
# Install to both /boot/devicetree (for rootfs) and /usr/share/tegraflash (for sysroot staging)
do_install() {
    install -d ${D}/boot/devicetree
    install -m 0644 ${WORKDIR}/tegra264-p4071-0000+p3834-0008-nv.dtb ${D}/boot/devicetree/

    # Also install to /usr/share/tegraflash so it gets staged to sysroot for tegraflash build
    install -d ${D}/usr/share/tegraflash
    install -m 0644 ${WORKDIR}/tegra264-p4071-0000+p3834-0008-nv.dtb ${D}/usr/share/tegraflash/
}

# Deploy the prebuilt DTB
do_deploy() {
    install -d ${DEPLOYDIR}
    install -m 0644 ${WORKDIR}/tegra264-p4071-0000+p3834-0008-nv.dtb ${DEPLOYDIR}/
}

FILES:${PN} = "/boot/devicetree/*.dtb /usr/share/tegraflash/*.dtb"
