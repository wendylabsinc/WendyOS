#!/usr/bin/env bash
# .github/scripts/install-scripts_test.sh
# Tests the shared resolver block (Task 1) and cli.sh deferral (Task 2).
set -euo pipefail
cd "$(dirname "$0")"

REPO_ROOT="$(cd ../.. && pwd)"
CLI="${REPO_ROOT}/go/internal/cli/assets/docs/cli.sh"
AGENT="${REPO_ROOT}/go/internal/cli/assets/docs/agent.sh"
BEGIN='# >>> wendy-install-shared'
END='# <<< wendy-install-shared'

fail=0
check() { if [ "$2" != "$3" ]; then echo "FAIL $1: expected [$2] got [$3]"; fail=1; else echo "ok $1"; fi; }
contains() { case "$2" in *"$3"*) echo "ok $1";; *) echo "FAIL $1: [$2] does not contain [$3]"; fail=1;; esac; }
absent()  { case "$2" in *"$3"*) echo "FAIL $1: [$2] unexpectedly contains [$3]"; fail=1;; *) echo "ok $1";; esac; }

# Extract the marked block from a script (exclusive of the marker lines).
extract_block() { awk "/${BEGIN}/{f=1;next} /${END}/{f=0} f" "$1"; }

# --- Test A: both scripts carry a byte-identical shared block ---
cli_block="$(extract_block "$CLI")"
agent_block="$(extract_block "$AGENT")"
check "block.nonempty" "yes" "$([ -n "$cli_block" ] && echo yes || echo no)"
check "block.identical" "yes" "$([ "$cli_block" = "$agent_block" ] && echo yes || echo no)"

# --- Harness: fake curl/wget servable from a table of url->file, logging calls ---
setup_net() { # $1 = dir with manifest.json / github.json (optional)
  BIN="$(mktemp -d)"; REQ_LOG="$(mktemp)"; SERVE_DIR="$1"
  cat > "$BIN/curl" <<EOF
#!/usr/bin/env bash
url=""; out=""
while [ \$# -gt 0 ]; do
  case "\$1" in
    -o) out="\$2"; shift 2;;
    http*|https*) url="\$1"; shift;;
    *) shift;;
  esac
done
echo "\$url" >> "$REQ_LOG"
case "\$url" in
  *install.wendy.dev/manifest.json) src="$SERVE_DIR/manifest.json";;
  *api.github.com/*) src="$SERVE_DIR/github.json";;
  */releases/download/*) src="$SERVE_DIR/\${url##*/}";;
  */repo-signing-key.gpg) src="$SERVE_DIR/repo-signing-key.gpg";;
  *) src="";;
esac
[ -n "\$src" ] && [ -f "\$src" ] || exit 22   # mimic curl -f on missing/non-2xx
if [ -n "\$out" ]; then cat "\$src" > "\$out"; else cat "\$src"; fi
EOF
  cp "$BIN/curl" "$BIN/wget" 2>/dev/null || true  # not used, but present
  chmod +x "$BIN/curl" "$BIN/wget"
}

# Build a script that sources ONLY the shared block, then calls resolve_version.
run_resolver() { # env: WENDY_VERSION optional
  local tmp; tmp="$(mktemp)"
  # Mirror the real scripts' shell options so the test catches errexit bugs
  # (a failing command substitution under `set -e` must NOT abort the fallback).
  { echo 'set -euo pipefail'; echo 'REPO="wendylabsinc/wendy-agent"'; extract_block "$CLI"; echo 'resolve_version'; } > "$tmp"
  PATH="$BIN:$PATH" bash "$tmp"
}

# --- Test B: WENDY_VERSION override wins ---
D="$(mktemp -d)"; setup_net "$D"
printf '{"latest":"2026.01.01-000000"}\n' > "$D/manifest.json"
export WENDY_VERSION=9.9.9
out="$(run_resolver)"
unset WENDY_VERSION
check "resolve.override" "9.9.9" "$out"
absent "resolve.override.no_net" "$(cat "$REQ_LOG")" "manifest.json"

# --- Test C: GCS manifest latest is preferred ---
D="$(mktemp -d)"; setup_net "$D"
printf '{"latest":"2026.07.19-143000","latest_nightly":"2026.07.20-010101"}\n' > "$D/manifest.json"
printf '{"tag_name":"2000.00.00-000000"}\n' > "$D/github.json"
out="$(run_resolver)"
check "resolve.gcs" "2026.07.19-143000" "$out"
contains "resolve.gcs.hit_manifest" "$(cat "$REQ_LOG")" "install.wendy.dev/manifest.json"

# --- Test D: falls back to GitHub when manifest is missing ---
D="$(mktemp -d)"; setup_net "$D"    # no manifest.json in dir
printf '{"tag_name":"2026.07.18-120000"}\n' > "$D/github.json"
out="$(run_resolver)"
check "resolve.fallback" "2026.07.18-120000" "$out"
contains "resolve.fallback.hit_github" "$(cat "$REQ_LOG")" "api.github.com"

