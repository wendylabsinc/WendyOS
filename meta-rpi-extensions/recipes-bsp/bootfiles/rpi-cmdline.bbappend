inherit partuuid-rpi

CMDLINE_ROOT_PARTITION:rpi = "PARTUUID=${WENDYOS_ROOT_PARTUUID}"
CMDLINE_ROOTFS:rpi = "console=serial0,115200 root=${CMDLINE_ROOT_PARTITION} rootfstype=ext4 fsck.repair=yes rootwait"
CMDLINE_ROOTFS:append:rpi = "${@' modules-load=dwc2' if d.getVar('WENDYOS_USB_GADGET') == '1' else ''}"
do_deploy[depends] += "${PN}:do_generate_partuuids"
