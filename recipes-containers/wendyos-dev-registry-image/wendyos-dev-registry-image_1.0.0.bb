SUMMARY = "WendyOS Development Container Registry Image"
DESCRIPTION = "Pre-built container image for the WendyOS development registry"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

PV = "1.0.7"

# Download the container image tar from GitHub releases
# Note: Using ;unpack=0 because the artifact format requires manual handling
SRC_URI = "https://github.com/wendylabsinc/containerd-registry/releases/download/v${PV}/containerd-registry-arm64.tar.gz;unpack=0"

# Checksum for v1.0.7 release
SRC_URI[sha256sum] = "af3b5a919acf2ff799b19b3fab16ca61f05f6b7192b4b16aece6a3cc0723f828"

inherit allarch

S = "${UNPACKDIR}"

# Directory where offline images are stored
OFFLINE_IMAGES_DIR = "${datadir}/wendyos/offline-images"

do_install() {
    install -d ${D}${OFFLINE_IMAGES_DIR}
    # Since we used ;unpack=0, manually decompress .tar.gz to .tar (like non-Yocto build)
    gunzip -c ${UNPACKDIR}/containerd-registry-arm64.tar.gz > ${D}${OFFLINE_IMAGES_DIR}/containerd-registry-arm64.tar
    chmod 0644 ${D}${OFFLINE_IMAGES_DIR}/containerd-registry-arm64.tar
}

FILES:${PN} = "${OFFLINE_IMAGES_DIR}/containerd-registry-arm64.tar"

# This package only contains a container image archive
ALLOW_EMPTY:${PN} = "0"

# Skip QA checks for this special package
INSANE_SKIP:${PN} = "arch"
