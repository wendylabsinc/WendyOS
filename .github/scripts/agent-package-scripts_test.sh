#!/usr/bin/env bash
# .github/scripts/agent-package-scripts_test.sh
# Tests that the wendy-agent package scripts keep the agent running across
# upgrades and stop it on removal, with dpkg and rpm arguments.
set -euo pipefail
cd "$(dirname "$0")"

FPM="$(cd ../.. && pwd)/packaging/linux/fpm"

# The scripts do nothing without systemd; GitHub's Ubuntu runners have it.
if [ ! -d /run/systemd/system ]; then
  echo "skip: /run/systemd/system is missing"
  exit 0
fi

fail=0
check() { if [ "$2" != "$3" ]; then echo "FAIL $1: expected [$2] got [$3]"; fail=1; else echo "ok $1"; fi; }

# Fake systemctl: logs every call except the reload, answers is-enabled and
# is-active from $FAKE_ENABLED and $FAKE_ACTIVE.
BIN="$(mktemp -d)"
CALLS="$(mktemp)"
cat > "$BIN/systemctl" <<EOF
#!/usr/bin/env bash
case "\$1" in
  daemon-reload) exit 0 ;;
  is-enabled) [ "\${FAKE_ENABLED:-yes}" = yes ]; exit \$? ;;
  is-active) [ "\${FAKE_ACTIVE:-yes}" = yes ]; exit \$? ;;
esac
echo "\$*" >> "$CALLS"
EOF
chmod +x "$BIN/systemctl"

# run SCRIPT [ARGS...] prints the wendy-agent systemctl calls the script made.
run() {
  : > "$CALLS"
  PATH="$BIN:$PATH" bash "$FPM/$1" "${@:2}" >/dev/null
  grep -E ' wendy-agent$' "$CALLS" | paste -sd ';' - || true
}

# preremove: stop only on removal.
check "preremove.deb.remove" "stop wendy-agent" "$(run wendy-agent-preremove.sh remove)"
check "preremove.deb.upgrade" "" "$(run wendy-agent-preremove.sh upgrade 2026.10.18-000000)"
check "preremove.deb.failed-upgrade" "" "$(run wendy-agent-preremove.sh failed-upgrade 2026.10.17-000000)"
check "preremove.deb.deconfigure" "stop wendy-agent" "$(run wendy-agent-preremove.sh deconfigure)"
check "preremove.rpm.erase" "stop wendy-agent" "$(run wendy-agent-preremove.sh 0)"
check "preremove.rpm.upgrade" "" "$(run wendy-agent-preremove.sh 1)"

# postinstall: an enabled agent is restarted even if an older preremove stopped it.
check "postinstall.enabled" "restart wendy-agent" \
  "$(FAKE_ENABLED=yes run wendy-agent-postinstall.sh configure 2026.10.17-000000)"
check "postinstall.fresh" "enable --now wendy-agent" \
  "$(FAKE_ENABLED=no run wendy-agent-postinstall.sh configure)"

# posttrans: start an enabled agent that an older rpm preremove stopped.
check "posttrans.stopped" "start wendy-agent" "$(FAKE_ENABLED=yes FAKE_ACTIVE=no run wendy-agent-posttrans.sh)"
check "posttrans.running" "" "$(FAKE_ENABLED=yes FAKE_ACTIVE=yes run wendy-agent-posttrans.sh)"
check "posttrans.disabled" "" "$(FAKE_ENABLED=no FAKE_ACTIVE=no run wendy-agent-posttrans.sh)"

rm -rf "$BIN" "$CALLS"
exit "$fail"
