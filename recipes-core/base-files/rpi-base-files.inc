# RPi5-specific base-files extensions.
# Included conditionally from base-files_%.bbappend — safe for non-RPi machines.

inherit partuuid-rpi
inherit journal-persist-rpi

SRC_URI:append = " file://rpi-fstab"

do_install:append() {
    # Install PARTUUID-templated fstab and substitute the actual UUIDs
    install -m 0644 ${WORKDIR}/rpi-fstab ${D}${sysconfdir}/fstab
    sed -i "s/RPI_BOOT_UUID/${WENDYOS_BOOT_PARTUUID}/g" ${D}${sysconfdir}/fstab
    sed -i "s/RPI_ROOT_UUID/${WENDYOS_ROOT_PARTUUID}/g" ${D}${sysconfdir}/fstab
}

# Trigger a rebuild whenever the PARTUUIDs change
do_install[vardeps] += " WENDYOS_BOOT_PARTUUID WENDYOS_ROOT_PARTUUID"

# systemd persistent log
# If /var/log is a symlink into /var/volatile/log, journald cannot persist logs
# and packaging any /var/log/* triggers a "directory symlink" QA error.
do_install:append:journal_persist-on() {
    if [ -L ${D}${localstatedir}/log ]; then
        rm -f ${D}${localstatedir}/log
        install -d -m 0755 ${D}${localstatedir}/log
    fi
}
