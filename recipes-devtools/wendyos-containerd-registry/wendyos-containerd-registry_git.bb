# [Deprecated]
# This recipe is no longer included in the image. wendy-agent now ships a built-in OCI
# registry on port 5000, superseding the container-based registry. Both this package and
# wendyos-dev-registry-image conflict with wendy-agent at runtime.
#
# [Note]
# This recipe uses SRCREV = "${AUTOREV}", which always fetches the latest commit
# from the main branch. This breaks build reproducibility and sstate-cache
# correctness: two builds may produce different outputs.
#
# [Fix]
# Pin SRCREV to a specific commit hash and update it deliberately when an
# upgrade is needed.

SUMMARY = "WendyOS Development Container Registry"
DESCRIPTION = "A lightweight OCI registry that uses containerd's content store"
HOMEPAGE = "https://github.com/mihai-chiorean/containerd-registry"
LICENSE = "Apache-2.0"
LIC_FILES_CHKSUM = "file://LICENSE;md5=3b83ef96387f14655fc854ddc3c6bd57"

SRC_URI = " \
    git://github.com/mihai-chiorean/containerd-registry.git;protocol=https;branch=main \
    file://wendyos-dev-registry.service \
    file://wendyos-dev-registry-import.service \
    file://wendyos-dev-registry.sh \
    "

# Use latest commit on main branch (update SRCREV as needed)
SRCREV = "${AUTOREV}"

S = "${WORKDIR}/git"

# We don't build the Go binary - it's provided in the container image
# This recipe only installs systemd services and management scripts
inherit systemd

# Skip compile - the binary is in the container image
do_compile[noexec] = "1"

# Split into two packages for independent SYSTEMD_AUTO_ENABLE control.
# The systemd bbclass only supports per-package (not per-service) granularity:
# SYSTEMD_AUTO_ENABLE is keyed on the package name, not the service name.
PACKAGES =+ "${PN}-import"
SYSTEMD_PACKAGES = "${PN} ${PN}-import"

# Registry service: disabled — started on-demand by wendy-agent
SYSTEMD_SERVICE:${PN} = "wendyos-dev-registry.service"
SYSTEMD_AUTO_ENABLE:${PN} = "disable"

# Import service: enabled — runs once on first boot to import the container image
SYSTEMD_SERVICE:${PN}-import = "wendyos-dev-registry-import.service"
SYSTEMD_AUTO_ENABLE:${PN}-import = "enable"

# Runtime dependencies
# Note: containerd and nerdctl are expected to be provided by other packages
# The import service requires 'ctr' command from containerd package
RDEPENDS:${PN} = " \
    bash \
    ${PN}-import \
    "

do_install:append() {
    # Install systemd services
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/wendyos-dev-registry.service ${D}${systemd_system_unitdir}/
    install -m 0644 ${WORKDIR}/wendyos-dev-registry-import.service ${D}${systemd_system_unitdir}/

    # Install management script
    install -d ${D}${bindir}
    install -m 0755 ${WORKDIR}/wendyos-dev-registry.sh ${D}${bindir}/wendyos-dev-registry

    # Create directory for state file
    install -d ${D}${localstatedir}/lib/wendyos
}

FILES:${PN} += "\
    ${systemd_system_unitdir}/wendyos-dev-registry.service \
    ${localstatedir}/lib/wendyos \
    "

FILES:${PN}-import = "\
    ${systemd_system_unitdir}/wendyos-dev-registry-import.service \
    "

# Disable QA checks that may fail for Go binaries
INSANE_SKIP:${PN} = "ldflags already-stripped"
