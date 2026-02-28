# Only runs on RPi machines (rpi-config recipe only exists in meta-raspberrypi)
do_deploy:append:rpi() {
    # enable_uart=1 is already written by the upstream rpi-config recipe when
    # ENABLE_UART=1 (set in raspberrypi5-wendyos.conf). Do not duplicate it here.
    echo "dtoverlay=uart0" >> "${DEPLOYDIR}/${BOOTFILES_DIR_NAME}/config.txt"
    # Note: dtoverlay=dwc2,dr_mode=peripheral is written by the upstream rpi-config
    # recipe when ENABLE_DWC2_PERIPHERAL=1 (set in raspberrypi5-wendyos.conf).
    # Do NOT add a second dtoverlay=dwc2 here — it would override dr_mode=peripheral.
}
