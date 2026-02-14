# Universal UNPACKDIR compatibility fix for scarthgap
#
# In Yocto Whinlatter (5.3+), UNPACKDIR is a separate directory from WORKDIR
# In Yocto Scarthgap (5.0), this variable doesn't work the same way
# This class provides compatibility by mapping UNPACKDIR to WORKDIR in scarthgap
#
# Usage: Add to INHERIT in local.conf or distro config
#        INHERIT += "scarthgap-unpackdir-compat"

# Map UNPACKDIR to WORKDIR for scarthgap compatibility
# This allows recipes written for whinlatter to work on scarthgap
UNPACKDIR = "${WORKDIR}"

# Override S if it uses UNPACKDIR to use WORKDIR instead
# Most recipes set S = "${UNPACKDIR}/something", so we catch that here
python __anonymous() {
    s = d.getVar('S')
    if s and '${UNPACKDIR}' in s:
        # Replace UNPACKDIR with WORKDIR in S variable
        s_new = s.replace('${UNPACKDIR}', '${WORKDIR}')
        d.setVar('S', s_new)
        bb.debug(2, f"scarthgap-unpackdir-compat: Rewrote S from {s} to {s_new}")
}
