# Skip UEFI capsule updates for Thor - causes build failures
# Can be re-enabled later once the build issues are resolved

do_compile() {
    # Skip - bootloader updates not needed for initial bring-up
    :
}

do_install() {
    # Create empty install directory to satisfy dependencies
    :
}

do_deploy() {
    # Skip deployment
    :
}

# Allow empty package
ALLOW_EMPTY:${PN} = "1"
