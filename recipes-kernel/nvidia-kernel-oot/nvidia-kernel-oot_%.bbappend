# Fix for Yocto whinlatter (5.3) - multi-git recipe with custom destsuffix
# nvidia-kernel-oot uses 9 git repos with explicit destsuffix=${BPN}-${PV}/subdir
# BitBake auto-sets S="${UNPACKDIR}/git" but sources land in ${BPN}-${PV}/
# The insane.bbclass do_qa_unpack check fires on any S containing 'git',
# so we override it to use the correct path and skip the stale check.
S = "${UNPACKDIR}/${BPN}-${PV}"

python do_qa_unpack() {
    # nvidia-kernel-oot uses multi-git with custom destsuffix, so S != ${UNPACKDIR}/git
    # The default do_qa_unpack in insane.bbclass fires bb.fatal() for this pattern.
    # We override it here since we explicitly manage S for this multi-source recipe.
    import os
    s_dir = d.getVar('S')
    if not os.path.exists(s_dir):
        bb.warn('%s: S directory %s does not exist' % (d.getVar('PN'), s_dir))
}

