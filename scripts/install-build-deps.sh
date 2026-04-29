#!/usr/bin/env bash
#
# Install the host packages needed to run a Yocto / WendyOS build.
# Sourced by both scripts/docker/dockerfile (local-dev container) and
# ci/packer/wendyos-builder.pkr.hcl (CI runner AMI), so the package list
# stays in one place.
#
# Idempotent: safe to re-run. Must be invoked as root (or via sudo).

set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
    echo "install-build-deps.sh: must be run as root" >&2
    exit 1
fi

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get -qy upgrade

# Yocto / OE build prerequisites for Scarthgap on Ubuntu 24.04, plus a few
# wendyos-specific extras (mender, jetson tegraflash, image builder).
apt-get -qy install \
    gawk wget git-core diffstat unzip texinfo \
    build-essential chrpath socat cpio \
    python3 python3-pip python3-pexpect python3-venv python3-git \
    xz-utils bzip2 libxml2-utils debianutils iputils-ping \
    python3-jinja2 python3-yaml libsdl1.2-dev xterm make \
    xsltproc docbook-utils fop dblatex xmlto git-lfs u-boot-tools \
    strace tree fdisk efitools uuid-runtime rsync \
    lz4 zstd liblz4-tool graphviz \
    python3-gi python3-gi-cairo gir1.2-gtk-3.0 x11-apps \
    libncurses5-dev libncursesw5-dev bison flex patchutils \
    libssl-dev ca-certificates locales sudo mc quilt vim pkg-config \
    gdisk dosfstools bmap-tools device-tree-compiler \
    awscli

# gcc-multilib (32-bit x86 multilib headers) is only available on amd64.
# Skip on arm64 (e.g. Apple Silicon Docker Desktop, Graviton runners).
if [[ "$(dpkg --print-architecture)" == "amd64" ]]; then
    apt-get -qy install gcc-multilib
fi

apt-get -qy clean
rm -rf /var/lib/apt/lists/*

# Yocto requires a fully-formed UTF-8 locale. Without LANG / LC_ALL set, the
# do_compile tasks fail on locale-sensitive scripts (e.g. perl).
locale-gen --purge en_US.UTF-8
update-locale LC_ALL=en_US.UTF-8 LANG=en_US.UTF-8

cat > /etc/default/locale <<'EOF'
LANG=en_US.UTF-8
LC_NUMERIC=en_US.UTF-8
LC_TIME=en_US.UTF-8
LC_MONETARY=en_US.UTF-8
LC_PAPER=en_US.UTF-8
LC_NAME=en_US.UTF-8
LC_ADDRESS=en_US.UTF-8
LC_TELEPHONE=en_US.UTF-8
LC_MEASUREMENT=en_US.UTF-8
LC_IDENTIFICATION=en_US.UTF-8
LANGUAGE=en_US
EOF
