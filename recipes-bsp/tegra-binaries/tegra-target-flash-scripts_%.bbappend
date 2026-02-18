# Fix UDC device detection for Thor (T264) and future platforms
# The upstream script only checks for specific hardcoded UDC device names (3550000.usb, a808670000.usb)
# which doesn't include Thor's UDC device. We replace this with dynamic discovery that works for all platforms.
#
# Also fix QSPI device path for nvdd compatibility on T264/Thor:
# nvdd expects /dev/block/810c5b0000.spi (the SPI5 MMIO address) but the Yocto-built
# kernel exposes QSPI NOR flash as /dev/mtd0 (Linux MTD framework). The fix creates
# a symlink /dev/810c5b0000.spi -> /dev/mtd0 before the ADB flash server starts.
# (/dev/block is a symlink to /dev in Yocto initramfs, so nvdd resolves the path correctly)

do_compile:append() {
    # Delete the hardcoded UDC device lines and the checks for them
    sed -i '/^[[:space:]]*known_udc_dev1=/d; /^[[:space:]]*known_udc_dev2=/d' ${B}/ramdisk/usr/bin/nv_enable_remote.sh

    # Replace the first UDC check block with dynamic discovery
    sed -i '/echo "Finding UDC"/a\
\t\t# Dynamically find any UDC device (works for all Tegra platforms)\
\t\tfor udc_candidate in /sys/class/udc/*; do\
\t\t\tif [ -e "${udc_candidate}" ]; then\
\t\t\t\tudc_dev=$(basename "${udc_candidate}")\
\t\t\t\tbreak 2\
\t\t\tfi\
\t\tdone' ${B}/ramdisk/usr/bin/nv_enable_remote.sh

    # Remove the old hardcoded checks (4 lines per if block: if, assignment, break, fi)
    sed -i '/if \[ -e "\/sys\/class\/udc\/\${known_udc_dev[12]}" \]; then/,+3d' ${B}/ramdisk/usr/bin/nv_enable_remote.sh

    # Fix == to = for POSIX compliance
    sed -i 's/if \[ "\${udc_dev}" == "" \]/if [ -z "${udc_dev}" ]/' ${B}/ramdisk/usr/bin/nv_enable_remote.sh

    # Fix QSPI device path: create /dev/810c5b0000.spi -> /dev/mtd0 symlink before flash starts.
    # nvdd (NVIDIA's QSPI flash tool) expects the device at /dev/block/810c5b0000.spi, which in
    # Yocto initramfs resolves to /dev/810c5b0000.spi (since /dev/block -> /dev).
    # The Yocto kernel exposes QSPI NOR via Linux MTD as /dev/mtd0.
    # Race condition: QSPI driver probes ~2s after init starts, so we must wait for /dev/mtd0.
    # Note: base recipe rewrites /bin/ to ${bindir}/ (/usr/bin/), so match the rewritten path
    sed -i '/source \/usr\/bin\/nv_recovery\.sh/i\
# Fix QSPI device path for nvdd (T264/Thor: /dev/810c5b0000.spi must point to /dev/mtd0)\
# Wait for QSPI driver to probe (may take a few seconds after boot)\
qspi_wait=0\
while [ ! -e /dev/mtd0 ] && [ $qspi_wait -lt 15 ]; do\
    sleep 1\
    qspi_wait=$((qspi_wait + 1))\
done\
if [ -e /dev/mtd0 ]; then\
    ln -sf /dev/mtd0 /dev/810c5b0000.spi\
    echo "Flash-init: created QSPI symlink /dev/810c5b0000.spi -> /dev/mtd0 (waited ${qspi_wait}s)" > /dev/kmsg\
else\
    echo "Flash-init: WARNING - /dev/mtd0 not found after 15s, QSPI flash may fail" > /dev/kmsg\
fi' ${B}/ramdisk/init
}
