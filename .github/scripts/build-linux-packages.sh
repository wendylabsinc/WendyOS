#!/usr/bin/env bash
# Shared release and PR package recipe. Builds packages only; never installs them.
set -euo pipefail
[[ $# == 5 ]] || { echo 'usage: build-linux-packages.sh VERSION ARCH CLI AGENT NEW_OUTPUT_DIRECTORY' >&2; exit 2; }
package_version=$1
package_arch=$2
[[ "$package_version" =~ ^[0-9][A-Za-z0-9.+_~-]*$ ]] || { echo 'invalid package version' >&2; exit 2; }
case "$package_arch" in
  amd64) package_rpm_arch=x86_64 ;;
  arm64) package_rpm_arch=aarch64 ;;
  *) echo 'architecture must be amd64 or arm64' >&2; exit 2 ;;
esac
package_cli=$(realpath "$3")
package_agent=$(realpath "$4")
[[ -f "$package_cli" && -x "$package_cli" && -f "$package_agent" && -x "$package_agent" ]]
[[ ! -e "$5" && ! -L "$5" ]] || { echo 'output must be a new directory' >&2; exit 2; }
mkdir -p "$5"
package_output=$(realpath "$5")
package_repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
package_work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wendy-linux-packages.XXXXXXXX")
trap 'rm -rf -- "$package_work"' EXIT
cd "$package_work"

set -euo pipefail
mkdir -p pkgroot/wendy/usr/bin
mkdir -p pkgroot/wendy/usr/lib/udev/rules.d
mkdir -p pkgroot/wendy/usr/lib/sysusers.d
mkdir -p pkgroot/wendy/usr/share/doc/wendy
mkdir -p pkgroot/wendy-agent/usr/bin
mkdir -p pkgroot/wendy-agent/usr/lib/systemd/system
mkdir -p pkgroot/wendy-agent/usr/lib/wendy-agent
mkdir -p pkgroot/wendy-agent/etc/default
mkdir -p pkgroot/wendy-agent/etc/wendy-agent
mkdir -p pkgroot/wendy-agent/etc/avahi/services
mkdir -p pkgroot/wendy-agent/var/lib/wendy-agent/storage
install -m755 "$package_cli" \
  pkgroot/wendy/usr/bin/wendy
# USB access for Jetson recovery flashing (`wendy os install`).
install -m644 "$package_repo"/packaging/linux/udev/70-wendy-jetson.rules \
  pkgroot/wendy/usr/lib/udev/rules.d/70-wendy-jetson.rules
install -m644 "$package_repo"/packaging/linux/udev/70-wendy-qualcomm.rules \
  pkgroot/wendy/usr/lib/udev/rules.d/70-wendy-qualcomm.rules
# Preserve the upstream plugdev group setup for distributions without it.
install -m644 "$package_repo"/packaging/linux/sysusers/wendy.conf \
  pkgroot/wendy/usr/lib/sysusers.d/wendy.conf
# libusb is statically linked into wendy (LGPL-2.1): ship its notice.
install -m644 "$package_repo/NOTICE" pkgroot/wendy/usr/share/doc/wendy/NOTICE
install -m755 "$package_agent" \
  pkgroot/wendy-agent/usr/bin/wendy-agent
install -m644 "$package_repo"/packaging/linux/systemd/wendy-agent.service \
  pkgroot/wendy-agent/usr/lib/systemd/system/wendy-agent.service
install -m644 "$package_repo"/packaging/linux/default/wendy-agent \
  pkgroot/wendy-agent/etc/default/wendy-agent
install -m644 "$package_repo"/packaging/linux/avahi/wendy-agent.service \
  pkgroot/wendy-agent/etc/avahi/services/wendy-agent.service
install -m755 "$package_repo"/packaging/linux/fpm/wendy-agent-setup-mdns.sh \
  pkgroot/wendy-agent/usr/lib/wendy-agent/setup-mdns.sh
# Placeholder config created at install time by package manager.
printf "{}\n" > pkgroot/wendy-agent/etc/wendy-agent/config.json
chmod 600 pkgroot/wendy-agent/etc/wendy-agent/config.json

