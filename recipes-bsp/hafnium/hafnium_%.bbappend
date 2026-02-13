# Skip hafnium (Secure Partition Manager) for Thor - missing toolchain dependencies
# hafnium requires gn-native and lld-native which aren't available in our layer stack
# Not needed for initial bring-up and basic functionality

do_compile() {
    # Skip - advanced security feature not needed for initial bring-up
    :
}

do_install() {
    # Create empty install directory to satisfy dependencies
    :
}

do_deploy() {
    # Skip deployment - create empty .fip to satisfy dependencies
    install -d ${DEPLOYDIR}
    touch ${DEPLOYDIR}/hafnium_t264.fip
}

ALLOW_EMPTY:${PN} = "1"
