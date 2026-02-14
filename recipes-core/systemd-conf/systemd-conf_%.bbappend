
FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

inherit ${@bb.utils.contains('DISTRO_FEATURES', 'systemd', 'systemd', '', d)}

# Add persistent journal configuration if enabled
SRC_URI += " \
    ${@'file://journald-persistent.conf file://var-log.mount' if d.getVar('WENDYOS_PERSIST_JOURNAL_LOGS') == '1' else ''} \
    "

# Enable var-log.mount unit when journal persistence is enabled
SYSTEMD_SERVICE:${PN} += "${@'var-log.mount' if d.getVar('WENDYOS_PERSIST_JOURNAL_LOGS') == '1' else ''}"
SYSTEMD_AUTO_ENABLE:${PN} = "enable"

# Whinlatter compatibility: Use UNPACKDIR for file paths
do_install:append() {
    if [ "${WENDYOS_PERSIST_JOURNAL_LOGS}" = "1" ]; then
        # Install persistent journal configuration
        # systemd-journald will automatically create /var/log/journal
        # with correct permissions when Storage=persistent is set
        install -D -m0644 ${UNPACKDIR}/journald-persistent.conf ${D}${systemd_unitdir}/journald.conf.d/10-wendyos-persistent.conf

        # Install var-log.mount unit to bind mount /data/log to /var/log
        # The x-systemd.mkdir option auto-creates /data/log if needed
        install -D -m0644 ${UNPACKDIR}/var-log.mount ${D}${systemd_system_unitdir}/var-log.mount
    fi
}