# --- Test E: cli.sh Homebrew path makes zero GitHub/manifest calls (deferral) ---
D="$(mktemp -d)"; setup_net "$D"           # curl fails on every URL and logs it
printf '{"latest":"2026.07.19-143000"}\n' > "$D/manifest.json"
STUB="$(mktemp -d)"
# uname stub: pretend Apple Silicon macOS so the darwin/brew branch is taken.
cat > "$STUB/uname" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  -s) echo "Darwin";;
  -m) echo "arm64";;
  *) echo "Darwin";;
esac
EOF
# brew stub: present, but "brew help trust" fails so the trust steps are skipped;
# every other subcommand is a successful no-op.
cat > "$STUB/brew" <<'EOF'
#!/usr/bin/env bash
[ "$1" = "help" ] && exit 1
exit 0
EOF
chmod +x "$STUB/uname" "$STUB/brew"
: > "$REQ_LOG"
PATH="$STUB:$BIN:$PATH" bash "$CLI" -y >/dev/null 2>&1 || true
absent "defer.no_github"   "$(cat "$REQ_LOG")" "api.github.com"
absent "defer.no_manifest" "$(cat "$REQ_LOG")" "install.wendy.dev/manifest.json"

# ===== No-TTY installs (agent shells, CI, `curl | bash` with no terminal) =====

# no_tty runs a command with no controlling terminal. setsid detaches it from
# the session's tty, so opening /dev/tty fails with ENXIO exactly as it does in
# an agent shell — even when this test itself is run from a terminal.
no_tty() {
  perl -MPOSIX -e 'my $pid = fork() // die "fork: $!"; if ($pid) { waitpid($pid, 0); exit($? >> 8) } POSIX::setsid() or die "setsid: $!"; exec @ARGV or die "exec: $!"' "$@"
}

# Only system dirs on PATH, so a developer's Homebrew/package managers can't leak in.
BASE_PATH="/usr/bin:/bin:/usr/sbin:/sbin"

# make_stubs OS ARCH: fresh STUB dir with uname (reporting OS/ARCH), id (a
# non-root user) and sudo (logs its args to SUDO_LOG, then fails the way
# `sudo -n` does with no cached credentials).
make_stubs() {
  STUB="$(mktemp -d)"; SUDO_LOG="$(mktemp)"
  cat > "$STUB/uname" <<EOF
#!/usr/bin/env bash
case "\$1" in -s) echo "$1";; -m) echo "$2";; *) echo "$1";; esac
EOF
  cat > "$STUB/id" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = "-u" ]; then echo 1000; else exec /usr/bin/id "$@"; fi
EOF
  cat > "$STUB/sudo" <<EOF
#!/usr/bin/env bash
echo "\$*" >> "$SUDO_LOG"
echo "sudo: a password is required" >&2
exit 1
EOF
  chmod +x "$STUB/uname" "$STUB/id" "$STUB/sudo"
}

# serve_cli_release DIR OS ARCH: manifest.json plus the matching release
# tarball (holding a stub `wendy`) for the fake curl to serve.
serve_cli_release() {
  local dir="$1" os="$2" arch="$3" v="2026.07.19-143000" pkg
  printf '{"latest":"%s"}\n' "$v" > "$dir/manifest.json"
  pkg="$(mktemp -d)"
  mkdir -p "$pkg/wendy-cli-${os}-${arch}"
  printf '#!/bin/sh\necho "wendy version %s"\n' "$v" > "$pkg/wendy-cli-${os}-${arch}/wendy"
  chmod +x "$pkg/wendy-cli-${os}-${arch}/wendy"
  tar -czf "$dir/wendy-cli-${os}-${arch}-${v}.tar.gz" -C "$pkg" "wendy-cli-${os}-${arch}"
}

# run_no_tty OUT SCRIPT ARGS...: SCRIPT with no terminal, stdin from
# /dev/null, the stub PATH and a throwaway HOME. Returns the script's exit code.
# SH (default bash) is the shell command that runs it, e.g. SH="bash --posix".
run_no_tty() {
  local out="$1" script="$2"; shift 2
  # shellcheck disable=SC2086 # SH is a command plus its options
  no_tty env PATH="$STUB:$BIN:$BASE_PATH" HOME="$FAKE_HOME" ${SH:-bash} "$script" "$@" </dev/null >"$out" 2>&1
}

# cli_with_default_dir DIR prints the path of a copy of cli.sh whose built-in
# default install dir is DIR instead of /usr/local/bin. The ~/.local/bin
# fallback only applies to the default dir (an explicit -d never relocates), so
# this is how the tests reach it without going near the real /usr/local/bin.
# Aborts the suite if the rewrite didn't apply, rather than let a test run
# against the real default.
cli_with_default_dir() {
  local copy; copy="$(mktemp)"
  sed "s|^INSTALL_DIR=\"/usr/local/bin\"\$|INSTALL_DIR=\"$1\"|" "$CLI" > "$copy"
  if ! grep -qxF "INSTALL_DIR=\"$1\"" "$copy"; then
    echo "FAIL harness: could not rewrite the default INSTALL_DIR in cli.sh"
    exit 1
  fi
  echo "$copy"
}

