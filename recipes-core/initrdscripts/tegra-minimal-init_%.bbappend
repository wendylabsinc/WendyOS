# Fix UNPACKDIR variable expansion issue in scarthgap
# The base recipe uses S = "${UNPACKDIR}" which causes expansion errors
# Override to use standard WORKDIR-based path

S = "${UNPACKDIR}"

# Fix do_install to use S instead of UNPACKDIR
do_install() {
    install -m 0755 ${S}/init-boot.sh ${D}/init
    install -m 0555 -d ${D}/proc ${D}/sys
    install -m 0755 -d ${D}/dev ${D}/mnt ${D}/run ${D}/usr
    install -m 1777 -d ${D}/tmp
    mknod -m 622 ${D}/dev/console c 5 1
    install -d ${D}${sysconfdir}
    install -m 0644 ${S}/platform-preboot.sh ${D}${sysconfdir}/platform-preboot
    sed -i -e "s#@@TNSPEC_BOOTDEV@@#${TNSPEC_BOOTDEV}#g" ${D}${sysconfdir}/platform-preboot
}
