# Whinlatter compatibility: Fix file paths in do_install
# The upstream recipe uses hardcoded ${WORKDIR} paths which don't work
# with whinlatter's UNPACKDIR=${WORKDIR}/sources
#
# Copy all files from UNPACKDIR to WORKDIR where the recipe expects them
# This is simpler than selective copying and ensures we don't miss any files

do_install:prepend() {
    # Copy all files (not directories) from UNPACKDIR to WORKDIR
    find "${UNPACKDIR}" -maxdepth 1 -type f -exec cp {} "${WORKDIR}/" \;
}
