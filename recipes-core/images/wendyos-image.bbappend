EXTRA_IMAGEDEPENDS:append:tegra = " wendy-config-partition"

# Replace placeholders in external-flash.xml.in for NVMe flash images
# This ensures DTB_FILE, DATAFILE, and APPFILE are replaced with actual filenames
# Uses the tegraflash_custom_post hook which runs after XML creation but before archiving

tegraflash_custom_post:append() {
    if [ -f "external-flash.xml.in" ]; then
        # Get the actual DTB filename
        DTB_NAME="$(basename ${KERNEL_DEVICETREE})"

        # Replace placeholders with actual filenames
        sed -i \
            -e "s,DTB_FILE,${DTB_NAME}," \
            -e "s,DATAFILE,${IMAGE_LINK_NAME}.dataimg," \
            -e "s,APPFILE_b,${IMAGE_BASENAME}.ext4," \
            -e "s,APPFILE,${IMAGE_BASENAME}.ext4," \
            external-flash.xml.in

        bbnote "Replaced placeholders in external-flash.xml.in"
        bbnote "  DTB_FILE -> ${DTB_NAME}"
        bbnote "  DATAFILE -> ${IMAGE_LINK_NAME}.dataimg"
        bbnote "  APPFILE -> ${IMAGE_BASENAME}.ext4"
    else
        bberror "external-flash.xml.in not found in tegraflash_custom_post"
        bberror "Current directory: $(pwd)"
        bberror "Files present: $(ls -la)"
    fi

    # Copy wendy-config FAT32 image into the tegraflash package directory so
    # tegraparser_v2 can find it when processing external-flash.xml.in.
    # create_tegraflash_pkg never auto-includes files from DEPLOY_DIR_IMAGE
    # just because they are referenced in a partition layout XML — each file
    # must be copied explicitly.
    if [ -f "${DEPLOY_DIR_IMAGE}/wendy-config.fat32.img" ]; then
        cp "${DEPLOY_DIR_IMAGE}/wendy-config.fat32.img" ./wendy-config.fat32.img
        bbnote "Copied wendy-config.fat32.img into tegraflash package"
    else
        bberror "wendy-config.fat32.img not found in DEPLOY_DIR_IMAGE (${DEPLOY_DIR_IMAGE})"
        bberror "Ensure wendy-config-partition is listed in EXTRA_IMAGEDEPENDS and has run do_deploy"
    fi
}
