SUMMARY = "WendyOS MOTD (Message of the Day) Scripts"
DESCRIPTION = "Dynamic SSH login banner with WendyOS branding, system info, and service status"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = " \
    file://10-wendyos-header \
    file://20-system-info \
    file://30-services \
    "

UNPACKDIR = "${UNPACKDIR}/sources"
S = "${UNPACKDIR}"

do_install() {
    # Install update-motd.d scripts
    install -d ${D}${sysconfdir}/update-motd.d
    install -m 0755 ${UNPACKDIR}/10-wendyos-header ${D}${sysconfdir}/update-motd.d/
    install -m 0755 ${UNPACKDIR}/20-system-info ${D}${sysconfdir}/update-motd.d/
    install -m 0755 ${UNPACKDIR}/30-services ${D}${sysconfdir}/update-motd.d/

    # Create wrapper script to generate MOTD
    install -d ${D}${bindir}
    cat > ${D}${bindir}/update-motd << 'EOF'
#!/bin/sh
# Generate dynamic MOTD from scripts in /etc/update-motd.d/
for script in /etc/update-motd.d/*; do
    [ -x "$script" ] && "$script"
done
EOF
    chmod 0755 ${D}${bindir}/update-motd

    # Configure PAM to run update-motd on login (via profile.d)
    install -d ${D}${sysconfdir}/profile.d
    cat > ${D}${sysconfdir}/profile.d/motd.sh << 'EOF'
# Display dynamic MOTD on login
if [ -x /usr/bin/update-motd ] && [ -t 0 ]; then
    /usr/bin/update-motd
fi
EOF
    chmod 0644 ${D}${sysconfdir}/profile.d/motd.sh
}

FILES:${PN} = " \
    ${sysconfdir}/update-motd.d/ \
    ${sysconfdir}/profile.d/motd.sh \
    ${bindir}/update-motd \
    "

RDEPENDS:${PN} = "bash"
