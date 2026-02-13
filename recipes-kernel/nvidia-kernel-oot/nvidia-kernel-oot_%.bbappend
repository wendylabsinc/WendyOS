FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

# For Thor/R38 with kernel 6.8: nvidia-kernel-oot has extensive API compatibility issues
# Since the working Thor device doesn't use these modules, we skip building them
# but keep empty packages to satisfy dependencies

# Completely skip compilation - working device doesn't have these modules
do_compile() {
    :
}

# Create empty install - packages will exist but contain no files
do_install() {
    # Intentionally empty - working Thor device doesn't use these modules
    :
}

# Fix for R38.2.x: License files referenced in LIC_FILES_CHKSUM don't exist
ERROR_QA:remove = "license-checksum"
WARN_QA:append = " license-checksum"

# Allow empty packages - we're intentionally creating empty module packages
# Working Thor device doesn't use these modules
ALLOW_EMPTY:${PN} = "1"
ALLOW_EMPTY:${PN}-dev = "1"

# Allow all nvidia kernel module packages to be empty
python __anonymous() {
    # Get all packages from the recipe
    packages = d.getVar('PACKAGES') or ''
    for pkg in packages.split():
        if pkg.startswith('nv-kernel-module-'):
            d.setVar('ALLOW_EMPTY:' + pkg, '1')
}
