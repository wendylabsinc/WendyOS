# Include Mender data partition image in tegraflash package
# The dataimg contains a pre-formatted ext4 filesystem for /data (mounted at boot)
# APPFILE, DTB_FILE, and DATAFILE placeholders in external-flash.xml.in
# are handled by tegra-flash-helper.sh at flash time.
DATAFILE = "${IMAGE_LINK_NAME}.dataimg"
IMAGE_TEGRAFLASH_DATA = "${IMGDEPLOYDIR}/${IMAGE_NAME}.dataimg"

# Ensure tegraflash tar waits for dataimg to be built
IMAGE_TYPEDEP:tegraflash += "dataimg"
IMAGE_TYPEDEP:tegraflash.tar += "dataimg"

# Override mender's do_copy_rootfs to avoid copyhardlinktree tar ownership issue
# (pseudo doesn't intercept chown in subprocess tar on whinlatter)
python do_copy_rootfs() {
    import shutil, os
    _from = os.path.realpath(os.path.join(d.getVar("IMAGE_ROOTFS"), "data"))
    _to = os.path.realpath(os.path.join(d.getVar("WORKDIR"), "data.copy.%s" % d.getVar('BB_CURRENTTASK')))
    if os.path.exists(_to):
        shutil.rmtree(_to)
    shutil.copytree(_from, _to, symlinks=True)
    d.setVar('_MENDER_ROOTFS_COPY', _to)
}

# Mask broken services after rootfs is assembled (after pkg_postinst scripts run).
# Masking in individual recipe bbappends breaks pkg_postinst scriptlets.

# All machines: networkd-wait-online always times out (NetworkManager is our network manager)
mask_common_services() {
    install -d ${IMAGE_ROOTFS}${sysconfdir}/systemd/system
    ln -sf /dev/null ${IMAGE_ROOTFS}${sysconfdir}/systemd/system/systemd-networkd-wait-online.service
}
ROOTFS_POSTPROCESS_COMMAND:append = " mask_common_services;"

# Thor only: nvpmodel (kernel lacks /sys/class/devfreq/bwmgr/max_freq)
#            jtop (depends on nvpmodel)
thor_mask_services() {
    ln -sf /dev/null ${IMAGE_ROOTFS}${sysconfdir}/systemd/system/nvpmodel.service
    ln -sf /dev/null ${IMAGE_ROOTFS}${sysconfdir}/systemd/system/jtop.service
}
ROOTFS_POSTPROCESS_COMMAND:append:jetson-agx-thor-devkit = " thor_mask_services;"

# Replace /var/log symlink with real directory when persistent journal is enabled.
# Yocto creates /var/log -> volatile/log (tmpfs), but systemd refuses to mount
# on non-canonical paths (symlinks), causing var-log.mount to fail.
fix_var_log_symlink() {
    if [ "${WENDYOS_PERSIST_JOURNAL_LOGS}" = "1" ] && [ -L ${IMAGE_ROOTFS}/var/log ]; then
        rm -f ${IMAGE_ROOTFS}/var/log
        mkdir -p ${IMAGE_ROOTFS}/var/log
    fi
}
ROOTFS_POSTPROCESS_COMMAND:append = " fix_var_log_symlink;"
