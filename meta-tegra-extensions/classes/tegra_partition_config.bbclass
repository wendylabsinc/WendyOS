# tegra_partition_config.bbclass
# Modify Tegra partition layouts for WendyOS
#
# Adds a do_modify_partition_layout task that runs after do_install and before
# do_populate_sysroot/do_package.  The task modifies the XML partition layout
# files installed by tegra-storage-layout-base to:
#
#   1. Remove the 'reserved' partition (blocks expansion, not needed)
#   2. Remove <filename> from UDA (kept for NVIDIA compat, not used)
#   3. Rename 'permanet_user_storage' -> 'mender_data' if present (meta-mender-
#      tegra p3768 boards), or create 'mender_data' from scratch (BSP XML)
#   4. Insert a FAT32 'config' partition before mender_data
#
# The BSP tarball's _rootfs_ab.xml does NOT contain permanet_user_storage —
# that partition only exists in meta-mender-tegra's custom XML for p3768
# upstream boards.  For WendyOS machines, mender_data is always created from
# scratch.  The rename path is kept as defensive code.
#
# Build pipeline context:
#
#   tegra-storage-layout-base do_install()       <- NVIDIA BSP installs raw XMLs
#     -> meta-mender-tegra do_install:append()   <- adds DATAFILE to UDA
#     -> do_modify_partition_layout()            <- THIS TASK
#     -> do_populate_sysroot                     <- publishes modified XMLs
#     -> tegra-storage-layout do_compile()       <- reads our XMLs, replaces
#                                                   firmware placeholders
#     -> tegraflash package                      <- final output for flashing
#
# Uses xml.etree.ElementTree (Python 3.9+) instead of sed/nvflashxmlparse for
# robust, format-independent XML manipulation.
# Precedent: nvflashxmlparse.py in meta-tegra uses the same ET library.