set -euo pipefail
VERSION="$package_version"
# Build wendy .deb
fpm -s dir -t deb \
  -n wendy \
  -v "$VERSION" \
  --architecture $package_arch \
  --description "Wendy CLI - Remote device debugging and deployment" \
  --url "https://wendy.dev" \
  --maintainer "Wendy Labs Inc. <support@wendy.dev>" \
  --license "Proprietary" \
  --vendor "Wendy Labs Inc." \
  --category "devel" \
  --depends "usbutils" \
  --depends "ca-certificates" \
  --after-install "$package_repo"/packaging/linux/fpm/wendy-postinstall.sh \
  -C pkgroot/wendy \
  .
# Build wendy-agent .deb
fpm -s dir -t deb \
  -n wendy-agent \
  -v "$VERSION" \
  --architecture $package_arch \
  --description "Wendy Agent - Runs on target devices for remote debugging" \
  --url "https://wendy.dev" \
  --maintainer "Wendy Labs Inc. <support@wendy.dev>" \
  --license "Proprietary" \
  --vendor "Wendy Labs Inc." \
  --category "devel" \
  --depends "containerd" \
  --depends "xdg-dbus-proxy" \
  --depends "dbus" \
  --depends "systemd" \
  --depends "ca-certificates" \
  --depends "avahi-daemon" \
  --deb-recommends "nerdctl" \
  --deb-recommends "network-manager | connman" \
  --deb-recommends "bluez" \
  --deb-recommends "v4l2loopback-dkms (>= 0.15.0)" \
  --config-files /etc/default/wendy-agent \
  --config-files /etc/wendy-agent/config.json \
  --config-files /etc/avahi/services/wendy-agent.service \
  --after-install "$package_repo"/packaging/linux/fpm/wendy-agent-postinstall.sh \
  --before-remove "$package_repo"/packaging/linux/fpm/wendy-agent-preremove.sh \
  -C pkgroot/wendy-agent \
  .
ls -la *.deb

set -euo pipefail
VERSION="$package_version"
# RPM doesn't allow hyphens in version, replace with underscores
RPM_VERSION=$(echo "$VERSION" | tr '-' '_')
# Artifact Registry requires RPM v4; RPM 6 defaults to v6 packages.
# Build wendy .rpm
fpm -s dir -t rpm \
  --rpm-rpmbuild-define "_rpmformat 4" \
  -n wendy \
  -v "$RPM_VERSION" \
  --architecture $package_rpm_arch \
  --description "Wendy CLI - Remote device debugging and deployment" \
  --url "https://wendy.dev" \
  --maintainer "Wendy Labs Inc. <support@wendy.dev>" \
  --license "Proprietary" \
  --vendor "Wendy Labs Inc." \
  --category "Development/Tools" \
  --depends "usbutils" \
  --depends "ca-certificates" \
  --after-install "$package_repo"/packaging/linux/fpm/wendy-postinstall.sh \
  -C pkgroot/wendy \
  .
# Build wendy-agent .rpm
fpm -s dir -t rpm \
  --rpm-rpmbuild-define "_rpmformat 4" \
  -n wendy-agent \
  -v "$RPM_VERSION" \
  --architecture $package_rpm_arch \
  --description "Wendy Agent - Runs on target devices for remote debugging" \
  --url "https://wendy.dev" \
  --maintainer "Wendy Labs Inc. <support@wendy.dev>" \
  --license "Proprietary" \
  --vendor "Wendy Labs Inc." \
  --category "Development/Tools" \
  --depends "containerd" \
  --depends "xdg-dbus-proxy" \
  --depends "dbus" \
  --depends "systemd" \
  --depends "ca-certificates" \
  --depends "avahi" \
  --rpm-tag "Recommends: v4l2loopback >= 0.15.0" \
  --config-files /etc/default/wendy-agent \
  --config-files /etc/wendy-agent/config.json \
  --config-files /etc/avahi/services/wendy-agent.service \
  --after-install "$package_repo"/packaging/linux/fpm/wendy-agent-postinstall.sh \
  --before-remove "$package_repo"/packaging/linux/fpm/wendy-agent-preremove.sh \
  --rpm-posttrans "$package_repo"/packaging/linux/fpm/wendy-agent-posttrans.sh \
  -C pkgroot/wendy-agent \
  .
ls -la *.rpm

