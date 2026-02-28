# Skip X11 dependency for headless/container use
# DeepStream has some X11-linked binaries but they're not needed for container deployments

# Skip QA checks for missing dependencies
INSANE_SKIP:${PN} += "file-rdeps"
INSANE_SKIP:${PN}-samples += "file-rdeps"

# Prevent automatic dependency detection from adding X11 libs
# These libraries link to X11 but we don't need that functionality
PRIVATE_LIBS:${PN} = "libX11.so.6"
PRIVATE_LIBS:${PN}-samples = "libX11.so.6"

# Also skip FILEDEPS scanning for these packages to avoid RPM dep generation
SKIP_FILEDEPS:${PN} = "1"
SKIP_FILEDEPS:${PN}-samples = "1"

# Fix missing libcivetweb.so.1 symlink (required by GStreamer plugins)
do_install:append() {
    if [ -f ${D}/opt/nvidia/deepstream/deepstream-7.1/lib/libcivetweb.so.1.16.0 ]; then
        cd ${D}/opt/nvidia/deepstream/deepstream-7.1/lib
        ln -sf libcivetweb.so.1.16.0 libcivetweb.so.1
    fi
}
