# Fix UNPACKDIR variable expansion issue in scarthgap
S = "${UNPACKDIR}"

DEPENDS:append = " tegra-helper-scripts-native"
PATH =. "${STAGING_BINDIR_NATIVE}/tegra-flash:"

# Whinlatter compatibility: Skip sstate to avoid uid/gid hash computation errors
SSTATE_SKIP_CREATION = "1"

# Skip file ownership checks for whinlatter compatibility
ERROR_QA:remove = "host-user-contaminated"
WARN_QA:append = " host-user-contaminated"

# Fix file ownership before packaging
fakeroot python do_fix_ownership() {
    import os

    dest_dir = d.getVar('D')
    if not dest_dir or not os.path.exists(dest_dir):
        bb.warn("D directory not found, skipping ownership fix")
        return

    target_uid = 0
    target_gid = 0

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

# Modify external-flash.xml to remove problematic partitions
do_install:append() {
    # Only apply to NVMe-based WendyOS machines
    case "${MACHINE}" in
        jetson-orin-nano-devkit-nvme-wendyos|jetson-agx-thor-devkit-nvme-wendyos)
            ;;
        *)
            # Not a WendyOS NVMe machine, skip modifications
            return
            ;;
    esac

    # Modify external-flash.xml
    local external_flash="${D}${datadir}/tegraflash/external-flash.xml"

    if [ ! -f "${external_flash}" ]; then
        bbwarn "external-flash.xml not found at ${external_flash}, skipping modifications"
        return
    fi

    bbnote "WendyOS: Modifying external-flash.xml for ${MACHINE}..."

    local tmpfile="${external_flash}.tmp"

    # 1. Remove "reserved" partition (blocks expansion)
    nvflashxmlparse --remove --partitions-to-remove reserved \
        --output "${tmpfile}" \
        "${external_flash}"

    # 2. Use NVIDIA's official "permanet_user_storage" partition (T264/Thor only)
    #    - Remove filename tag (no pre-flashed content needed)
    #    - Rename to "mender_data" for clarity
    #    - Increase size from 400MB to 512MB
    #    - Adjust allocation_attribute for proper expansion
    if [ "${MACHINE}" = "jetson-agx-thor-devkit-nvme-wendyos" ]; then
        # Remove filename tag from permanet_user_storage
        sed -i '/<partition name="permanet_user_storage"/,/<\/partition>/ {
            /<filename>/d
        }' "${tmpfile}"

        # Rename to mender_data
        sed -i 's/name="permanet_user_storage"/name="mender_data"/' "${tmpfile}"

        # Update size to 512MB (536870912 bytes)
        sed -i '/<partition name="mender_data"/,/<\/partition>/ s/<size> 419430400 <\/size>/<size> 536870912 <\/size>/' "${tmpfile}"

        # Update allocation_attribute from 0x808 to 0x8 (enable expansion)
        sed -i '/<partition name="mender_data"/,/<\/partition>/ s/<allocation_attribute> 0x808 <\/allocation_attribute>/<allocation_attribute> 0x8 <\/allocation_attribute>/' "${tmpfile}"

        # Add Linux filesystem GUID
        sed -i '/<partition name="mender_data"/,/<\/partition>/ {
            /<percent_reserved>/a\            <partition_type_guid> 0FC63DAF-8483-4772-8E79-3D69D8477DE4 </partition_type_guid>
        }' "${tmpfile}"
    else
        # T234 (Orin Nano) - add custom mender_data partition
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
            <description> **WendyOS/Mender.** Data partition for persistent storage (home directories, user data, Mender state). Positioned after APP_b to allow expansion to fill remaining disk space. Auto-expands via mender-grow-data.service on first boot. UDA (p15) is kept for NVIDIA compatibility but not mounted by wendyos. </description>\
        </partition>' \
            "${tmpfile}"
    fi

    # 3. Remove DATAFILE filename from UDA partition (both platforms)
    sed -i '/<partition name="UDA"/,/<\/partition>/ {
        /<filename>/d
    }' "${tmpfile}"

    # Replace the original file with the modified version
    mv "${tmpfile}" "${external_flash}"

    bbnote "WendyOS: Successfully modified external-flash.xml"
}
