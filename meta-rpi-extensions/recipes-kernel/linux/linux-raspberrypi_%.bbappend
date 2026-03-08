FILESEXTRAPATHS:prepend := "${THISDIR}/linux-raspberrypi:"

# Add container support kernel config when WENDYOS_CONTAINER_RUNTIME is enabled
SRC_URI:append:rpi = "${@' file://container.cfg' if d.getVar('WENDYOS_CONTAINER_RUNTIME') == '1' else ''}"

# Add USB gadget kernel config when WENDYOS_USB_GADGET is enabled
SRC_URI:append:rpi = "${@' file://usb-gadget.cfg' if d.getVar('WENDYOS_USB_GADGET') == '1' else ''}"
SRC_URI += "file://0001-dwc2-force-g_dma-false-for-BCM2712-in-peripheral-mod.patch"