# --- Test F: no TTY, Homebrew path, no -y: proceeds instead of dying on /dev/tty ---
make_stubs Darwin arm64
BREW_LOG="$(mktemp)"
cat > "$STUB/brew" <<EOF
#!/usr/bin/env bash
echo "\$*" >> "$BREW_LOG"
[ "\$1" = "help" ] && exit 1
exit 0
EOF
chmod +x "$STUB/brew"
D="$(mktemp -d)"; setup_net "$D"; FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"
rc=0; run_no_tty "$OUT" "$CLI" || rc=$?
check "notty.brew.exit" "0" "$rc"
contains "notty.brew.autoyes" "$(cat "$OUT")" "continuing as if -y was passed"
contains "notty.brew.installed" "$(cat "$BREW_LOG")" "install wendylabsinc/tap/wendy"
absent "notty.brew.no_tty_error" "$(cat "$OUT")" "/dev/tty"
absent "notty.brew.no_tour_hint" "$(cat "$OUT")" "tour"

# Root ignores directory permissions, so the read-only-dir cases need a normal user.
if [ "$(/usr/bin/id -u)" -ne 0 ]; then
  # --- Test G: no TTY, default install dir unwritable, sudo needs a password: ~/.local/bin ---
  make_stubs Darwin arm64
  D="$(mktemp -d)"; setup_net "$D"; serve_cli_release "$D" darwin arm64
  FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; RO="$(mktemp -d)"; chmod 555 "$RO"
  SCRIPT="$(cli_with_default_dir "$RO")"
  rc=0; run_no_tty "$OUT" "$SCRIPT" || rc=$?
  chmod 755 "$RO"
  check "notty.fallback.exit" "0" "$rc"
  check "notty.fallback.binary" "yes" "$([ -x "$FAKE_HOME/.local/bin/wendy" ] && echo yes || echo no)"
  check "notty.fallback.ro_untouched" "no" "$([ -e "$RO/wendy" ] && echo yes || echo no)"
  contains "notty.fallback.says_where" "$(cat "$OUT")" "installing to $FAKE_HOME/.local/bin instead"
  check "notty.fallback.one_path_line" "1" "$(grep -c 'export PATH=' "$OUT")"
  check "notty.fallback.sudo_only_probed" "-n true" "$(sort -u "$SUDO_LOG")"

  # --- Test N: an explicit -d that can't be written fails with one line; never relocates ---
  make_stubs Darwin arm64
  D="$(mktemp -d)"; setup_net "$D"; serve_cli_release "$D" darwin arm64
  FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; RO="$(mktemp -d)"; chmod 555 "$RO"
  rc=0; run_no_tty "$OUT" "$CLI" -d "$RO" || rc=$?
  chmod 755 "$RO"
  check "notty.explicit_dir.exit" "1" "$rc"
  contains "notty.explicit_dir.says_why" "$(cat "$OUT")" "Error: $RO is not writable"
  contains "notty.explicit_dir.says_what_to_do" "$(cat "$OUT")" "Re-run with -d <writable dir>."
  check "notty.explicit_dir.one_error_line" "1" "$(grep -c 'Error:' "$OUT")"
  check "notty.explicit_dir.no_fallback" "no" "$([ -e "$FAKE_HOME/.local/bin/wendy" ] && echo yes || echo no)"
  check "notty.explicit_dir.ro_untouched" "no" "$([ -e "$RO/wendy" ] && echo yes || echo no)"
  check "notty.explicit_dir.sudo_only_probed" "-n true" "$(sort -u "$SUDO_LOG")"

  # --- Test J: Linux, apt present, no TTY, no passwordless sudo: standalone binary ---
  make_stubs Linux x86_64
  APT_LOG="$(mktemp)"
  printf '#!/usr/bin/env bash\necho "$*" >> "%s"\n' "$APT_LOG" > "$STUB/apt-get"; chmod +x "$STUB/apt-get"
  D="$(mktemp -d)"; setup_net "$D"; serve_cli_release "$D" linux amd64
  FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; RO="$(mktemp -d)"; chmod 555 "$RO"
  SCRIPT="$(cli_with_default_dir "$RO")"
  rc=0; run_no_tty "$OUT" "$SCRIPT" || rc=$?
  chmod 755 "$RO"
  check "notty.linux.exit" "0" "$rc"
  check "notty.linux.no_apt" "" "$(cat "$APT_LOG")"
  contains "notty.linux.says_standalone" "$(cat "$OUT")" "installing the standalone binary"
  check "notty.linux.binary" "yes" "$([ -x "$FAKE_HOME/.local/bin/wendy" ] && echo yes || echo no)"
fi

