# Fix for Yocto 5.3 (whinlatter) UNPACKDIR changes
# Base recipe in meta-tegra R38 branch already uses UNPACKDIR correctly
# Just ensure S points to UNPACKDIR

S = "${UNPACKDIR}"

# Use UNPACKDIR for whinlatter compatibility
do_install() {
    install -m 0755 -D ${UNPACKDIR}/tar-wrapper.sh ${D}${bindir}/tar
}
