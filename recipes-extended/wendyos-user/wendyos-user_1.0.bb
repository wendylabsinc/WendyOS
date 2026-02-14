SUMMARY = "WendyOS Default User Configuration"
DESCRIPTION = "Creates the default 'wendy' user with appropriate permissions for WendyOS. \
Home directory is initialized on first boot from persistent storage (/data/home)."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

inherit useradd systemd

SRC_URI = " \
    file://wendyos-user-setup.sh \
    file://wendyos-user-setup.service \
"

# Create wendy user - simplified group list (non-existent groups cause failures)
USERADD_PACKAGES = "${PN}"
# Password 'wendy' hash generated with: openssl passwd -6 -salt 5ixFr0sKRtsKKKhY wendy
# NOTE: useradd with -m flag will try to create home, but since /home is bind-mounted
# from /data/home, the actual initialization happens via first-boot service
USERADD_PARAM:${PN} = "-m -d /home/wendy -s /bin/bash -G dialout,video,audio,users -p '\$6\$5ixFr0sKRtsKKKhY\$5SyCVB9y95JEITWZ8AMcMCrMF4Rvq97ymUjEoUCBKfTl7vWHjTLEboowxWF6hIJgBUMOnJQfeIRPPwYCUaIwm.' wendy"

SYSTEMD_SERVICE:${PN} = "wendyos-user-setup.service"
SYSTEMD_AUTO_ENABLE = "enable"

do_install() {
    # Install first-boot setup script
    install -d ${D}${sbindir}
    install -m 0755 ${UNPACKDIR}/wendyos-user-setup.sh ${D}${sbindir}/wendyos-user-setup.sh

    # Install systemd service
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${UNPACKDIR}/wendyos-user-setup.service ${D}${systemd_system_unitdir}/wendyos-user-setup.service
}

pkg_postinst_ontarget:${PN}() {
    # Add sudoers entry for wendy user on target only
    if [ -d /etc/sudoers.d ]; then
        echo "wendy ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/wendy
        chmod 0440 /etc/sudoers.d/wendy
    fi
}

FILES:${PN} += " \
    ${sbindir}/wendyos-user-setup.sh \
    ${systemd_system_unitdir}/wendyos-user-setup.service \
"

# Ensure required packages are available
# systemd-mount-home provides the /home bind mount
RDEPENDS:${PN} = "sudo bash systemd systemd-mount-home"
