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
}

# Override tegraflash data file variable to be empty for NVMe-only configurations
# The data partition is created dynamically on first boot, not pre-flashed
DATAFILE = ""
IMAGE_TEGRAFLASH_DATA = ""
