# Tegra/Jetson-specific image configuration
# This file is included when building for Tegra machines (MACHINEOVERRIDES contains 'tegra')

# Apply our UEFI boot-priority overlay during flash
TEGRA_BOOTCONTROL_OVERLAYS += "boot-priority.dtbo"

# Tegra-specific packages (Jetson hardware only)
IMAGE_INSTALL:append = " \
    mender-esp \
    tegra-bootcontrol-overlay \
    packagegroup-wendyos-tegra \
    packagegroup-nvidia-container \
    nvidia-container-config \
    setup-nv-boot-control \
    "

# Conditional UEFI capsule package installation
# Controlled by WENDYOS_UPDATE_BOOTLOADER (defined in conf/distro/wendyos.conf)
IMAGE_INSTALL:append = " \
    ${@oe.utils.ifelse( \
        d.getVar('WENDYOS_UPDATE_BOOTLOADER') == '1', \
            ' \
                tegra-uefi-capsules \
                bootloader-update \
            ', \
            '' \
        )} \
    "

# Enable DeepStream SDK support (optional - adds ~1GB to image)
# Tegra/NVIDIA hardware only - DeepStream requires NVIDIA GPUs
# Also enables l4t-deepstream.csv which provides:
# - GPU device nodes (/dev/nvhost-*) for tegrastats and GPU monitoring
# - CUDA compilation toolchain (headers, binaries, nvvm) for Triton/JIT
# - Additional libraries (libnuma) and monitoring paths
WENDYOS_DEEPSTREAM ?= "1"
IMAGE_INSTALL:append = " \
    ${@oe.utils.ifelse( \
        d.getVar('WENDYOS_DEEPSTREAM') == '1', \
            ' \
                deepstream-7.1 \
            ', \
            '' \
        )} \
    "
