# OpenEmbedded and poky layers shared by every board.

BBLAYERS += " \
    ${TOPDIR}/../repos/meta-openembedded/meta-filesystems \
    ${TOPDIR}/../repos/meta-openembedded/meta-multimedia \
    ${TOPDIR}/../repos/meta-openembedded/meta-networking \
    ${TOPDIR}/../repos/meta-openembedded/meta-oe \
    ${TOPDIR}/../repos/meta-openembedded/meta-python \
    ${TOPDIR}/../repos/meta-virtualization \
    ${TOPDIR}/../repos/poky/meta \
    ${TOPDIR}/../repos/poky/meta-poky \
    "
