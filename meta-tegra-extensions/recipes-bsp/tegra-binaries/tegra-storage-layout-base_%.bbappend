DEPENDS:append = " tegra-helper-scripts-native"
PATH =. "${STAGING_BINDIR_NATIVE}/tegra-flash:"

# Override partition layouts for WendyOS Jetson machines.
# Runs AFTER meta-mender-tegra's do_install:append which injects DATAFILE into UDA.

do_install:append() {
    case "${MACHINE}" in

        jetson-orin-nano-devkit-nvme-wendyos|jetson-agx-orin-devkit-nvme-wendyos)
            # ------------------------------------------------------------------
            # NVMe: modify flash_l4t_t234_nvme_rootfs_ab.xml
            #
            # 1. Remove "reserved" partition (blocks expansion, not needed)
            # 2. Add "mender_data" (p17) after APP_b — persistent Mender store
            # 2.5. Add "wendy_config" (p16, 64 MB FAT32) before mender_data
            # 3. Remove DATAFILE from UDA (UDA kept for NVIDIA compat, not used)
            # ------------------------------------------------------------------
            local layout_file="flash_l4t_t234_nvme_rootfs_ab.xml"
            local layout_path="${D}${datadir}/l4t-storage-layout/${layout_file}"

            if [ ! -f "${layout_path}" ]; then
                bbwarn "Layout file ${layout_file} not found at ${layout_path}, skipping WendyOS NVMe modifications"
                return
            fi

            bbnote "wendyos: Modifying ${layout_file} for NVMe (mender_data + wendy_config)..."

            # 1. Remove the "reserved" partition
            nvflashxmlparse --remove --partitions-to-remove reserved \
                --output ${WORKDIR}/${layout_file}.tmp1 \
                ${layout_path}

            # 2. Add "mender_data" AFTER APP_b and BEFORE secondary_gpt
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

            # 2.5. Add wendy_config (id=16) BEFORE mender_data (id=17)
            #      Microsoft Basic Data GUID → macOS auto-mounts as /Volumes/WENDYCONFIG
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

            # 3. Remove DATAFILE from UDA (prevents flash error; UDA not mounted by WendyOS)
            sed -i '/<partition name="UDA"/,/<\/partition>/ {
                /<filename>/d
            }' ${WORKDIR}/${layout_file}.tmp1

            install -m 0644 ${WORKDIR}/${layout_file}.tmp1 ${layout_path}
            bbnote "WendyOS: Successfully modified ${layout_file} for NVMe"
            ;;

        jetson-orin-nano-devkit-wendyos)
            # ------------------------------------------------------------------
            # SD card: modify the SD template layout XML.
            #
            # With USE_REDUNDANT_FLASH_LAYOUT=1 and
            # PARTITION_LAYOUT_TEMPLATE_DEFAULT_SUPPORTS_REDUNDANT unset,
            # PARTITION_LAYOUT_TEMPLATE resolves to
            # flash_t234_qspi_sd_rootfs_ab.xml (not flash_t234_qspi_sd.xml).
            # Use ${PARTITION_LAYOUT_TEMPLATE} to follow the same variable that
            # the base recipe and meta-mender-tegra use.
            #
            # Add wendy_config (id=17, 64 MB FAT32) AFTER APP_b (id=2) in the
            # sdcard device block, immediately before secondary_gpt.
            # APP_b (id=2) is the last data partition on the SD card.
            # UDA (p15, mender data) is unaffected — no machine conf changes needed.
            # id=17 is the next free GPT slot after reserved (slot 16).
            # ------------------------------------------------------------------
            local layout_file="${PARTITION_LAYOUT_TEMPLATE}"
            local layout_path="${D}${datadir}/l4t-storage-layout/${layout_file}"

            if [ ! -f "${layout_path}" ]; then
                bbwarn "Layout file ${layout_file} not found at ${layout_path}, skipping WendyOS SD modifications"
                return
            fi

            bbnote "wendyos: Modifying ${layout_file} for SD (wendy_config)..."

            cp "${layout_path}" "${WORKDIR}/${layout_file}.tmp1"

            # Add wendy_config AFTER APP_b (id=2), before secondary_gpt.
            # The range APP_b-opening → first </partition> captures the APP_b
            # block exactly; a\ appends the new partition immediately after it.
            sed -i '/<partition name="APP_b" id="2"/,/<\/partition>/{
/<\/partition>/a\
        <partition name="wendy_config" id="17" type="data">\
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
        </partition>
}' \
                "${WORKDIR}/${layout_file}.tmp1"

            install -m 0644 "${WORKDIR}/${layout_file}.tmp1" "${layout_path}"
            bbnote "WendyOS: Successfully added wendy_config to ${layout_file}"
            ;;

        *)
            return
            ;;
    esac
}
