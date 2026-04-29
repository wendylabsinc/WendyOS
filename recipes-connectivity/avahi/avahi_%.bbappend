
FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

SRC_URI += " \
    file://wendyos-mdns.service \
    file://generate-hostname.sh \
    file://wendyos-hostname.service \
    file://nsswitch.conf.append \
    file://90-wendyos.preset \
    "

# Ensure D-Bus support is enabled for proper service publishing
PACKAGECONFIG += "dbus"

# Ensure Avahi compiles with static service file support
EXTRA_OECONF += " \
    --with-avahi-user=avahi \
    --with-avahi-group=avahi \
    "

inherit systemd

do_install:append() {
    # Install hostname generation script + systemd unit (goes to sub-package)
    install -d ${D}${sbindir}
    install -m 0755 ${WORKDIR}/generate-hostname.sh ${D}${sbindir}/

    # Install Avahi service file with platform substitution
    install -d ${D}${sysconfdir}/avahi/services
    sed 's|@WENDYOS_PLATFORM@|${MACHINE}|' \
        ${WORKDIR}/wendyos-mdns.service \
        > ${D}${sysconfdir}/avahi/services/wendyos-mdns.service
    chmod 0644 ${D}${sysconfdir}/avahi/services/wendyos-mdns.service

    # Install systemd service for hostname setup
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/wendyos-hostname.service ${D}${systemd_system_unitdir}/

    # Ensure NSS mDNS is properly configured
    if [ -f "${D}${sysconfdir}/nsswitch.conf" ]
    then
        # Check if mdns is already configured
        if ! grep -q "mdns" "${D}${sysconfdir}/nsswitch.conf"
        then
            # Replace the hosts line with our configuration
            sed -i '/^hosts:/d' "${D}${sysconfdir}/nsswitch.conf"
            cat "${WORKDIR}/nsswitch.conf.append" >> "${D}${sysconfdir}/nsswitch.conf"
        fi
    fi

    # Enable Avahi daemon and ensure it starts with proper settings
    if [ -f "${D}${sysconfdir}/avahi/avahi-daemon.conf" ]
    then
        # Enable D-Bus support for proper service publishing
        sed -i 's/^#*enable-dbus=.*/enable-dbus=yes/' "${D}${sysconfdir}/avahi/avahi-daemon.conf"

        # Reflector bridges mDNS across interfaces. In avahi 0.8 it prevents
        # the daemon from completing startup regardless of interface count,
        # so services never get published. Keep it disabled.
        sed -i 's/^#*enable-reflector=.*/enable-reflector=no/' "${D}${sysconfdir}/avahi/avahi-daemon.conf"

        # Restrict Avahi to specific interfaces if configured
        # Empty WENDYOS_MDNS_INTERFACES means all interfaces (no restriction)
        if [ -n "${WENDYOS_MDNS_INTERFACES}" ]; then
            if grep -q '^allow-interfaces=' "${D}${sysconfdir}/avahi/avahi-daemon.conf"; then
                sed -i "s/^allow-interfaces=.*/allow-interfaces=${WENDYOS_MDNS_INTERFACES}/" \
                    "${D}${sysconfdir}/avahi/avahi-daemon.conf"
            else
                sed -i '/^\[server\]/a allow-interfaces=${WENDYOS_MDNS_INTERFACES}' \
                    "${D}${sysconfdir}/avahi/avahi-daemon.conf"
            fi
        fi

        # Set proper hostname behavior
        sed -i 's/^#*use-ipv4=.*/use-ipv4=yes/' "${D}${sysconfdir}/avahi/avahi-daemon.conf"
        sed -i 's/^#*use-ipv6=.*/use-ipv6=yes/' "${D}${sysconfdir}/avahi/avahi-daemon.conf"

        # Enable publishing
        sed -i 's/^#*publish-addresses=.*/publish-addresses=yes/' "${D}${sysconfdir}/avahi/avahi-daemon.conf"
        sed -i 's/^#*publish-hinfo=.*/publish-hinfo=yes/' "${D}${sysconfdir}/avahi/avahi-daemon.conf"
        sed -i 's/^#*publish-workstation=.*/publish-workstation=yes/' "${D}${sysconfdir}/avahi/avahi-daemon.conf"
        sed -i 's/^#*publish-domain=.*/publish-domain=yes/' "${D}${sysconfdir}/avahi/avahi-daemon.conf"

        # Set host name
        sed -i 's/^#*host-name=.*/# host-name is set dynamically by wendyos-hostname.service/' "${D}${sysconfdir}/avahi/avahi-daemon.conf"
    fi

    # Systemd preset to auto-enable hostname service by default
    install -d ${D}${systemd_unitdir}/system-preset
    install -m 0644 ${WORKDIR}/90-wendyos.preset \
        ${D}${systemd_unitdir}/system-preset/90-wendyos.preset
}

# --- What remains in the avahi main package (ONLY the .service for mDNS) ---
FILES:${PN} += " ${sysconfdir}/avahi/services/wendyos-mdns.service "

# --- Sub-package for WendyOS hostname setup ---
PACKAGES:prepend = "${PN}-wendyos-hostname "
FILES:${PN}-wendyos-hostname = " \
    ${sbindir}/generate-hostname.sh \
    ${systemd_system_unitdir}/wendyos-hostname.service \
    ${systemd_unitdir}/system-preset/90-wendyos.preset \
    "

RDEPENDS:${PN}-wendyos-hostname = "bash iproute2 systemd avahi-daemon"
SYSTEMD_SERVICE:${PN}-wendyos-hostname = "wendyos-hostname.service"
SYSTEMD_AUTO_ENABLE:${PN}-wendyos-hostname = "enable"

# Postinstall hook: safety net in case preset doesn't run at image build time
pkg_postinst:${PN}-wendyos-hostname () {
    if [ -z "$D" ]
    then
        systemctl enable wendyos-hostname.service || true
        systemctl start  wendyos-hostname.service || true
    fi
}
