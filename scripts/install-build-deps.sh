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
    gdisk dosfstools bmap-tools device-tree-compiler

# gcc-multilib (32-bit x86 multilib headers) is only available on amd64.
# Skip on arm64 (e.g. Apple Silicon Docker Desktop, Graviton runners).
if [[ "$(dpkg --print-architecture)" == "amd64" ]]; then
    apt-get -qy install gcc-multilib
fi

apt-get -qy clean
rm -rf /var/lib/apt/lists/*

# Detect host arch once; the AWS CLI and s5cmd installers below pick the
# right release tarball based on it.
arch="$(dpkg --print-architecture)"

# AWS CLI v2. Ubuntu 24.04 (Noble) dropped the legacy 'awscli' apt package
# (it was AWS CLI v1, which AWS itself deprecated). Install the official
# bundle from awscli.amazonaws.com; idempotent thanks to '--update'.
case "${arch}" in
    amd64) aws_arch="x86_64" ;;
    arm64) aws_arch="aarch64" ;;
    *)     echo "Unsupported arch for aws-cli: ${arch}" >&2; exit 1 ;;
esac
tmp_aws=$(mktemp -d)
wget -qO "${tmp_aws}/awscliv2.zip" \
    "https://awscli.amazonaws.com/awscli-exe-linux-${aws_arch}.zip"
unzip -q "${tmp_aws}/awscliv2.zip" -d "${tmp_aws}"
"${tmp_aws}/aws/install" --update -i /usr/local/aws-cli -b /usr/local/bin
rm -rf "${tmp_aws}"
/usr/local/bin/aws --version

# s5cmd: highly parallel S3 client. Used by .github/workflows/build.yml's
# sstate / downloads cache restore + save steps. For the millions-of-tiny-files
# shape of sstate-cache it is roughly an order of magnitude faster than
# `aws s3 sync`. Pinned to a specific release so the AMI bake is reproducible.
S5CMD_VERSION="2.2.2"
case "${arch}" in
    amd64) s5cmd_arch="64bit" ;;
    arm64) s5cmd_arch="arm64" ;;
    *)     echo "Unsupported arch for s5cmd: ${arch}" >&2; exit 1 ;;
esac
tmp_s5=$(mktemp -d)
wget -qO "${tmp_s5}/s5cmd.tar.gz" \
    "https://github.com/peak/s5cmd/releases/download/v${S5CMD_VERSION}/s5cmd_${S5CMD_VERSION}_Linux-${s5cmd_arch}.tar.gz"
tar -xzf "${tmp_s5}/s5cmd.tar.gz" -C "${tmp_s5}" s5cmd
install -m 0755 "${tmp_s5}/s5cmd" /usr/local/bin/s5cmd
rm -rf "${tmp_s5}"
/usr/local/bin/s5cmd version

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
