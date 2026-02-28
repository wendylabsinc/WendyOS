SUMMARY = "UEFI certificate enrollment for capsule update authentication"
LICENSE = "MIT"

FILESEXTRAPATHS:prepend := "${THISDIR}/files:"

# Add UefiDefaultSecurityKeys.dts containing EDK2 test certificates
# This enables UEFI capsule update authentication on the device
#
# IMPORTANT: This file must be generated using NVIDIA's gen_uefi_keys_dts.sh tool
# See uefi-test-keys/README.md for detailed instructions
#
# The DTS file contains:
# - PKDefault: Platform Key (root of trust)
# - KEKDefault: Key Exchange Key
# - dbDefault: Signature Database (authorized capsule signatures)
#
# These certificates match the EDK2 test certificates used to sign
# capsules in tegra-uefi-capsule-signing.bbclass
SRC_URI += "file://UefiDefaultSecurityKeys.dts"

# Verify the file is not empty (catches placeholder files)
do_configure:prepend() {
    if [ ! -s "${WORKDIR}/UefiDefaultSecurityKeys.dts" ]; then
        bbfatal "UefiDefaultSecurityKeys.dts is empty! See uefi-test-keys/README.md for generation instructions."
    fi

    # Log that we're using custom security keys
    bbnote "Using WendyOS UEFI security keys for capsule update authentication"
}