python do_modify_partition_layout() {
    import os
    import xml.etree.ElementTree as ET

    machine = d.getVar('MACHINE')
    destdir = d.getVar('D')
    datadir = d.getVar('datadir')

    # This is where tegra-storage-layout-base do_install() places the XMLs.
    # Our task modifies them in-place before do_populate_sysroot publishes
    # them to the sysroot for tegra-storage-layout to consume.
    layout_dir = os.path.join(destdir, datadir.lstrip('/'), 'l4t-storage-layout')

    # Read partition configuration from BitBake variables.
    # WENDYOS_CONFIG_PART_SIZE_MB: distro config (wendyos.conf), default 64 MB.
    # WENDYOS_CONFIG_PART_NUMBER: GPT partition number for config, default 16.
    # MENDER_DATA_PART_NUMBER: GPT partition number for mender_data, from machine config.
    config_size_mb = d.getVar('WENDYOS_CONFIG_PART_SIZE_MB')
    config_size_bytes = int(config_size_mb) * 1024 * 1024
    config_part_num = int(d.getVar('WENDYOS_CONFIG_PART_NUMBER') or '16')
    data_part_num = int(d.getVar('MENDER_DATA_PART_NUMBER') or '17')

    # helper: build a <partition> element with standard sub-elements
    #
    # Creates XML like:
    #   <partition name="config" id="16" type="data">
    #       <allocation_policy> sequential </allocation_policy>
    #       <filesystem_type> basic </filesystem_type>
    #       <size> 67108864 </size>
    #       ...
    #   </partition>
    #
    # The space-padded text (' value ') matches NVIDIA's XML formatting
    # convention.  nvflashxmlparse strips whitespace when reading, so this
    # is cosmetic but keeps the output consistent with upstream XMLs.
    def make_partition_element(name, part_id, size, alloc_attr, type_guid,
                               filename=None, description=None, align=16384):
        part = ET.Element('partition', name=name, id=str(part_id), type='data')
        sub = lambda tag, text: _add_sub(part, tag, text)
        sub('allocation_policy', 'sequential')
        sub('filesystem_type', 'basic')
        sub('size', str(size))
        sub('file_system_attribute', '0')
        # allocation_attribute: 0x8 = fixed size (no fill-to-end).
        # Bit 0x800 would mean "fill to end of disk" which we don't want —
        # runtime expansion via parted/mender-grow-data handles that instead.
        sub('allocation_attribute', alloc_attr)
        # partition_type_guid: GPT partition type.
        # EBD0A0A2-... = Microsoft Basic Data (FAT32, recognized by all OSes)
        # 0FC63DAF-... = Linux filesystem (ext4/data partitions)
        sub('partition_type_guid', type_guid)
        sub('percent_reserved', '0')
        # align_boundary: 16384 bytes (16 KB), matches NVIDIA convention.
        sub('align_boundary', str(align))
        if filename is not None:
            sub('filename', filename)
        if description is not None:
            sub('description', description)
        return part

    def _add_sub(parent, tag, text):
        """Add a sub-element with space-padded text to match NVIDIA XML style."""
        el = ET.SubElement(parent, tag)
        el.text = ' %s ' % text
        return el

    # helper: apply all partition modifications to a <device> element
    #
    # The NVIDIA XML structure is:
    #   <partition_layout>
    #       <device type="external" ...>      <- NVMe disk
    #           <partition name="primary_gpt">...</partition>
    #           <partition name="A_kernel">...</partition>
    #           ...boot partitions...
    #           <partition name="UDA">...</partition>
    #           <partition name="reserved">...</partition>   <- removed
    #           <partition name="APP" id="1">...</partition>
    #           <partition name="APP_b" id="2">...</partition>
    #                                                        <- config inserted here
    #                                                        <- mender_data inserted here
    #           <partition name="secondary_gpt">...</partition>
    #       </device>
    #   </partition_layout>
    #
    # After modification:
    #   ... APP_b (id=2) -> config (id=16) -> mender_data (id=17) -> secondary_gpt
    #
    # The 'id' attribute sets the GPT partition number (NOT physical order).
    # This is how APP ends up as /dev/nvme0n1p1 despite not being first
    # physically.  parted/sgdisk use these numbers, so mender-grow-data's
    # "parted resizepart 17 100%" targets the right partition regardless
    # of where config (id=16) sits physically.
    def modify_device(device, layout_path):
        # Step 1: Remove 'reserved' partition
        #
        # NVIDIA includes a ~480 MB 'reserved' partition between UDA and APP
        # "in case there is any partition change required in the future".
        # It wastes space and, more importantly, sits between UDA and the
        # rootfs partitions, blocking data partition expansion.  We remove it
        # to reclaim that space for the data partition.
        reserved = device.find('./partition[@name="reserved"]')
        if reserved is not None:
            device.remove(reserved)
            bb.note('  Removed "reserved" partition')

        # Step 2: Remove <filename> from UDA
        #
        # meta-mender-tegra's do_install:append() adds <filename>DATAFILE</filename>
        # to UDA, which tells NVIDIA flash tools to write the Mender data image
        # there.  We don't use UDA for data (we use mender_data instead), so
        # this filename must be removed.  If left, the flash tools try to write
        # a large data image into the 400 MB UDA partition, which fails.
        # The UDA partition itself is kept empty for NVIDIA compatibility.
        uda = device.find('./partition[@name="UDA"]')
        if uda is not None:
            fn = uda.find('filename')
            if fn is not None:
                uda.remove(fn)
                bb.note('  Removed <filename> from UDA')

        # Step 3: Create or rename mender_data partition
        #
        # Two possible input states:
        #
        # A) BSP XML (normal case for WendyOS machines):
        #    The XML has no permanet_user_storage.  We create mender_data
        #    from scratch and insert it before secondary_gpt.
        #
        # B) meta-mender-tegra custom XML (p3768 upstream boards only):
        #    The XML has permanet_user_storage (id=17, alloc_attr=0x808).
        #    We rename it to mender_data and fix its attributes:
        #    - size: 400 MB -> 512 MB
        #    - alloc_attr: 0x808 -> 0x8 (remove fill-to-end bit so runtime
        #      expansion via mender-grow-data.service works correctly)
        #    - add Linux filesystem GUID
        #
        # Path B is defensive code — it's not exercised by WendyOS machines
        # but is kept for upstream compatibility.
        pus = device.find('./partition[@name="permanet_user_storage"]')
        if pus is not None:
            # Path B: rename in-place
            pus.set('name', 'mender_data')
            pus.set('id', str(data_part_num))
            size_el = pus.find('size')
            if size_el is not None:
                size_el.text = ' 536870912 '
            alloc_el = pus.find('allocation_attribute')
            if alloc_el is not None:
                alloc_el.text = ' 0x8 '
            guid_el = pus.find('partition_type_guid')
            if guid_el is None:
                guid_el = ET.SubElement(pus, 'partition_type_guid')
            guid_el.text = ' 0FC63DAF-8483-4772-8E79-3D69D8477DE4 '
            desc_el = pus.find('description')
            if desc_el is None:
                desc_el = ET.SubElement(pus, 'description')
            desc_el.text = (' **WendyOS/Mender.** Data partition for persistent'
                            ' storage. Auto-expands via mender-grow-data.service'
                            ' on first boot. ')
            bb.note('  Renamed "permanet_user_storage" -> "mender_data" (id=%d)' % data_part_num)
            mender_data = pus
        else:
            # Path A: create from scratch and insert before secondary_gpt.
            # list(device).index(sec_gpt) finds the child position of
            # secondary_gpt, then device.insert(idx, ...) places mender_data
            # just before it — making mender_data the last real partition
            # on disk (before the GPT backup header).  This is required for
            # "parted resizepart <N> 100%" to expand it to fill free space.
            mender_data = make_partition_element(
                name='mender_data',
                part_id=data_part_num,
                size=536870912,
                alloc_attr='0x8',
                type_guid='0FC63DAF-8483-4772-8E79-3D69D8477DE4',
                filename='DATAFILE',
                description=('**WendyOS/Mender.** Data partition for persistent'
                             ' storage. Auto-expands via mender-grow-data.service'
                             ' on first boot.'),
            )
            sec_gpt = device.find('./partition[@name="secondary_gpt"]')
            if sec_gpt is None:
                bb.fatal('%s: "secondary_gpt" partition not found' % layout_path)
            idx = list(device).index(sec_gpt)
            device.insert(idx, mender_data)
            bb.note('  Created "mender_data" partition (id=%d)' % data_part_num)

        # Step 4: Insert 'config' partition before mender_data
        #
        # The config partition must go BEFORE mender_data (not after) because
        # mender_data must be the last real partition on disk for runtime
        # expansion to work.  "parted resizepart 17 100%" grows partition 17
        # from its start to the end of the disk — if config were after
        # mender_data, the expansion would overwrite it.
        #
        # Sanity check: APP_b must exist (A/B layout expected).
        app_b = device.find('./partition[@name="APP_b"]')
        if app_b is None:
            bb.fatal('%s: "APP_b" partition not found — expected A/B layout' % layout_path)

        config_part = make_partition_element(
            name='config',
            part_id=config_part_num,
            size=config_size_bytes,
            alloc_attr='0x8',
            type_guid='EBD0A0A2-B9E5-4433-87C0-68B6B72699C7',
            filename='config-partition.fat32.img',
            description='FAT32 configuration partition.',
        )

        # Insert config just before mender_data.
        # Final order: ... APP_b -> config -> mender_data -> secondary_gpt
        idx = list(device).index(mender_data)
        device.insert(idx, config_part)
        bb.note('  Inserted "config" partition (id=%d, %d MB)' % (config_part_num, int(config_size_mb)))

    # Determine which layout files to modify
    #
    # Instead of hardcoded machine lists, we use BitBake variables that
    # meta-tegra already defines for each machine:
    #
    # PARTITION_LAYOUT_EXTERNAL: NVMe layout filename.
    #   Set by tegra-common.inc from PARTITION_LAYOUT_EXTERNAL_DEFAULT.
    #   For NVMe machines: 'flash_l4t_t234_nvme_rootfs_ab.xml'
    #   Contains a single <device type="external"> with all NVMe partitions.
    #
    # PARTITION_LAYOUT_TEMPLATE: internal/QSPI/SD card layout filename.
    #   Set by the machine config via PARTITION_LAYOUT_TEMPLATE_DEFAULT.
    #   For NVMe machines: 'flash_t234_qspi.xml' (QSPI only, no APP_b -> skipped)
    #   For SD card machines: 'flash_t234_qspi_sd_rootfs_ab.xml' (has sdcard
    #     device block with APP_b -> modified)
    #
    # This means:
    #   NVMe machine:    only external layout modified
    #   SD card machine: both external AND template layout modified
    #   Unknown machine: nothing modified, note logged
    layout_external = d.getVar('PARTITION_LAYOUT_EXTERNAL')
    layout_template = d.getVar('PARTITION_LAYOUT_TEMPLATE')

    modified = False

    # Modify the external (NVMe) layout if present
    if layout_external:
        layout_path = os.path.join(layout_dir, layout_external)
        if os.path.exists(layout_path):
            bb.note('WendyOS: modifying %s (external/NVMe layout)...' % layout_external)
            tree = ET.parse(layout_path)
            root = tree.getroot()
            device = root.find('.//device[@type="external"]')
            if device is None:
                bb.fatal('%s: <device type="external"> not found' % layout_path)
            modify_device(device, layout_path)
            # ET.indent reformats the entire XML for readability.
            # This changes whitespace on unmodified elements too, but
            # produces clean, consistently indented output.
            ET.indent(tree, space='    ')
            tree.write(layout_path, encoding='UTF-8', xml_declaration=True)
            bb.note('WendyOS: successfully modified %s' % layout_external)
            modified = True

    # Modify the template (SD card) layout if it has APP_b
    #
    # Only modify if the template has a sdcard/sdmmc_user device block
    # containing APP_b (i.e., it's an A/B layout needing our partitions).
    # For NVMe machines, the template is QSPI-only (device type="spi",
    # no APP_b) and is correctly skipped.
    if layout_template:
        layout_path = os.path.join(layout_dir, layout_template)
        if os.path.exists(layout_path):
            tree = ET.parse(layout_path)
            root = tree.getroot()
            for dtype in ['sdcard', 'sdmmc_user']:
                device = root.find('.//device[@type="%s"]' % dtype)
                if device is not None and device.find('./partition[@name="APP_b"]') is not None:
                    bb.note('WendyOS: modifying %s (template/%s layout)...' % (layout_template, dtype))
                    modify_device(device, layout_path)
                    ET.indent(tree, space='    ')
                    tree.write(layout_path, encoding='UTF-8', xml_declaration=True)
                    bb.note('WendyOS: successfully modified %s' % layout_template)
                    modified = True
                    break

    if not modified:
        bb.note('WendyOS: no partition layouts modified for machine %s' % machine)
}

addtask modify_partition_layout after do_install before do_populate_sysroot do_package
do_modify_partition_layout[doc] = "Modify Tegra partition layouts to add config and mender_data partitions"

# Declare variable dependencies so BitBake re-runs this task when any of
# these change.  Python tasks need explicit vardeps because BitBake cannot
# auto-detect d.getVar() calls inside Python code.
do_modify_partition_layout[vardeps] += " \
    WENDYOS_CONFIG_PART_SIZE_MB \
    WENDYOS_CONFIG_PART_NUMBER \
    MENDER_DATA_PART_NUMBER \
    PARTITION_LAYOUT_EXTERNAL \
    PARTITION_LAYOUT_TEMPLATE \
    "