# --- Test H: no TTY, writable install dir: installs there without touching sudo ---
make_stubs Darwin arm64
D="$(mktemp -d)"; setup_net "$D"; serve_cli_release "$D" darwin arm64
FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; DEST="$(mktemp -d)"
rc=0; run_no_tty "$OUT" "$CLI" -d "$DEST" || rc=$?
check "notty.writable.exit" "0" "$rc"
check "notty.writable.binary" "yes" "$([ -x "$DEST/wendy" ] && echo yes || echo no)"
check "notty.writable.no_sudo" "" "$(cat "$SUDO_LOG")"

# --- Test K: both installers' confirm() proceeds without a terminal ---
for script in "$CLI" "$AGENT"; do
  tmp="$(mktemp)"
  { echo 'set -euo pipefail'; echo 'YES=false'; extract_block "$script"
    awk '/^confirm\(\) \{/{f=1} f{print} f&&/^\}/{exit}' "$script"
    echo 'confirm "Proceed?"; echo "rc=$?"'; } > "$tmp"
  out="$(no_tty bash "$tmp" </dev/null 2>&1 || true)"
  name="$(basename "$script")"
  contains "notty.confirm.${name}.proceeds" "$out" "rc=0"
  contains "notty.confirm.${name}.says_so" "$out" "continuing as if -y was passed"
done

# with_pty runs a command attached to a fresh pseudo-terminal (so /dev/tty
# works), feeding this function's stdin to it as keystrokes. BSD/macOS and
# util-linux `script` take different arguments.
with_pty() {
  if script --version >/dev/null 2>&1; then
    script -qec "$(printf '%q ' "$@")" /dev/null
  else
    script -q /dev/null "$@"
  fi
}

# --- Test L: with a terminal, the prompt is still shown and "n" aborts ---
make_stubs Darwin arm64
BREW_LOG="$(mktemp)"
cat > "$STUB/brew" <<EOF
#!/usr/bin/env bash
echo "\$*" >> "$BREW_LOG"
[ "\$1" = "help" ] && exit 1
exit 0
EOF
chmod +x "$STUB/brew"
D="$(mktemp -d)"; setup_net "$D"; FAKE_HOME="$(mktemp -d)"
# Keep stdin open past the answer: BSD script ends the session at stdin EOF.
out="$({ printf 'n\n'; sleep 3; } | with_pty env PATH="$STUB:$BIN:$BASE_PATH" HOME="$FAKE_HOME" bash "$CLI" 2>&1 || true)"
contains "tty.prompted" "$out" "Proceed? [y/N]"
contains "tty.aborted" "$out" "Aborted."
absent "tty.no_install" "$(cat "$BREW_LOG")" "install"

# --- Test M: no TTY, no sudo, default dir unwritable, HOME unset: one clear error, not "unbound variable" ---
if [ "$(/usr/bin/id -u)" -ne 0 ]; then
  make_stubs Darwin arm64
  D="$(mktemp -d)"; setup_net "$D"; serve_cli_release "$D" darwin arm64
  OUT="$(mktemp)"; RO="$(mktemp -d)"; chmod 555 "$RO"
  SCRIPT="$(cli_with_default_dir "$RO")"
  rc=0; no_tty env -u HOME PATH="$STUB:$BIN:$BASE_PATH" bash "$SCRIPT" </dev/null >"$OUT" 2>&1 || rc=$?
  chmod 755 "$RO"
  check "notty.nohome.exit" "1" "$rc"
  contains "notty.nohome.says_why" "$(cat "$OUT")" "HOME is unset. Re-run with -d <writable dir>."
  absent "notty.nohome.no_unbound" "$(cat "$OUT")" "unbound variable"
fi

# --- Test P: the same no-TTY installs under POSIX-mode shells ---
# `sh cli.sh` (and `curl … | sh`) run the installer with whatever sh is: dash
# on Debian/Ubuntu, where a script run from a file re-execs itself under bash
# (piped, it can't — see Test W), but bash itself on Fedora/RHEL and macOS,
# where it runs in POSIX mode. In POSIX mode a failed redirection on
# a special builtin (`: </dev/tty`) exits the whole script, so every terminal
# probe (confirm, can_elevate, the tour check) must survive there. The POSIX
# case uses the bash running this suite: macOS's /bin/bash 3.2 predates the rule.
for sh in "sh" "$BASH --posix"; do
  tag="$(basename "${sh%% *}")${sh#"${sh%% *}"}"; tag="${tag// /_}"
  # macOS without Homebrew, no -y: confirm() and the tour check probe the terminal.
  make_stubs Darwin arm64
  D="$(mktemp -d)"; setup_net "$D"; serve_cli_release "$D" darwin arm64
  FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; DEST="$(mktemp -d)"
  rc=0; SH="$sh" run_no_tty "$OUT" "$CLI" -d "$DEST" || rc=$?
  check "posix.${tag}.prompt.exit" "0" "$rc"
  check "posix.${tag}.prompt.binary" "yes" "$([ -x "$DEST/wendy" ] && echo yes || echo no)"
  contains "posix.${tag}.prompt.autoyes" "$(cat "$OUT")" "continuing as if -y was passed"

  # -y: only the tour check probes the terminal.
  make_stubs Darwin arm64
  D="$(mktemp -d)"; setup_net "$D"; serve_cli_release "$D" darwin arm64
  FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; DEST="$(mktemp -d)"
  rc=0; SH="$sh" run_no_tty "$OUT" "$CLI" -y -d "$DEST" || rc=$?
  check "posix.${tag}.yes.exit" "0" "$rc"
  contains "posix.${tag}.yes.summary" "$(cat "$OUT")" "Installed to $DEST/wendy"

  # Linux, -y: can_elevate probes the terminal before trying sudo -n.
  make_stubs Linux x86_64
  D="$(mktemp -d)"; setup_net "$D"; serve_cli_release "$D" linux amd64
  FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; DEST="$(mktemp -d)"
  rc=0; SH="$sh" run_no_tty "$OUT" "$CLI" -y -d "$DEST" || rc=$?
  check "posix.${tag}.linux.exit" "0" "$rc"
  check "posix.${tag}.linux.binary" "yes" "$([ -x "$DEST/wendy" ] && echo yes || echo no)"
