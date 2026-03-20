SUMMARY = "NVIDIA Container Configuration for Jetson"
DESCRIPTION = "Provides l4t.csv configuration, CDI spec generation, and CUDA environment detection for NVIDIA GPU containers"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

inherit systemd

# DeepStream support flag (default off)
WENDYOS_DEEPSTREAM ?= "0"

SRC_URI = " \
    file://l4t.csv \
    file://l4t-deepstream.csv \
    file://devices-wendyos.csv \
    file://wendyos-cdi-generate.service \
    file://wendyos-cuda-detect.service \
    file://generate-cuda-env.sh \
    file://99-z-nvidia-tegra.rules \
    file://fix-cdi-gstreamer-paths.sh \
    "

S = "${WORKDIR}"

SYSTEMD_SERVICE:${PN} = "wendyos-cdi-generate.service wendyos-cuda-detect.service"
SYSTEMD_AUTO_ENABLE:${PN} = "enable"

do_install() {
    # Install CSV files to the NVIDIA container runtime config directory
    install -d ${D}${sysconfdir}/nvidia-container-runtime/host-files-for-container.d

    # Install base l4t.csv (CUDA/PyTorch libraries)
    install -m 0644 ${WORKDIR}/l4t.csv ${D}${sysconfdir}/nvidia-container-runtime/host-files-for-container.d/

    # Install DeepStream CSV if enabled
    if [ "${WENDYOS_DEEPSTREAM}" = "1" ]; then
        bbnote "Installing DeepStream l4t-deepstream.csv"
        install -m 0644 ${WORKDIR}/l4t-deepstream.csv ${D}${sysconfdir}/nvidia-container-runtime/host-files-for-container.d/

        # Create multiarch compatibility symlinks for GStreamer DeepStream plugins
        # This allows the CDI GST_PLUGIN_PATH to work with both Yocto and Debian/Ubuntu conventions
        install -d ${D}${libdir}/aarch64-linux-gnu/gstreamer-1.0
        ln -sf ../../gstreamer-1.0/deepstream ${D}${libdir}/aarch64-linux-gnu/gstreamer-1.0/deepstream

        # Create multiarch symlink for nvidia libraries (libgstnvcustomhelper.so, etc.)
        install -d ${D}${libdir}/aarch64-linux-gnu
        ln -sf ../nvidia ${D}${libdir}/aarch64-linux-gnu/nvidia
    fi

    # Install WendyOS device/sysfs mappings (supplements meta-tegra's devices.csv)
    install -m 0644 ${WORKDIR}/devices-wendyos.csv ${D}${sysconfdir}/nvidia-container-runtime/host-files-for-container.d/

    # Install CUDA environment detection script
    install -d ${D}${bindir}
    install -m 0755 ${WORKDIR}/generate-cuda-env.sh ${D}${bindir}/

    # Install CDI post-processing script for DeepStream path fixes
    install -m 0755 ${WORKDIR}/fix-cdi-gstreamer-paths.sh ${D}${bindir}/

    # Install systemd services
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/wendyos-cdi-generate.service ${D}${systemd_system_unitdir}/
    install -m 0644 ${WORKDIR}/wendyos-cuda-detect.service ${D}${systemd_system_unitdir}/

    # Install udev rules for GPU device permissions (z- prefix ensures it runs last)
    install -d ${D}${sysconfdir}/udev/rules.d
    install -m 0644 ${WORKDIR}/99-z-nvidia-tegra.rules ${D}${sysconfdir}/udev/rules.d/

    # Create directory for CUDA environment file
    install -d ${D}${sysconfdir}/default
}

FILES:${PN} += "${sysconfdir}/nvidia-container-runtime/host-files-for-container.d/l4t.csv"
FILES:${PN} += "${sysconfdir}/nvidia-container-runtime/host-files-for-container.d/l4t-deepstream.csv"
FILES:${PN} += "${sysconfdir}/nvidia-container-runtime/host-files-for-container.d/devices-wendyos.csv"
FILES:${PN} += "${bindir}/generate-cuda-env.sh"
FILES:${PN} += "${bindir}/fix-cdi-gstreamer-paths.sh"
FILES:${PN} += "${systemd_system_unitdir}/wendyos-cdi-generate.service"
FILES:${PN} += "${systemd_system_unitdir}/wendyos-cuda-detect.service"
FILES:${PN} += "${sysconfdir}/udev/rules.d/99-z-nvidia-tegra.rules"

# Multiarch compatibility symlinks (only when DeepStream is enabled)
FILES:${PN} += "${@bb.utils.contains('WENDYOS_DEEPSTREAM', '1', '${libdir}/aarch64-linux-gnu/gstreamer-1.0/deepstream', '', d)}"
FILES:${PN} += "${@bb.utils.contains('WENDYOS_DEEPSTREAM', '1', '${libdir}/aarch64-linux-gnu/nvidia', '', d)}"

# nvidia-container-toolkit is now available via meta-tegra virtualization layer
RDEPENDS:${PN} = "nvidia-container-toolkit libnvidia-container bash"
