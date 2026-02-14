SUMMARY = "WendyOS Device Identity Management"
DESCRIPTION = "Generates and manages unique device UUID and device name for WendyOS devices"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

inherit systemd

SRC_URI = " \
    file://generate-uuid.sh \
    file://generate-device-name.sh \
    file://update-mdns-uuid.sh \
    file://wendyos-uuid-generate.service \
    file://wendyos-device-name-generate.service \
    file://wendyos-identity.service \
    file://adjectives.txt \
    file://nouns.txt \
    "

S = "${UNPACKDIR}"

SYSTEMD_SERVICE:${PN} = "wendyos-uuid-generate.service wendyos-device-name-generate.service wendyos-identity.service"
SYSTEMD_AUTO_ENABLE:${PN} = "enable"

do_install() {
    # Install scripts to /usr/bin
    install -d ${D}${bindir}
    install -m 0755 ${UNPACKDIR}/generate-uuid.sh ${D}${bindir}/
    install -m 0755 ${UNPACKDIR}/generate-device-name.sh ${D}${bindir}/
    install -m 0755 ${UNPACKDIR}/update-mdns-uuid.sh ${D}${bindir}/

    # Install word lists for device name generation
    install -d ${D}${datadir}/wendyos
    install -m 0644 ${UNPACKDIR}/adjectives.txt ${D}${datadir}/wendyos/
    install -m 0644 ${UNPACKDIR}/nouns.txt ${D}${datadir}/wendyos/

    # Install systemd services
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${UNPACKDIR}/wendyos-uuid-generate.service ${D}${systemd_system_unitdir}/
    install -m 0644 ${UNPACKDIR}/wendyos-device-name-generate.service ${D}${systemd_system_unitdir}/
    install -m 0644 ${UNPACKDIR}/wendyos-identity.service ${D}${systemd_system_unitdir}/

    # Create directory for identity storage
    install -d ${D}${sysconfdir}/wendyos

    # Create Wendy/WendyOS version file
    install -d ${D}${sysconfdir}/wendy
    echo "WendyOS-${DISTRO_VERSION}" > ${UNPACKDIR}/version.txt
    install -m 0644 ${UNPACKDIR}/version.txt ${D}${sysconfdir}/wendy/version.txt

    # Create build ID file (actual date will be set at first boot if needed)
    echo "WendyOS-${DISTRO_VERSION}" > ${UNPACKDIR}/wendyos-build-id
    install -m 0644 ${UNPACKDIR}/wendyos-build-id ${D}${sysconfdir}/wendyos-build-id
}

FILES:${PN} += "${bindir}/generate-uuid.sh"
FILES:${PN} += "${bindir}/generate-device-name.sh"
FILES:${PN} += "${bindir}/update-mdns-uuid.sh"
FILES:${PN} += "${datadir}/wendyos/adjectives.txt"
FILES:${PN} += "${datadir}/wendyos/nouns.txt"
FILES:${PN} += "${systemd_system_unitdir}/wendyos-uuid-generate.service"
FILES:${PN} += "${systemd_system_unitdir}/wendyos-device-name-generate.service"
FILES:${PN} += "${systemd_system_unitdir}/wendyos-identity.service"
FILES:${PN} += "${sysconfdir}/wendyos"
FILES:${PN} += "${sysconfdir}/wendy/version.txt"
FILES:${PN} += "${sysconfdir}/wendyos-build-id"

RDEPENDS:${PN} = "bash util-linux-uuidgen avahi-daemon coreutils iproute2"