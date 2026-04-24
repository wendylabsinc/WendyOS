# Only runs on RPi machines (rpi-config recipe only exists in meta-raspberrypi)
# RPi5 needs dtoverlay=uart0 to map PL011 to GPIO 14/15 (yields /dev/ttyAMA0).
# RPi3/4 use the upstream default: PL011 stays on Bluetooth, serial console
# runs on the mini UART (/dev/ttyS0) via enable_uart=1 set in their machine
# configs. Do NOT apply dtoverlay=uart0 for all :rpi — on RPi3/4 it would
# steal PL011 from Bluetooth.
do_deploy:append:raspberrypi5() {
    # enable_uart=1 is already written by the upstream rpi-config recipe when
    # ENABLE_UART=1 (set in raspberrypi5-wendyos.conf). Do not duplicate it here.
    echo "dtoverlay=uart0" >> "${DEPLOYDIR}/${BOOTFILES_DIR_NAME}/config.txt"
    # Note: dtoverlay=dwc2,dr_mode=peripheral is written by the upstream rpi-config
    # recipe when ENABLE_DWC2_PERIPHERAL=1 (set in raspberrypi5-wendyos.conf).
    # Do NOT add a second dtoverlay=dwc2 here — it would override dr_mode=peripheral.
}
