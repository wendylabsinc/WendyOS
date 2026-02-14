SUMMARY = "Audio and Bluetooth Configuration for WendyOS"
DESCRIPTION = "Configures PipeWire, WirePlumber, and BlueZ for out-of-the-box Bluetooth audio support"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

inherit systemd allarch

SRC_URI = " \
    file://pipewire-user-setup.service \
    file://pipewire-user-setup.sh \
    file://95-pipewire.preset \
    file://50-wireplumber-headless.conf \
    file://wireplumber-bluetooth.conf \
    file://wireplumber-dbus.conf \
"

S = "${UNPACKDIR}"

SYSTEMD_SERVICE:${PN} = "pipewire-user-setup.service"
SYSTEMD_AUTO_ENABLE:${PN} = "enable"

do_install() {
    # Install user service enablement script (runs at boot to enable per-user services)
    install -d ${D}${sbindir}
    install -m 0755 ${UNPACKDIR}/pipewire-user-setup.sh ${D}${sbindir}/

    # Install systemd service to enable audio for wendy user
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${UNPACKDIR}/pipewire-user-setup.service ${D}${systemd_system_unitdir}/

    # Install systemd user preset to auto-enable PipeWire/WirePlumber
    install -d ${D}${systemd_unitdir}/user-preset
    install -m 0644 ${UNPACKDIR}/95-pipewire.preset ${D}${systemd_unitdir}/user-preset/

    # Install WirePlumber configuration for headless Bluetooth
    # Disables seat monitoring so Bluetooth works without a display server
    install -d ${D}${sysconfdir}/wireplumber/wireplumber.conf.d
    install -m 0644 ${UNPACKDIR}/50-wireplumber-headless.conf \
        ${D}${sysconfdir}/wireplumber/wireplumber.conf.d/

    # Install D-Bus policy for Bluetooth access
    # Allows wendy user to communicate with BlueZ over D-Bus
    install -d ${D}${sysconfdir}/dbus-1/system.d
    install -m 0644 ${UNPACKDIR}/wireplumber-bluetooth.conf \
        ${D}${sysconfdir}/dbus-1/system.d/

    # Install WirePlumber systemd service drop-in
    # Sets D-Bus environment so WirePlumber can find the session bus
    install -d ${D}${systemd_unitdir}/user/wireplumber.service.d
    install -m 0644 ${UNPACKDIR}/wireplumber-dbus.conf \
        ${D}${systemd_unitdir}/user/wireplumber.service.d/dbus.conf
}

FILES:${PN} += " \
    ${sbindir}/pipewire-user-setup.sh \
    ${systemd_system_unitdir}/pipewire-user-setup.service \
    ${systemd_unitdir}/user-preset/95-pipewire.preset \
    ${sysconfdir}/wireplumber/wireplumber.conf.d/50-wireplumber-headless.conf \
    ${sysconfdir}/dbus-1/system.d/wireplumber-bluetooth.conf \
    ${systemd_unitdir}/user/wireplumber.service.d/dbus.conf \
"

# Runtime dependencies
RDEPENDS:${PN} = " \
    bash \
    pipewire \
    wireplumber \
    pipewire-pulse \
    pipewire-alsa \
    pipewire-spa-tools \
    pipewire-tools \
    bluez5 \
    bluez5-obex \
    dbus \
    alsa-utils \
    alsa-plugins \
    alsa-lib \
"

# rtkit provides real-time priority for audio, but requires polkit
# Add it if polkit is enabled in distro features
RDEPENDS:${PN} += "${@bb.utils.contains('DISTRO_FEATURES', 'polkit', 'rtkit', '', d)}"
