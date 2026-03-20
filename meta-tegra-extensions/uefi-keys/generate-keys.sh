#!/usr/bin/env bash

set -euo pipefail


# folder where the script is located
HOME_DIR="$(realpath "${0%/*}")"

# folder from which the script was called
WORK_DIR="$(pwd)"

cleanup() {
    # preserve original exit code
    rc=$?
    cd -- "${WORK_DIR}" || true
    exit "$rc"
}
trap cleanup EXIT

# Change to script's directory so all paths are relative to it
cd "${HOME_DIR}"

# Create esl directory for EFI Signature List files
mkdir -p esl

# development/production keys (1/0)
DEV_KEYS=${DEV_KEYS:-1}
GITHUB_URL="https://raw.githubusercontent.com/tianocore/edk2/master/BaseTools/Source/Python/Pkcs7Sign"

PLATFORM_KEY="PK.pem"
KEY_EXCHANGE_KEY="KEK.pem"
DB_KEY="db.pem"
KEYS=(
  "${GITHUB_URL}|TestRoot.pub.pem|${PLATFORM_KEY}"
  "${GITHUB_URL}|TestRoot.pub.pem|${KEY_EXCHANGE_KEY}"
  "${GITHUB_URL}|TestSub.pub.pem|${DB_KEY}"
)

download() {
  local url="$1"
  local output="$2"

  # -q: quiet, --show-progress: progress bar, -O: output file
  wget -q --show-progress -O "${output}" "${url}"
}

generate_dev_keys() {
    for entry in "${KEYS[@]}"
    do
        url="${entry%%|*}"
        tmp="${entry#*|}"
        src="${tmp%%|*}"
        dst="${tmp#*|}"

        download "${url}/${src}" "${dst}"

        # Generate UUIDs and convert certificates to ESL format
        esl_file="${dst%.pem}.esl"
        cert-to-efi-sig-list -g "$(uuidgen)" "${dst}" "esl/${esl_file}"
    done
}

generate_prod_keys() {
    local key_size="${KEY_SIZE:-4096}"
    local validity_days="${CERT_VALIDITY_DAYS:-3650}"  # 10 years default
    local org_name="${ORG_NAME:-YourCompany}"

    # Check for OpenSSL
    if ! command -v openssl >/dev/null 2>&1; then
        printf "ERROR: OpenSSL not found. Install with: sudo apt-get install openssl\n" >&2
        exit 1
    fi

    printf "Generating production UEFI security keys...\n"
    printf "  Organization: %s\n" "${org_name}"
    printf "  Key size: %d bits\n" "${key_size}"
    printf "  Validity: %d days\n" "${validity_days}"
    printf "\n"

    # Platform Key (PK)
    printf "Generating Platform Key (PK)...\n"
    openssl req -new -x509 -newkey rsa:${key_size} -sha256 -days ${validity_days} \
        -nodes -keyout PK.key -out PK.pem \
        -subj "/CN=${org_name} Platform Key/"

    # Key Exchange Key (KEK)
    printf "Generating Key Exchange Key (KEK)...\n"
    openssl req -new -x509 -newkey rsa:${key_size} -sha256 -days ${validity_days} \
        -nodes -keyout KEK.key -out KEK.pem \
        -subj "/CN=${org_name} Key Exchange Key/"

    # Signature Database (db)
    printf "Generating Signature Database key (db)...\n"
    openssl req -new -x509 -newkey rsa:${key_size} -sha256 -days ${validity_days} \
        -nodes -keyout db.key -out db.pem \
        -subj "/CN=${org_name} Signature Database/"

    printf "\nWARNING: Private keys generated (.key files)\n"
    printf "  - Store securely (offline, encrypted filesystem)\n"
    printf "  - NEVER commit to version control\n"
    printf "  - Required for signing capsule updates\n"
    printf "\nGenerated files:\n"
    ls -lh PK.* KEK.* db.*

    # Convert to ESL format
    printf "\nConverting certificates to ESL format...\n"
    cert-to-efi-sig-list -g "$(uuidgen)" PK.pem "esl/PK.esl"
    cert-to-efi-sig-list -g "$(uuidgen)" KEK.pem "esl/KEK.esl"
    cert-to-efi-sig-list -g "$(uuidgen)" db.pem "esl/db.esl"

    printf "\nProduction keys generated successfully.\n"
    printf "Next steps:\n"
    printf "  1. Securely backup private keys (.key files)\n"
    printf "  2. Run script to generate UefiDefaultSecurityKeys.dts\n"
    printf "  3. Include in build and reflash device\n"
}

generate_keys() {
    if [[ 1 -eq "${DEV_KEYS}" ]]
    then
        printf "Generate development keys (EDK2 test certificates...)\n"
        printf "Download directly from EDK2 source:\n"
        printf "  '%s\n" "${GITHUB_URL}"
        generate_dev_keys
    else
        printf "Generate production keys...)\n"
        generate_prod_keys
    fi
}

generate_uefi_keys_conf() {
    # Generate uefi_keys.conf with different UEFI_DB_1_KEY_FILE based on key type:
    # - Test keys: Use db.pem (we don't have the private key, only public cert from GitHub)
    # - Production keys: Use db.key (we generated the private key ourselves)

    if [[ 1 -eq "${DEV_KEYS}" ]]; then
        cat > uefi_keys.conf <<'EOF'
# UEFI Security Keys Configuration for EDK2 Test Certificates
# WARNING: These are TEST certificates for development only!
# Production devices must use production certificates.

# Signing keys (required by gen_uefi_keys_dts.sh)
# Point to the public cert files (we don't have private keys for test certs)
UEFI_DB_1_KEY_FILE="db.pem"      # Test keys: use public cert (no private key available)
UEFI_DB_1_CERT_FILE="db.pem"

# Platform Key (root of trust)
UEFI_DEFAULT_PK_ESL="esl/PK.esl"

# Key Exchange Key (intermediate key)
UEFI_DEFAULT_KEK_ESL_0="esl/KEK.esl"

# Signature Database (authorized signatures for capsule updates)
UEFI_DEFAULT_DB_ESL_0="esl/db.esl"

# Signature Database Blocklist (optional - revoked signatures)
# UEFI_DEFAULT_DBX_ESL_0=""
EOF
    else
        cat > uefi_keys.conf <<'EOF'
# UEFI Security Keys Configuration for Production Certificates
# WARNING: Protect private keys (.key files) - they are used for capsule signing

# Signing keys (required by gen_uefi_keys_dts.sh)
UEFI_DB_1_KEY_FILE="db.key"      # Production keys: use private key (we generated it)
UEFI_DB_1_CERT_FILE="db.pem"

# Platform Key (root of trust)
UEFI_DEFAULT_PK_ESL="esl/PK.esl"

# Key Exchange Key (intermediate key)
UEFI_DEFAULT_KEK_ESL_0="esl/KEK.esl"

# Signature Database (authorized signatures for capsule updates)
UEFI_DEFAULT_DB_ESL_0="esl/db.esl"

# Signature Database Blocklist (optional - revoked signatures)
# UEFI_DEFAULT_DBX_ESL_0=""
EOF
    fi
}

download_bsp_tools() {
    local version="36.4.4"
    local major="${version%%.*}"
    local rest="${version#*.}"
    local file="jetson_linux_r36.4.4_aarch64.tbz2"
    local url="https://developer.nvidia.com/downloads/embedded/l4t/r36_release_v4.4/release/${file}"

    cd "/tmp"
    if [[ ! -f "${file}" ]]
    then
        wget "${url}"
    fi

    printf "Unpack BSP tools...\n"
    tar xjf "${file}"
}

# Generate UefiDefaultSecurityKeys.dts
generate_default_keys() {
    local config="$(realpath ${HOME_DIR}/uefi_keys.conf)"

    cd /tmp/Linux_for_Tegra

    # Run NVIDIA's script to generate DTS from ESL files
    # Provide absolute path to uefi_keys.conf
    # sudo tools/gen_uefi_keys_dts.sh $(realpath ~/wendyos/meta-wendyos/uefi-test-keys/uefi_keys.conf)
    sudo tools/gen_uefi_keys_dts.sh "${config}"

    # Expected output:
    # Info: generating default keys dtbo
    # Info: adding node PKDefault
    # Info: adding node KEKDefault
    # Info: adding node dbDefault
    # Info: dts file is generated to UefiDefaultSecurityKeys.dts
    # Info: dtbo file is generated to UefiDefaultSecurityKeys.dtbo

    # Copy generated DTS file to meta-wendyos
    # cp UefiDefaultSecurityKeys.dts ~/wendyos/meta-wendyos/uefi-test-keys/
    # cp UefiDefaultSecurityKeys.dts ~/wendyos/meta-wendyos/recipes-bsp/uefi/files/

    # cp UefiDefaultSecurityKeys.dts "${HOME_DIR}/uefi-keys"
    cp "${HOME_DIR}/UefiDefaultSecurityKeys.dts" "${HOME_DIR}/../recipes-bsp/uefi/files"

    # Verify file is not empty
    # wc -l ~/wendyos/meta-wendyos/uefi-test-keys/UefiDefaultSecurityKeys.dts
    # wc -l "${HOME_DIR}/uefi-keys/UefiDefaultSecurityKeys.dts"
    wc -l "${HOME_DIR}/../recipes-bsp/uefi/files/UefiDefaultSecurityKeys.dts"

    cd "${HOME_DIR}"
}

generate_keys
generate_uefi_keys_conf
download_bsp_tools
generate_default_keys
