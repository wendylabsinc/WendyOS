DEPENDS:append = " tegra-helper-scripts-native"
PATH =. "${STAGING_BINDIR_NATIVE}/tegra-flash:"

# Whinlatter compatibility: Skip sstate to avoid uid/gid hash computation errors
# The base recipe creates files with host UID/GID that pseudo doesn't properly handle
# Skipping sstate allows packaging to complete successfully
SSTATE_SKIP_CREATION = "1"

# Skip file ownership checks for whinlatter compatibility
# Pseudo doesn't properly handle ownership for this recipe's files
ERROR_QA:remove = "host-user-contaminated"
WARN_QA:append = " host-user-contaminated"

# Fix file ownership before packaging to avoid RPM errors
# The base recipe creates files that don't go through pseudo correctly
fakeroot python do_fix_ownership() {
    import os

    dest_dir = d.getVar('D')
    if not dest_dir or not os.path.exists(dest_dir):
        bb.warn("D directory not found, skipping ownership fix")
        return

    # Get root UID/GID (0) for target
    target_uid = 0
    target_gid = 0

    # Walk through all files and fix ownership
    file_count = 0
    for root, dirs, files in os.walk(dest_dir):
        for item in dirs + files:
            path = os.path.join(root, item)
            if os.path.exists(path):
                try:
                    os.lchown(path, target_uid, target_gid)
                    file_count += 1
                except Exception as e:
                    bb.warn(f"Failed to chown {path}: {e}")

    bb.note(f"Fixed ownership for {file_count} files in {dest_dir}")
}

addtask do_fix_ownership after do_install before do_package

# Override NVMe partition layout for WendyOS to:
# 1. Remove "reserved" partition (between UDA and APP)
# 2. Rename "permanet_user_storage" (p17) to "mender_data"
# 3. Update mender_data size to 512MB (will auto-expand)
# 4. Change allocation_attribute from 0x808 to 0x8 (allow expansion)
# 5. Add partition type GUID for Linux filesystem
#
# This runs AFTER meta-mender-tegra's do_install:append which creates the _rootfs_ab.xml variant

do_install:append() {
    # Only apply to NVMe-based WendyOS machines
    # Determine the layout file based on machine type
    local layout_file=""
    case "${MACHINE}" in
        jetson-orin-nano-devkit-nvme-wendyos)
            layout_file="flash_l4t_t234_nvme_rootfs_ab.xml"
            ;;
        jetson-agx-thor-devkit-nvme-wendyos)
            layout_file="flash_l4t_t264_nvme_rootfs_ab.xml"
            ;;
        *)
            # Not a WendyOS NVMe machine, skip modifications
            return
            ;;
    esac

    local layout_path="${D}${datadir}/l4t-storage-layout/${layout_file}"

    if [ ! -f "${layout_path}" ]; then
        bbwarn "Layout file ${layout_file} not found at ${layout_path}, skipping WendyOS modifications"
        return
    fi

    bbnote "WendyOS: Modifying ${layout_file} for ${MACHINE} to use mender_data partition..."

    # Create a temporary file in tmpdir (managed by BitBake with proper pseudo context)
    local tmpfile="${layout_path}.tmp"

    # 1. Remove blocking partitions
    #    - "reserved": blocks expansion and is not needed
    #    - "permanet_user_storage": NVIDIA's data partition (only exists on T264/Thor)
    #       We'll replace it with our own mender_data partition
    local partitions_to_remove="reserved"

    # T264 (Thor) has "permanet_user_storage" partition that we need to remove
    if [ "${MACHINE}" = "jetson-agx-thor-devkit-nvme-wendyos" ]; then
        partitions_to_remove="reserved permanet_user_storage"
    fi

    nvflashxmlparse --remove --partitions-to-remove ${partitions_to_remove} \
        --output "${tmpfile}" \
        "${layout_path}"

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
        "${tmpfile}"

    # 3. Remove DATAFILE filename from UDA partition
    #    Prevent flash error when dataimg is larger than UDA partition
    #    UDA is not used by WendyOS (mender_data is used instead)
    #    UDA is kept for NVIDIA compatibility but should not have pre-written content
    #    The filename field causes flash tools to fail during signing
    sed -i '/<partition name="UDA"/,/<\/partition>/ {
        /<filename>/d
    }' "${tmpfile}"

    # Replace the original file with the modified version
    # Using mv instead of install to preserve proper pseudo-managed ownership
    mv "${tmpfile}" "${layout_path}"

    bbnote "WendyOS: Successfully added mender_data partition to ${layout_file}"
}
