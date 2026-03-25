DEPENDS:append = " tegra-helper-scripts-native"
PATH =. "${STAGING_BINDIR_NATIVE}/tegra-flash:"

# Override NVMe partition layout for WendyOS to:
# 1. Remove "reserved" partition (between UDA and APP)
# 2. Rename "permanet_user_storage" (p17) to "mender_data"
# 3. Update mender_data size to 512MB (will auto-expand)
# 4. Change allocation_attribute from 0x808 to 0x8 (allow expansion)
# 5. Add partition type GUID for Linux filesystem
#
# This runs AFTER meta-mender-tegra's do_install:append which creates the _rootfs_ab.xml variant

do_install:append() {
    # Only apply to NVMe variants (both Nano and AGX use same T234 partition layout)
    case "${MACHINE}" in
        jetson-orin-nano-devkit-nvme-wendyos|jetson-agx-orin-devkit-nvme-wendyos)
            ;;
        *)
            return
            ;;
    esac

    # Modify the _rootfs_ab.xml file created by meta-mender-tegra
    local layout_file="flash_l4t_t234_nvme_rootfs_ab.xml"
    local layout_path="${D}${datadir}/l4t-storage-layout/${layout_file}"

    if [ ! -f "${layout_path}" ]; then
        bbwarn "Layout file ${layout_file} not found at ${layout_path}, skipping WendyOS modifications"
        return
    fi

    bbnote "wendyos: Modifying ${layout_file} to use mender_data partition..."

    # 1. Remove the "reserved" partition (between UDA and APP)
    #    This partition blocks expansion and is not needed
    nvflashxmlparse --remove --partitions-to-remove reserved \
        --output ${WORKDIR}/${layout_file}.tmp1 \
        ${layout_path}

    # 2. Add new "mender_data" partition AFTER APP_b and BEFORE secondary_gpt
    #    Insert the partition definition using sed
    sed -i '/<partition name="secondary_gpt"/i\
        <partition name="mender_data" id="17" type="data">\
            <allocation_policy> sequential </allocation_policy>\
            <filesystem_type> basic </filesystem_type>\
            <size> 536870912 </size>\
            <file_system_attribute> 0 </file_system_attribute>\
            <allocation_attribute> 0x8 </allocation_attribute>\
            <partition_type_guid> 0FC63DAF-8483-4772-8E79-3D69D8477DE4 </partition_type_guid>\
            <percent_reserved> 0 </percent_reserved>\
            <align_boundary> 16384 </align_boundary>\
            <filename> DATAFILE </filename>\
            <description> **WendyOS/Mender.** Data partition for persistent storage (home directories, user data, Mender state). Positioned after APP_b to allow expansion to fill remaining disk space. Auto-expands via mender-grow-data.service on first boot. UDA (p15) is kept for NVIDIA compatibility but not mounted by wendyos. </description>\
        </partition>' \
        ${WORKDIR}/${layout_file}.tmp1

    # 2.5. Add wendy_config partition (id=16) BEFORE mender_data (id=17)
    #      64 MB FAT32, Microsoft Basic Data GUID → macOS auto-mounts as /Volumes/WENDYCONFIG
    sed -i '/<partition name="mender_data" id="17"/i\
        <partition name="wendy_config" id="16" type="data">\
            <allocation_policy> sequential </allocation_policy>\
            <filesystem_type> basic </filesystem_type>\
            <size> 67108864 </size>\
            <file_system_attribute> 0 </file_system_attribute>\
            <allocation_attribute> 0x8 </allocation_attribute>\
            <partition_type_guid> EBD0A0A2-B9E5-4433-87C0-68B6B72699C7 </partition_type_guid>\
            <percent_reserved> 0 </percent_reserved>\
            <align_boundary> 16384 </align_boundary>\
            <filename> wendy-config.fat32.img </filename>\
            <description> WendyOS first-boot config partition (FAT32, 64 MB). </description>\
        </partition>' \
        ${WORKDIR}/${layout_file}.tmp1

    # 3. Remove DATAFILE filename from UDA partition
    #    Prevent flash error when dataimg is larger than UDA partition
    #    UDA is not used by WendyOS (mender_data is used instead)
    #    UDA is kept for NVIDIA compatibility but should not have pre-written content
    #    The filename field causes flash tools to fail during signing
    sed -i '/<partition name="UDA"/,/<\/partition>/ {
        /<filename>/d
    }' ${WORKDIR}/${layout_file}.tmp1

    # Install the modified layout
    install -m 0644 ${WORKDIR}/${layout_file}.tmp1 ${layout_path}

    bbnote "WendyOS: Successfully added mender_data partition to ${layout_file}"
}
