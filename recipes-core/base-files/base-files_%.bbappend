
FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

# Provide our custom /etc/hosts, profile.d defaults, console branding, and sysctl
SRC_URI += " \
    file://hosts \
    file://profile.d/wendyos-defaults.sh \
    file://issue \
    file://issue.net \
    file://sysctl.d/99-quiet-console.conf \
    "

do_install:append() {
    install -m 0644 ${WORKDIR}/hosts ${D}${sysconfdir}/hosts

    # Install profile.d defaults
    install -d ${D}${sysconfdir}/profile.d
    install -m 0755 ${WORKDIR}/profile.d/wendyos-defaults.sh ${D}${sysconfdir}/profile.d/wendyos-defaults.sh

    # Install console login branding (displayed before login prompt)
    install -m 0644 ${WORKDIR}/issue ${D}${sysconfdir}/issue
    install -m 0644 ${WORKDIR}/issue.net ${D}${sysconfdir}/issue.net

    # Install sysctl config to quiet console (reduce kernel/audit messages)
    install -d ${D}${sysconfdir}/sysctl.d
    install -m 0644 ${WORKDIR}/sysctl.d/99-quiet-console.conf ${D}${sysconfdir}/sysctl.d/
}

# Make it a config file so local edits survive upgrades
CONFFILES:${PN} += "${sysconfdir}/hosts"

hostname:pn-base-files = "wendyos"

# RPi5-specific extensions — isolated so Tegra/QEMU builds are unaffected
require ${@'rpi-base-files.inc' if 'rpi' in d.getVar('MACHINEOVERRIDES').split(':') else ''}