done

# Piped, the way `curl … | sh -s -- -y` runs it where sh is bash.
make_stubs Darwin arm64
D="$(mktemp -d)"; setup_net "$D"; serve_cli_release "$D" darwin arm64
FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; DEST="$(mktemp -d)"
rc=0; no_tty env PATH="$STUB:$BIN:$BASE_PATH" HOME="$FAKE_HOME" "$BASH" --posix -s -- -y -d "$DEST" <"$CLI" >"$OUT" 2>&1 || rc=$?
check "posix.piped.exit" "0" "$rc"
check "posix.piped.binary" "yes" "$([ -x "$DEST/wendy" ] && echo yes || echo no)"

# --- Test S: after installing to ~/.local/bin, an older `wendy` earlier on PATH ---
# The summary used to say "Installed successfully!" and print the OLD binary's
# version. It must report the binary it installed and name the one shadowing it.
if [ "$(/usr/bin/id -u)" -ne 0 ]; then
  make_stubs Linux x86_64
  printf '#!/usr/bin/env bash\necho "apt-get $*"\n' > "$STUB/apt-get"; chmod +x "$STUB/apt-get"
  OLD="$(mktemp -d)"
  printf '#!/bin/sh\necho "wendy version 2026.01.01-OLD"\n' > "$OLD/wendy"; chmod +x "$OLD/wendy"
  # dpkg owns the old binary, as it would after an earlier apt install (here
  # of a differently named package, so the hint must use dpkg's answer).
  printf '#!/bin/sh\n[ "$1" = "-S" ] && [ "$2" = "%s/wendy" ] && echo "wendy-legacy: %s/wendy"\n' "$OLD" "$OLD" > "$STUB/dpkg-query"; chmod +x "$STUB/dpkg-query"
  D="$(mktemp -d)"; setup_net "$D"; serve_cli_release "$D" linux amd64
  FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; RO="$(mktemp -d)"; chmod 555 "$RO"
  SCRIPT="$(cli_with_default_dir "$RO")"
  rc=0; no_tty env PATH="$STUB:$BIN:$OLD:$BASE_PATH" HOME="$FAKE_HOME" bash "$SCRIPT" </dev/null >"$OUT" 2>&1 || rc=$?
  chmod 755 "$RO"
  check "shadow.exit" "0" "$rc"
  absent "shadow.no_false_success" "$(cat "$OUT")" "Installed successfully!"
  contains "shadow.new_version" "$(cat "$OUT")" "wendy version 2026.07.19-143000"
  absent "shadow.no_old_version" "$(cat "$OUT")" "2026.01.01-OLD"
  contains "shadow.names_old" "$(cat "$OUT")" "on your PATH is $OLD/wendy"
  contains "shadow.package_hint" "$(cat "$OUT")" "apt package wendy-legacy"
  contains "shadow.upgrade_hint" "$(cat "$OUT")" "sudo apt-get install --only-upgrade wendy-legacy"
  absent "shadow.no_remove_advice" "$(cat "$OUT")" "remove"
  contains "shadow.path_fix" "$(cat "$OUT")" "export PATH=\"$FAKE_HOME/.local/bin:\$PATH\""

  # Same install with ~/.local/bin already first on PATH: a plain success.
  make_stubs Linux x86_64
  D="$(mktemp -d)"; setup_net "$D"; serve_cli_release "$D" linux amd64
  FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; RO="$(mktemp -d)"; chmod 555 "$RO"
  SCRIPT="$(cli_with_default_dir "$RO")"
  rc=0; no_tty env PATH="$FAKE_HOME/.local/bin:$STUB:$BIN:$OLD:$BASE_PATH" HOME="$FAKE_HOME" bash "$SCRIPT" </dev/null >"$OUT" 2>&1 || rc=$?
  chmod 755 "$RO"
  check "shadow.first_on_path.exit" "0" "$rc"
  contains "shadow.first_on_path.success" "$(cat "$OUT")" "Installed successfully!"
  contains "shadow.first_on_path.version" "$(cat "$OUT")" "wendy version 2026.07.19-143000"
  absent "shadow.first_on_path.no_warning" "$(cat "$OUT")" "Warning:"
