# RPi5-specific systemd extensions.
# Included conditionally from systemd_%.bbappend — safe for non-RPi machines.

inherit journal-persist-rpi

# systemd persistent log
# Install the drop-in and tmpfiles rule ONLY when this feature is enabled.
do_install:append:journal_persist-on() {
    # journald drop-in to force persistence
    install -d ${D}${sysconfdir}/systemd/journald.conf.d
    cat > ${D}${sysconfdir}/systemd/journald.conf.d/10-persistent.conf << 'EOF'
[Journal]
# Persist logs under /var/log/journal
Storage=persistent
EOF

    # tmpfiles rule to ensure the directory exists with correct perms
    install -d ${D}${libdir}/tmpfiles.d
    cat > ${D}${libdir}/tmpfiles.d/50-journald-persistent.conf << 'EOF'
# type path                 mode  user group             age argument
d     /var/log/journal      2755  root systemd-journal   -   -
EOF
}

# Only ship these files when the feature is enabled.
FILES:${PN}:append:journal_persist-on = " \
    ${sysconfdir}/systemd/journald.conf.d/10-persistent.conf \
    ${libdir}/tmpfiles.d/50-journald-persistent.conf \
    "
