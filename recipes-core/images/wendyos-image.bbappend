# Replace placeholders in external-flash.xml.in for NVMe flash images
# This ensures DTB_FILE, DATAFILE, and APPFILE are replaced with actual filenames
# Uses the tegraflash_custom_post hook which runs after XML creation but before archiving

# Ensure config-partition FAT32 image is built before the tegraflash package
EXTRA_IMAGEDEPENDS:append:tegra = " config-partition"

tegraflash_custom_post:append() {
    # Copy config partition FAT32 image into the tegraflash package (all Tegra machines)
    if [ -f "${DEPLOY_DIR_IMAGE}/config-partition.fat32.img" ]; then
        cp "${DEPLOY_DIR_IMAGE}/config-partition.fat32.img" ./config-partition.fat32.img
        bbnote "Copied config-partition.fat32.img into tegraflash package"
    else
        bbfatal "config-partition.fat32.img not found in DEPLOY_DIR_IMAGE"
    fi

    # Replace placeholders in external-flash.xml.in (NVMe machines only)
    if [ -f "external-flash.xml.in" ]; then
        DTB_NAME="$(basename ${KERNEL_DEVICETREE})"
        sed -i \
            -e "s,DTB_FILE,${DTB_NAME}," \
            -e "s,DATAFILE,${IMAGE_LINK_NAME}.dataimg," \
            -e "s,APPFILE_b,${IMAGE_BASENAME}.ext4," \
            -e "s,APPFILE,${IMAGE_BASENAME}.ext4," \
            external-flash.xml.in
        bbnote "Replaced placeholders in external-flash.xml.in"
    fi
}

# Add fstab entry for config partition on Jetson (RPi5 has it in rpi-fstab)
add_config_fstab() {
    echo "LABEL=config  /config  vfat  defaults,noauto,nofail  0 0" >> ${IMAGE_ROOTFS}${sysconfdir}/fstab
    mkdir -p ${IMAGE_ROOTFS}/config
}
ROOTFS_POSTPROCESS_COMMAND:append:tegra = " add_config_fstab;"