fi

# --- Test U: the -d help says when the ~/.local/bin fallback applies ---
help="$(bash "$CLI" -h)"
contains "usage.fallback_when" "$help" "sudo can't be used (not installed,"
contains "usage.explicit_dir" "$help" "is never relocated."

# sudo_that_works replaces the stub sudo with one that succeeds, as sudo does
# with NOPASSWD or a password typed at a terminal. It logs every call to
# SUDO_LOG and never runs anything; it drains piped input (`… | sudo tee`).
sudo_that_works() {
  cat > "$STUB/sudo" <<EOF
#!/usr/bin/env bash
echo "\$*" >> "$SUDO_LOG"
[ -t 0 ] || cat >/dev/null
exit 0
EOF
  chmod +x "$STUB/sudo"
}

# --- Test Q: no TTY, passwordless sudo: the package manager is still used ---
make_stubs Linux x86_64
sudo_that_works
printf '#!/usr/bin/env bash\nexit 0\n' > "$STUB/apt-get"; chmod +x "$STUB/apt-get"
D="$(mktemp -d)"; setup_net "$D"; echo "fake key" > "$D/repo-signing-key.gpg"
FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"
rc=0; run_no_tty "$OUT" "$CLI" || rc=$?
check "nopasswd.pkg.exit" "0" "$rc"
contains "nopasswd.pkg.apt" "$(cat "$OUT")" "APT detected"
contains "nopasswd.pkg.probed" "$(cat "$SUDO_LOG")" "-n true"
contains "nopasswd.pkg.installed" "$(cat "$SUDO_LOG")" "apt-get install -y wendy"
absent "nopasswd.pkg.no_standalone" "$(cat "$OUT")" "standalone"

# --- Test R: with a terminal, Linux: the package manager is used, sudo prompts itself ---
make_stubs Linux x86_64
sudo_that_works
printf '#!/usr/bin/env bash\nexit 0\n' > "$STUB/apt-get"; chmod +x "$STUB/apt-get"
D="$(mktemp -d)"; setup_net "$D"; echo "fake key" > "$D/repo-signing-key.gpg"
FAKE_HOME="$(mktemp -d)"
out="$({ printf 'y\n'; sleep 3; } | with_pty env PATH="$STUB:$BIN:$BASE_PATH" HOME="$FAKE_HOME" bash "$CLI" 2>&1 || true)"
contains "tty.pkg.prompted" "$out" "Proceed? [y/N]"
contains "tty.pkg.apt" "$out" "APT detected"
contains "tty.pkg.installed" "$(cat "$SUDO_LOG")" "apt-get install -y wendy"
absent "tty.pkg.no_nonint_probe" "$(cat "$SUDO_LOG")" "-n true"
absent "tty.pkg.no_standalone" "$out" "standalone"

# --- Test V: a package-manager install shadowed by an older `wendy` earlier on PATH ---
# The no-TTY fallback above leaves a standalone ~/.local/bin/wendy behind, and
# on Ubuntu ~/.local/bin precedes /usr/bin. A later brew/apt install used to
# print "Installed successfully!" with that OLD binary's version.
old_local_wendy() { # old_local_wendy HOME: an unmanaged old binary in HOME/.local/bin
  mkdir -p "$1/.local/bin"
  printf '#!/bin/sh\necho "wendy version 2026.01.01-OLD"\n' > "$1/.local/bin/wendy"; chmod +x "$1/.local/bin/wendy"
}

# Homebrew: `brew --prefix`/bin/wendy is what was installed.
make_stubs Darwin arm64
PFX="$(mktemp -d)"
cat > "$STUB/brew" <<EOF
#!/usr/bin/env bash
case "\$1" in
  help) exit 1;;
  --prefix) echo "$PFX";;
  install) mkdir -p "$PFX/bin"; printf '#!/bin/sh\necho "wendy version NEW-brew"\n' > "$PFX/bin/wendy"; chmod +x "$PFX/bin/wendy";;
