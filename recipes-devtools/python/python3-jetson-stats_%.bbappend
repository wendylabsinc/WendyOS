# Mask jtop on Thor - it depends on nvpmodel which fails on Thor.
# Masking is done via ROOTFS_POSTPROCESS_COMMAND in wendyos-image.bbappend
# to avoid breaking pkg_postinst scriptlets.
