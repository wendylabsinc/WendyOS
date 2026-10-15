#!/usr/bin/env bash
# Test the installer's access choice without installing packages or services.
# Variables and overrides are consumed by the dynamically extracted installer.
# shellcheck disable=SC1091,SC2034,SC2329
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
AGENT="${REPO_ROOT}/go/internal/cli/assets/docs/agent.sh"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

# Source definitions only: never dispatch to an installer branch.
awk '/^ARCH=\$\(detect_arch\)/{exit} {print}' "$AGENT" > "$TEST_DIR/helpers.sh"
source "$TEST_DIR/helpers.sh"
SUDO=""

check() {
  if [[ "$2" != "$3" ]]; then
    echo "FAIL $1: expected [$2], got [$3]" >&2
    exit 1
  fi
  echo "ok $1"
}

# Exercise detection using a real executable fixture, without installing it.
INSTALL_DIR="$TEST_DIR/bin"
BINARY_NAME="pr2240-fixture-agent"
mkdir -p "$INSTALL_DIR"
printf '#!/bin/sh\n' > "$INSTALL_DIR/$BINARY_NAME"
chmod +x "$INSTALL_DIR/$BINARY_NAME"
agent_already_installed || { echo "FAIL existing binary not detected"; exit 1; }
NETWORK_ACCESS=""
YES=true
choose_network_access "$TEST_DIR/missing"
check "legacy unattended update preserves networking" network "$NETWORK_ACCESS"
NETWORK_ACCESS=""
YES=false
have_tty() { echo "FAIL legacy update must not prompt" >&2; exit 1; }
choose_network_access "$TEST_DIR/missing"
check "legacy interactive update preserves networking without prompting" network "$NETWORK_ACCESS"
NETWORK_ACCESS=local
choose_network_access "$TEST_DIR/missing"
check "explicit revocation overrides migration" local "$NETWORK_ACCESS"
rm "$INSTALL_DIR/$BINARY_NAME"
# Isolate fresh-install cases from any service installed on the test runner.
agent_already_installed() { return 1; }
have_tty() { return 1; }
YES=false
NETWORK_ACCESS=""
choose_network_access "$TEST_DIR/missing"
check "no terminal defaults local" local "$NETWORK_ACCESS"

have_tty() { return 0; }
YES=true
NETWORK_ACCESS=""
choose_network_access "$TEST_DIR/missing"
check "-y never opts in" local "$NETWORK_ACCESS"

printf 'network\n' > "$TEST_DIR/saved"
NETWORK_ACCESS=""
choose_network_access "$TEST_DIR/saved"
check "updates preserve network choice" network "$NETWORK_ACCESS"
NETWORK_ACCESS=local
choose_network_access "$TEST_DIR/saved"
check "explicit choice overrides saved choice" local "$NETWORK_ACCESS"

printf 'invalid\n' > "$TEST_DIR/invalid"
if ( NETWORK_ACCESS=""; choose_network_access "$TEST_DIR/invalid" ) >/dev/null 2>&1; then
  echo "FAIL invalid saved choice accepted" >&2
  exit 1
fi

NETWORK_ACCESS=local
configure_network_access "$TEST_DIR/config" "$TEST_DIR/systemd"
check "persist local before service install" local "$(cat "$TEST_DIR/config/network-access")"
check "legacy registry is also local" \
  "$(printf '[Service]\nEnvironment=LISTEN_ADDRESS=127.0.0.1:5000')" \
  "$(cat "$TEST_DIR/systemd/wendyos-dev-registry.service.d/network-access.conf")"

NETWORK_ACCESS=network
configure_network_access "$TEST_DIR/config" "$TEST_DIR/systemd"
check "change persisted choice" network "$(cat "$TEST_DIR/config/network-access")"
check "legacy registry opt-in" \
  "$(printf '[Service]\nEnvironment=LISTEN_ADDRESS=0.0.0.0:5000')" \
  "$(cat "$TEST_DIR/systemd/wendyos-dev-registry.service.d/network-access.conf")"

# Exercise /dev/tty rather than stdin, which carries the script in curl | bash.
cat > "$TEST_DIR/prompt.sh" <<'EOF'
set -euo pipefail
helpers="$1"
choice_file="$2"
set --
source "$helpers"
agent_already_installed() { return 1; }
YES=false
NETWORK_ACCESS=""
choose_network_access "$choice_file"
printf 'SELECTED=%s\n' "$NETWORK_ACCESS"
EOF
python3 - "$TEST_DIR" <<'PY'
import errno
import os
import pty
import select
import signal
import sys
import time

root = sys.argv[1]
for label, answer, expected in [
    ("enter keeps local", b"\n", "local"),
    ("explicit opt-in", b"2\n", "network"),
    ("invalid answer does not opt in", b"wrong\n1\n", "local"),
]:
    pid, fd = pty.fork()
    if pid == 0:
        os.execl("/bin/bash", "bash", root + "/prompt.sh", root + "/helpers.sh", root + "/missing")
    output = b""
    sent = False
    deadline = time.monotonic() + 10
    try:
        while time.monotonic() < deadline:
            if not select.select([fd], [], [], 0.1)[0]:
                continue
            try:
                chunk = os.read(fd, 4096)
            except OSError as exc:
                if exc.errno == errno.EIO:
                    break
                raise
            if not chunk:
                break
            output += chunk
            if not sent and b"Choose [1/2, default 1]:" in output:
                os.write(fd, answer)
                sent = True
        else:
            os.kill(pid, signal.SIGKILL)
            raise AssertionError(f"prompt timed out: {output!r}")
    finally:
        os.close(fd)
        _, status = os.waitpid(pid, 0)
    assert os.waitstatus_to_exitcode(status) == 0, output
    assert sent and f"SELECTED={expected}".encode() in output, output
    print(f"ok interactive {label}")
PY

if bash "$AGENT" --network-access invalid >/dev/null 2>&1; then
  echo "FAIL invalid flag accepted" >&2
  exit 1
fi
if bash "$AGENT" --network-access >/dev/null 2>&1; then
  echo "FAIL missing flag value accepted" >&2
  exit 1
fi
