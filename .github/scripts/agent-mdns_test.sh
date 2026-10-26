#!/usr/bin/env bash
# Run the actual package hook against an isolated /etc tree (GNU sed required).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$TEST_DIR/etc/avahi/services" "$TEST_DIR/etc/wendy-agent" "$TEST_DIR/etc/wendyos"
sed "s|/etc/|$TEST_DIR/etc/|g" "$ROOT/packaging/linux/fpm/wendy-agent-setup-mdns.sh" > "$TEST_DIR/hook.sh"
printf 'regression-device-id\n' > "$TEST_DIR/etc/wendyos/device-uuid"
printf 'regression-device\n' > "$TEST_DIR/etc/wendyos/device-name"
printf 'local\n' > "$TEST_DIR/etc/wendy-agent/network-access"
active="$TEST_DIR/etc/avahi/services/wendy-agent.service"
cp "$ROOT/packaging/linux/avahi/wendy-agent.service" "$active"
bash "$TEST_DIR/hook.sh"
[[ ! -f "$active" ]]
grep -q 'id=regression-device-id<' "$active.local-only"
if grep -q 'WENDY_DEVICE_' "$active.local-only"; then
  echo 'FAIL hidden advertisement retains template placeholders' >&2
  exit 1
fi
# Policy.UpdateAdvertisements restores this exact file when opting in.
mv "$active.local-only" "$active"
grep -q 'name=regression-device<' "$active"
printf 'network\n' > "$TEST_DIR/etc/wendy-agent/network-access"
bash "$TEST_DIR/hook.sh"
grep -q 'id=regression-device-id<' "$active"
echo 'ok local installation retains populated identity through network opt-in'
