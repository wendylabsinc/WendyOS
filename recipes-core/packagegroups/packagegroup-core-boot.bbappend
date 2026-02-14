# Remove kernel-devicetree dependency for QEMU machines
# QEMU doesn't require device tree binaries in the same way physical hardware does
RDEPENDS:${PN}:remove:qemuall = "kernel-devicetree"
