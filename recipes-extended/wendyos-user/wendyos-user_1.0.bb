SUMMARY = "WendyOS Default User Configuration"
DESCRIPTION = "Creates the default 'wendy' user with appropriate permissions for WendyOS. \
The base package creates the user and sudoers config. The -data-setup package \
provides the first-boot service that initializes the home directory on the \
persistent /data partition (Tegra only)."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

inherit useradd systemd

SRC_URI = " \
    file://wendyos-user-setup.sh \
    file://wendyos-user-setup.service \
"

PACKAGES = "${PN}-data-setup ${PN}"

# Create wendy user - simplified group list (non-existent groups cause failures)
USERADD_PACKAGES = "${PN}"
# Password 'wendy' hash generated with: openssl passwd -6 -salt 5ixFr0sKRtsKKKhY wendy
# useradd -m creates /home/wendy on the rootfs; on Tegra, the first-boot service
# in wendyos-user-data-setup re-initializes it from persistent storage (/data/home)
USERADD_PARAM:${PN} = "-m -d /home/wendy -s /bin/bash -G dialout,video,audio,users -p '\$6\$5ixFr0sKRtsKKKhY\$5SyCVB9y95JEITWZ8AMcMCrMF4Rvq97ymUjEoUCBKfTl7vWHjTLEboowxWF6hIJgBUMOnJQfeIRPPwYCUaIwm.' wendy"

SYSTEMD_SERVICE:${PN}-data-setup = "wendyos-user-setup.service"
SYSTEMD_AUTO_ENABLE:${PN}-data-setup = "enable"

do_install() {
    # Install first-boot setup script (packaged in -data-setup)
    install -d ${D}${sbindir}
    install -m 0755 ${WORKDIR}/wendyos-user-setup.sh ${D}${sbindir}/wendyos-user-setup.sh

    # Install systemd service (packaged in -data-setup)
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/wendyos-user-setup.service ${D}${systemd_system_unitdir}/wendyos-user-setup.service
}

pkg_postinst_ontarget:${PN}() {
    # Add sudoers entry for wendy user on target only
    if [ -d /etc/sudoers.d ]; then
        echo "wendy ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/wendy
        chmod 0440 /etc/sudoers.d/wendy
    fi
}

# Base package: user creation + sudoers (no files, useradd class handles /etc/passwd)
FILES:${PN} = ""
ALLOW_EMPTY:${PN} = "1"
RDEPENDS:${PN} = "sudo bash systemd"

# Data setup package: first-boot /data/home initialization (Tegra only)
SUMMARY:${PN}-data-setup = "WendyOS User Home Directory Setup for /data Partition"
FILES:${PN}-data-setup = " \
    ${sbindir}/wendyos-user-setup.sh \
    ${systemd_system_unitdir}/wendyos-user-setup.service \
"
RDEPENDS:${PN}-data-setup = "${PN} bash systemd-mount-home"
