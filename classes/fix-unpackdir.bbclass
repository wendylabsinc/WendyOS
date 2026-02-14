# Compatibility fix for meta-tegra R38 recipes using UNPACKDIR with scarthgap
# The R38 branch uses S = "${UNPACKDIR}" which causes variable expansion errors
# This class automatically fixes affected recipes by overriding S to use WORKDIR

# Only apply this fix if S is set to UNPACKDIR
python __anonymous() {
    s = d.getVar('S')
    if s and s == d.getVar('UNPACKDIR'):
        # Override S to use WORKDIR instead
        d.setVar('S', d.getVar('WORKDIR'))
        bb.note("Fixed UNPACKDIR compatibility issue for %s" % d.getVar('PN'))
}
