
FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

# Provide our custom /etc/mender/mender.conf
SRC_URI += " \
    file://server.crt \
    file://mender.conf \
    "

do_install:append() {
    install -d ${D}${sysconfdir}/mender
    install -m 0644 ${WORKDIR}/server.crt ${D}${sysconfdir}/mender
}

FILES:mender-config += "${sysconfdir}/mender/server.crt"
CONFFILES:mender-config += "${sysconfdir}/mender/server.crt"
