# RPi-exclusive layers: meta-raspberrypi, RPi sub-layer.

BBLAYERS += " \
    ${TOPDIR}/../repos/meta-raspberrypi \
    ${TOPDIR}/../${WENDYOS_META_REPO}/meta-rpi-extensions \
    "
