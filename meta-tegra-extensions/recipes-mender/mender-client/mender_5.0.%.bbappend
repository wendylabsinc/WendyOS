
# Fix mender-resize-data-part substitution for NVMe devices
# The upstream recipe doesn't properly substitute MENDER_DATA_PART for NVMe configs

# Set PREFERRED_VERSION to silence the major version upgrade warning
# The upstream mender_5.x.inc checks for $PREFERRED_VERSION in shell context
# We set it from PREFERRED_VERSION_mender which is configured in conf/distro/wendyos.conf
PREFERRED_VERSION = "${PREFERRED_VERSION_mender}"

do_install:prepend:class-target:mender-growfs-data:mender-systemd() {
    # Force the substitution to use the correct MENDER_DATA_PART value
    sed -i "s#@MENDER_DATA_PART@#${MENDER_DATA_PART}#g" \
        "${WORKDIR}/mender-resize-data-part.sh.in"

    sed -i "s#@MENDER_DATA_PART_NUMBER@#${MENDER_DATA_PART_NUMBER}#g" \
        "${WORKDIR}/mender-resize-data-part.sh.in"
}
