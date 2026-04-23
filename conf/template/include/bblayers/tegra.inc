# Tegra-exclusive layers: Mender OTA stack, meta-tegra, Tegra sub-layer.

BBLAYERS += " \
    ${TOPDIR}/../repos/meta-mender/meta-mender-core \
    ${TOPDIR}/../repos/meta-mender-community/meta-mender-tegra/meta-mender-tegra-common \
    ${TOPDIR}/../repos/meta-mender-community/meta-mender-tegra/meta-mender-tegra-jetpack6 \
    ${TOPDIR}/../repos/meta-tegra \
    ${TOPDIR}/../repos/meta-tegra-community \
    ${TOPDIR}/../${WENDYOS_META_REPO}/meta-tegra-extensions \
    "
