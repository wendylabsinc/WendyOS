# Whinlatter compatibility: Fix do_install file paths
#
# The base recipe uses ${WORKDIR}/mender-update-verifier.sh in do_install
# but files unpack to ${UNPACKDIR} in whinlatter. Since S = ${UNPACKDIR},
# use ${S} for consistent file paths.

do_install() {
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${S}/mender-update-verifier.service ${D}${systemd_system_unitdir}
    install -d -m 755 ${D}${bindir}
    install -m 755 ${S}/mender-update-verifier.sh ${D}${bindir}/
}