esac
exit 0
EOF
chmod +x "$STUB/brew"
D="$(mktemp -d)"; setup_net "$D"; FAKE_HOME="$(mktemp -d)"; old_local_wendy "$FAKE_HOME"; OUT="$(mktemp)"
rc=0; no_tty env PATH="$FAKE_HOME/.local/bin:$PFX/bin:$STUB:$BIN:$BASE_PATH" HOME="$FAKE_HOME" bash "$CLI" -y </dev/null >"$OUT" 2>&1 || rc=$?
check "pkgshadow.brew.exit" "0" "$rc"
absent "pkgshadow.brew.no_false_success" "$(cat "$OUT")" "Installed successfully!"
contains "pkgshadow.brew.new_version" "$(cat "$OUT")" "wendy version NEW-brew"
absent "pkgshadow.brew.no_old_version" "$(cat "$OUT")" "2026.01.01-OLD"
contains "pkgshadow.brew.names_old" "$(cat "$OUT")" "on your PATH is $FAKE_HOME/.local/bin/wendy"
contains "pkgshadow.brew.rm_hint" "$(cat "$OUT")" "rm $FAKE_HOME/.local/bin/wendy"

# Same brew install, nothing shadowing it: a plain success with its version.
D="$(mktemp -d)"; setup_net "$D"; FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"
rc=0; no_tty env PATH="$PFX/bin:$STUB:$BIN:$BASE_PATH" HOME="$FAKE_HOME" bash "$CLI" -y </dev/null >"$OUT" 2>&1 || rc=$?
check "pkgshadow.brew_clean.exit" "0" "$rc"
contains "pkgshadow.brew_clean.success" "$(cat "$OUT")" "Installed successfully!"
contains "pkgshadow.brew_clean.version" "$(cat "$OUT")" "wendy version NEW-brew"

# APT (passwordless sudo): `dpkg -L wendy` says where the package put it.
make_stubs Linux x86_64
sudo_that_works
printf '#!/usr/bin/env bash\nexit 0\n' > "$STUB/apt-get"; chmod +x "$STUB/apt-get"
PKGBIN="$(mktemp -d)/usr/bin"; mkdir -p "$PKGBIN"
printf '#!/bin/sh\necho "wendy version NEW-apt"\n' > "$PKGBIN/wendy"; chmod +x "$PKGBIN/wendy"
printf '#!/bin/sh\n[ "$1" = "-L" ] && [ "$2" = "wendy" ] && printf "/.\\n%s\\n%s/wendy\\n" "%s" "%s"\n' "$PKGBIN" "$PKGBIN" "$PKGBIN" "$PKGBIN" > "$STUB/dpkg"; chmod +x "$STUB/dpkg"
D="$(mktemp -d)"; setup_net "$D"; echo "fake key" > "$D/repo-signing-key.gpg"
FAKE_HOME="$(mktemp -d)"; old_local_wendy "$FAKE_HOME"; OUT="$(mktemp)"
rc=0; no_tty env PATH="$FAKE_HOME/.local/bin:$PKGBIN:$STUB:$BIN:$BASE_PATH" HOME="$FAKE_HOME" bash "$CLI" </dev/null >"$OUT" 2>&1 || rc=$?
check "pkgshadow.apt.exit" "0" "$rc"
absent "pkgshadow.apt.no_false_success" "$(cat "$OUT")" "Installed successfully!"
contains "pkgshadow.apt.new_version" "$(cat "$OUT")" "wendy version NEW-apt"
absent "pkgshadow.apt.no_old_version" "$(cat "$OUT")" "2026.01.01-OLD"
contains "pkgshadow.apt.rm_hint" "$(cat "$OUT")" "rm $FAKE_HOME/.local/bin/wendy"

# --- Test W: piped into dash, as `curl … | sh` runs on Debian/Ubuntu ---
# Read from a pipe the script can't re-exec itself under bash ($0 is just the
# shell's name), which used to fail with "cannot execute binary file" (exit
# 126). It must say to pipe it into bash instead. From a file it still re-execs.
if command -v dash >/dev/null 2>&1; then
  for script in "$CLI" "$AGENT"; do
    name="$(basename "$script")"
    OUT="$(mktemp)"
    rc=0; (cd "$(mktemp -d)" && no_tty env PATH="$BASE_PATH" dash -s -- -y <"$script" >"$OUT" 2>&1) || rc=$?
    check "dash_pipe.${name}.exit" "1" "$rc"
    contains "dash_pipe.${name}.says_bash" "$(cat "$OUT")" "curl -fsSL https://install.wendy.dev/${name} | bash"
    check "dash_pipe.${name}.one_line" "1" "$(wc -l < "$OUT" | tr -d ' ')"
  done

  make_stubs Linux x86_64
  D="$(mktemp -d)"; setup_net "$D"; serve_cli_release "$D" linux amd64
  FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; DEST="$(mktemp -d)"
  rc=0; SH=dash run_no_tty "$OUT" "$CLI" -y -d "$DEST" || rc=$?
  check "dash_file.exit" "0" "$rc"
  check "dash_file.binary" "yes" "$([ -x "$DEST/wendy" ] && echo yes || echo no)"
fi

