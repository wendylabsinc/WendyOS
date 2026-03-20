
PR = "r0"
PACKAGE_ARCH = "${MACHINE_ARCH}"

inherit packagegroup

# ss(8) for socket/port inspection
# getent(1) for NSS/DNS lookups
SUMMARY:${PN} = "Debugging package group"
RDEPENDS:${PN} = " \
    ${@oe.utils.ifelse( \
        d.getVar('WENDYOS_DEBUG') == '1', \
        ' \
            mmc-utils \
            fio \
            memtester \
            gperftools \
            bash \
            rt-tests \
            nfs-utils \
            procps \
            sysstat \
            ldd \
            bc  \
            iproute2-ss \
        ', \
        '' \
        )} \
    "

# Tegra-specific debug tools (Jetson hardware only)
RDEPENDS:${PN}:append:tegra = " \
    ${@oe.utils.ifelse( \
        d.getVar('WENDYOS_DEBUG') == '1', \
        'python3-jetson-stats', \
        '' \
        )} \
    "
