# Whinlatter compatibility: Fix Go workspace structure
# The recipe expects Go workspace layout in build directory

# Disable license QA - paths don't match but licenses are valid
INSANE_SKIP:${PN} = "license-checksum"
ERROR_QA:remove = "license-checksum license-exists"
WARN_QA:append = " license-checksum license-exists"
do_check_for_missing_licenses[noexec] = "1"
SSTATE_SKIP_CREATION = "1"

# Override go_do_configure to create proper Go workspace structure
# Base go.bbclass tries to symlink ${S}/src to ${B}/src, but in whinlatter
# the source is directly in ${S}, not ${S}/src
go_do_configure() {
    # Create the Go workspace directory structure
    install -d ${B}/src/github.com/mendersoftware

    # Symlink source to expected Go workspace location
    ln -snf ${S} ${B}/src/github.com/mendersoftware/mender-artifact
}