# --- Test X: how the shadow report identifies the older binary ---
make_shadow_run() { # make_shadow_run: a standalone install into DEST with $1 prepended to PATH
  make_stubs Linux x86_64
  D="$(mktemp -d)"; setup_net "$D"; serve_cli_release "$D" linux amd64
  FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; DEST="$(mktemp -d)"
}
# A relative PATH entry: the report names the absolute path, and dpkg-query is
# not asked about "bin/wendy" (for a relative argument it does a substring
# search, which matches the apt package's /usr/bin/wendy).
make_shadow_run
W="$(mktemp -d)"; mkdir -p "$W/bin"
printf '#!/bin/sh\necho "wendy version 2026.01.01-OLD"\n' > "$W/bin/wendy"; chmod +x "$W/bin/wendy"
printf '#!/bin/sh\ncase "$2" in /*) [ "$2" = /usr/bin/wendy ];; *bin/wendy*) true;; *) false;; esac && echo "wendy: /usr/bin/wendy"\n' > "$STUB/dpkg-query"; chmod +x "$STUB/dpkg-query"
rc=0; (cd "$W" && no_tty env PATH="bin:$STUB:$BIN:$BASE_PATH" HOME="$FAKE_HOME" bash "$CLI" -y -d "$DEST" </dev/null >"$OUT" 2>&1) || rc=$?
W_ABS="$(cd "$W" && pwd)"
check "shadow_rel.exit" "0" "$rc"
contains "shadow_rel.absolute" "$(cat "$OUT")" "on your PATH is $W_ABS/bin/wendy"
contains "shadow_rel.rm_hint" "$(cat "$OUT")" "rm $W_ABS/bin/wendy"
absent "shadow_rel.no_false_package" "$(cat "$OUT")" "apt-get"

# A Homebrew-installed older wendy: upgrade it through brew, by formula name.
make_shadow_run
HB="$(mktemp -d)"; mkdir -p "$HB/Cellar/wendy/2026.01.01/bin" "$HB/bin"
printf '#!/bin/sh\necho "wendy version 2026.01.01-OLD"\n' > "$HB/Cellar/wendy/2026.01.01/bin/wendy"; chmod +x "$HB/Cellar/wendy/2026.01.01/bin/wendy"
ln -s ../Cellar/wendy/2026.01.01/bin/wendy "$HB/bin/wendy"
rc=0; no_tty env PATH="$HB/bin:$STUB:$BIN:$BASE_PATH" HOME="$FAKE_HOME" bash "$CLI" -y -d "$DEST" </dev/null >"$OUT" 2>&1 || rc=$?
check "shadow_brew.exit" "0" "$rc"
contains "shadow_brew.upgrade_hint" "$(cat "$OUT")" "brew upgrade wendy"

# --- Test Y: the summary's `wendy --version` can neither abort nor eat the script ---
# serve_cli_binary DIR OS ARCH BODY: like serve_cli_release, with BODY as the
# stub wendy's script.
serve_cli_binary() {
  local dir="$1" os="$2" arch="$3" body="$4" v="2026.07.19-143000" pkg
  printf '{"latest":"%s"}\n' "$v" > "$dir/manifest.json"
  pkg="$(mktemp -d)"; mkdir -p "$pkg/wendy-cli-${os}-${arch}"
  printf '#!/bin/sh\n%s\n' "$body" > "$pkg/wendy-cli-${os}-${arch}/wendy"; chmod +x "$pkg/wendy-cli-${os}-${arch}/wendy"
  tar -czf "$dir/wendy-cli-${os}-${arch}-${v}.tar.gz" -C "$pkg" "wendy-cli-${os}-${arch}"
}
# A binary that can't run here (noexec mount, wrong architecture): warn, and
# still print the PATH line instead of dying under set -e.
make_stubs Linux x86_64
D="$(mktemp -d)"; setup_net "$D"; serve_cli_binary "$D" linux amd64 'echo "exec format error" >&2; exit 126'
FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; DEST="$(mktemp -d)"
rc=0; run_no_tty "$OUT" "$CLI" -y -d "$DEST" || rc=$?
check "version_fails.exit" "0" "$rc"
contains "version_fails.warns" "$(cat "$OUT")" "Warning: '$DEST/wendy --version' failed"
contains "version_fails.path_line" "$(cat "$OUT")" "export PATH=\"$DEST:\$PATH\""
# Under `curl … | bash` stdin is the rest of the script: `wendy --version`
# must not be able to read it.
make_stubs Linux x86_64
D="$(mktemp -d)"; setup_net "$D"
serve_cli_binary "$D" linux amd64 'if read -r line; then echo "stdin had: $line"; fi; echo "wendy version 2026.07.19-143000"'
FAKE_HOME="$(mktemp -d)"; OUT="$(mktemp)"; DEST="$(mktemp -d)"
rc=0; no_tty env PATH="$DEST:$STUB:$BIN:$BASE_PATH" HOME="$FAKE_HOME" bash -s -- -y -d "$DEST" <"$CLI" >"$OUT" 2>&1 || rc=$?
check "version_stdin.exit" "0" "$rc"
contains "version_stdin.version" "$(cat "$OUT")" "wendy version 2026.07.19-143000"
absent "version_stdin.no_script_read" "$(cat "$OUT")" "stdin had"

exit $fail
