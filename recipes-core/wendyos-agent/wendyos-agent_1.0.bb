
# [Note]
# This recipe fetches the wendyos-agent binary from GitHub at build time
# inside do_compile using wget/curl, bypassing SRC_URI checksums and breaking
# build reproducibility (two builds may produce different binaries).
# It also uses 'SRCREV = "${AUTOREV}"'' for the source repo.
#
# [Fix]
# Pin the binary download URL and its sha256sum in SRC_URI, or use a proper
# recipe with SRC_URI[sha256sum].
# Runtime self-update should remain in wendyos-agent-updater.service,
# not at build time.

SUMMARY = "WendyOS Agent"
DESCRIPTION = "WendyOS agent binary for device management"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = "file://wendyos-agent.service \
           file://wendyos-agent-updater.service \
           file://wendyos-agent-updater.timer \
           file://wendyos-agent-updater.sh \
           file://download-wendyos-agent.sh"

S = "${WORKDIR}"

inherit systemd

SYSTEMD_SERVICE:${PN} = "wendyos-agent.service wendyos-agent-updater.service wendyos-agent-updater.timer"
SYSTEMD_AUTO_ENABLE:${PN} = "enable"

do_compile() {
    bbnote "Downloading wendy-agent binary for aarch64..."

    # Get the latest stable release from GitHub (excludes pre-releases)
    RELEASES_URL="https://api.github.com/repos/wendylabsinc/wendy-agent/releases/latest"

    # Fetch latest stable release
    wget -q -O ${B}/release.json "${RELEASES_URL}" || \
        curl -sL -o ${B}/release.json "${RELEASES_URL}" || \
        bbfatal "Failed to fetch latest release from GitHub"

    # Extract download URL for aarch64 binary (match .tar.gz files only)
    # Asset naming: wendy-agent-linux-arm64-*.tar.gz (formerly wendy-agent-linux-static-musl-aarch64)
    DOWNLOAD_URL=$(cat ${B}/release.json | \
        grep -o '"browser_download_url"[[:space:]]*:[[:space:]]*"[^"]*wendy-agent-linux-arm64[^"]*\.tar\.gz[^"]*"' | \
        head -1 | cut -d'"' -f4)

    if [ -z "${DOWNLOAD_URL}" ]; then
        bbfatal "Failed to find wendy-agent-linux-static-musl-aarch64 binary in release"
    fi

    bbnote "Downloading from: ${DOWNLOAD_URL}"

    # Download the binary archive
    wget -O ${B}/wendy-agent.tar.gz "${DOWNLOAD_URL}" || \
        curl -L -o ${B}/wendy-agent.tar.gz "${DOWNLOAD_URL}" || \
        bbfatal "Failed to download wendy-agent binary"

    # Extract the archive
    tar -xzf ${B}/wendy-agent.tar.gz -C ${B}

    # Find and prepare the binary (exclude wendy-cli)
    if [ ! -f ${B}/wendy-agent ]; then
        BINARY=$(find ${B} -name wendy-agent -type f ! -path "*/wendy-cli*" | head -1)
        if [ -n "${BINARY}" ]; then
            mv "${BINARY}" ${B}/wendy-agent
        else
            bbfatal "wendy-agent binary not found in archive"
        fi
    fi

    chmod +x ${B}/wendy-agent
    bbnote "wendy-agent binary ready"
}

do_install() {
    # Install the pre-downloaded binary
    install -d ${D}${bindir}
    install -m 0755 ${B}/wendy-agent ${D}${bindir}/wendy-agent

    # Install systemd services
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/wendyos-agent.service ${D}${systemd_system_unitdir}/
    install -m 0644 ${WORKDIR}/wendyos-agent-updater.service ${D}${systemd_system_unitdir}/
    install -m 0644 ${WORKDIR}/wendyos-agent-updater.timer ${D}${systemd_system_unitdir}/

    # Install updater and download scripts
    install -d ${D}/opt/wendyos/bin
    install -m 0755 ${WORKDIR}/wendyos-agent-updater.sh ${D}/opt/wendyos/bin/
    install -m 0755 ${WORKDIR}/download-wendyos-agent.sh ${D}/opt/wendyos/bin/

    # Create runtime directories
    install -d ${D}/var/lib/wendyos-agent
    install -d ${D}/var/lib/wendy-agent
    install -d ${D}/opt/wendy
}

FILES:${PN} = "${bindir}/* \
               /opt/wendyos/bin/* \
               /opt/wendy \
               ${systemd_system_unitdir}/* \
               /var/lib/wendyos-agent \
               /var/lib/wendy-agent"

# Allow network access during build
do_compile[network] = "1"

# Skip QA checks for pre-built binary
INSANE_SKIP:${PN} += "already-stripped"

# Runtime dependencies
# curl/wget needed for auto-updater, tar for extraction
RDEPENDS:${PN} = "bash curl tar"