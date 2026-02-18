SUMMARY = "WendyOS Development Container Registry"
DESCRIPTION = "A lightweight OCI registry that uses containerd's content store"
HOMEPAGE = "https://github.com/mihai-chiorean/containerd-registry"
LICENSE = "Apache-2.0"
LIC_FILES_CHKSUM = "file://LICENSE;md5=3b83ef96387f14655fc854ddc3c6bd57"

SRC_URI = "git://github.com/mihai-chiorean/containerd-registry.git;protocol=https;branch=main \
           file://wendyos-dev-registry.service \
           file://wendyos-dev-registry-import.service \
           file://wendyos-dev-registry.sh \
          "

# Use latest commit on main branch (update SRCREV as needed)
SRCREV = "${AUTOREV}"

# We don't build the Go binary - it's provided in the container image
# This recipe only installs systemd services and management scripts
inherit systemd

# Skip compile - the binary is in the container image
do_compile[noexec] = "1"

# Runtime dependencies
# Note: containerd and nerdctl are expected to be provided by other packages
# The import service requires 'ctr' command from containerd package
RDEPENDS:${PN} = "\
    bash \
"

# Systemd services
SYSTEMD_SERVICE:${PN} = "\
    wendyos-dev-registry.service \
    wendyos-dev-registry-import.service \
"

# Enable the import service (runs once on first boot)
# Disable the registry service (started on-demand by wendy-agent)
SYSTEMD_AUTO_ENABLE:${PN} = "enable"
SYSTEMD_AUTO_ENABLE:wendyos-dev-registry.service = "disable"

do_install:append() {
    # Install systemd services
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${UNPACKDIR}/wendyos-dev-registry.service ${D}${systemd_system_unitdir}/
    install -m 0644 ${UNPACKDIR}/wendyos-dev-registry-import.service ${D}${systemd_system_unitdir}/

    # Install management script
    install -d ${D}${bindir}
    install -m 0755 ${UNPACKDIR}/wendyos-dev-registry.sh ${D}${bindir}/wendyos-dev-registry

    # Create directory for state file
    install -d ${D}${localstatedir}/lib/wendyos
}

FILES:${PN} += "\
    ${systemd_system_unitdir}/wendyos-dev-registry.service \
    ${systemd_system_unitdir}/wendyos-dev-registry-import.service \
    ${localstatedir}/lib/wendyos \
"

# Disable QA checks that may fail for Go binaries
INSANE_SKIP:${PN} = "ldflags already-stripped"