set -euo pipefail
WENDY_DEB=$(ls -1 wendy_*_$package_arch.deb | head -1)
WENDY_AGENT_DEB=$(ls -1 wendy-agent_*_$package_arch.deb | head -1)
WENDY_RPM=$(ls -1 wendy-[0-9]*.rpm | head -1)
WENDY_AGENT_RPM=$(ls -1 wendy-agent-*.rpm | head -1)
# rpm can read v6 packages that Artifact Registry rejects. RPM v4
# uses a 3.0 lead; check it explicitly before publishing artifacts.
for package in "$WENDY_RPM" "$WENDY_AGENT_RPM"; do
  lead=$(od -An -tx1 -N6 "$package" | tr -d ' \n')
  if [[ "$lead" != "edabeedb0300" ]]; then
    echo "::error file=$package::Expected an RPM v4 package with a 3.0 lead, got $lead"
    exit 1
  fi
done
dpkg-deb -I "$WENDY_DEB" | grep -E "Depends:.*usbutils"
dpkg-deb -I "$WENDY_DEB" | grep -E "Depends:.*ca-certificates"
dpkg-deb -c "$WENDY_DEB" | grep "usr/lib/udev/rules.d/70-wendy-jetson.rules" > /dev/null
dpkg-deb -c "$WENDY_DEB" | grep "usr/lib/udev/rules.d/70-wendy-qualcomm.rules" > /dev/null
dpkg-deb -c "$WENDY_DEB" | grep "usr/lib/sysusers.d/wendy.conf" > /dev/null
dpkg-deb -c "$WENDY_DEB" | grep "usr/share/doc/wendy/NOTICE" > /dev/null
dpkg-deb -I "$WENDY_AGENT_DEB" | grep -E "Depends:.*containerd"
dpkg-deb -I "$WENDY_AGENT_DEB" | grep -E "Depends:.*dbus"
dpkg-deb -I "$WENDY_AGENT_DEB" | grep -E "Depends:.*systemd"
dpkg-deb -I "$WENDY_AGENT_DEB" | grep -E "Depends:.*ca-certificates"
dpkg-deb -c "$WENDY_AGENT_DEB" | grep "usr/lib/systemd/system/wendy-agent.service" > /dev/null
dpkg-deb -c "$WENDY_AGENT_DEB" | grep "etc/default/wendy-agent" > /dev/null
dpkg-deb -c "$WENDY_AGENT_DEB" | grep "etc/avahi/services/wendy-agent.service" > /dev/null
dpkg-deb -I "$WENDY_AGENT_DEB" | grep -E "Depends:.*avahi-daemon"
rpm -qp --requires "$WENDY_RPM" | grep "^usbutils" > /dev/null
rpm -qp --requires "$WENDY_RPM" | grep "^ca-certificates" > /dev/null
rpm -qp --requires "$WENDY_AGENT_RPM" | grep "^containerd" > /dev/null
rpm -qp --requires "$WENDY_AGENT_RPM" | grep "^dbus" > /dev/null
rpm -qp --requires "$WENDY_AGENT_RPM" | grep "^systemd" > /dev/null
rpm -qp --requires "$WENDY_AGENT_RPM" | grep "^ca-certificates" > /dev/null
rpm -qp --requires "$WENDY_AGENT_RPM" | grep "^avahi" > /dev/null
rpm -qpl "$WENDY_RPM" | grep "/usr/lib/udev/rules.d/70-wendy-jetson.rules" > /dev/null
rpm -qpl "$WENDY_RPM" | grep "/usr/lib/udev/rules.d/70-wendy-qualcomm.rules" > /dev/null
rpm -qpl "$WENDY_RPM" | grep "/usr/lib/sysusers.d/wendy.conf" > /dev/null
rpm -qpl "$WENDY_RPM" | grep "/usr/share/doc/wendy/NOTICE" > /dev/null
rpm -qpl "$WENDY_AGENT_RPM" | grep "/usr/lib/systemd/system/wendy-agent.service" > /dev/null
rpm -qpl "$WENDY_AGENT_RPM" | grep "/etc/default/wendy-agent" > /dev/null
rpm -qpl "$WENDY_AGENT_RPM" | grep "/etc/avahi/services/wendy-agent.service" > /dev/null
rpm -qp --scripts "$WENDY_AGENT_RPM" | grep "^posttrans scriptlet" > /dev/null

cp -- ./*.deb ./*.rpm "$package_output/"
