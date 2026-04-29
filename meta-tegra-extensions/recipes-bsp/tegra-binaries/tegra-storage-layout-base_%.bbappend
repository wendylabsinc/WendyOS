# Override partition layouts for WendyOS Jetson machines.
# All logic lives in tegra_partition_config.bbclass.
inherit tegra_partition_config

# meta-mender-tegra calls mender_flash_layout_adjust "${PARTITION_LAYOUT_EXTERNAL}"
# unconditionally, but eMMC machines set PARTITION_LAYOUT_EXTERNAL = "" to suppress
# NVMe external layout generation.  The empty argument causes nvflashxmlparse to
# receive "" as its output path and fail with IsADirectoryError.
# This layer (priority 51) is processed after meta-mender-tegra, so this definition
# replaces the upstream one.  The guard makes the call a no-op for empty filenames.
mender_flash_layout_adjust() {
    local file=$1
    [ -n "$file" ] || return 0
    mv ${D}${datadir}/l4t-storage-layout/$file ${WORKDIR}/$file
    nvflashxmlparse -v --rewrite-contents-from=${WORKDIR}/UDA.xml \
        --output=${D}${datadir}/l4t-storage-layout/$file \
        ${WORKDIR}/$file
}
