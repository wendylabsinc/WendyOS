# Extend COMPATIBLE_MACHINE to include qemuarm64-wendyos
# Base recipe restricts to specific QEMU machine names with exact regex match
COMPATIBLE_MACHINE:append = "|qemuarm64-wendyos"

# Use qemuarm64 BSP definition for kernel configuration
# linux-yocto looks for machine-specific kernel metadata, tell it to use qemuarm64
KMACHINE:qemuarm64-wendyos = "qemuarm64"
