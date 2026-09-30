#!/bin/sh
# Tests for the Wendy plugin's CLI launcher (plugins/wendy/scripts/wendy) and
# scripts/pin-cli.sh. POSIX sh; needs python3, tar, curl or wget, and shasum,
# sha256sum or openssl.
#
#   sh scripts/test/plugin-launcher/run.sh              # every case
#   sh scripts/test/plugin-launcher/run.sh test_name…   # some cases
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../../.." && pwd)
LAUNCHER_SRC="$REPO/plugins/wendy/scripts/wendy"
PIN="$REPO/scripts/pin-cli.sh"
PASSES=0
FAILS=0
SERVER_PID=""

# --- helpers ------------------------------------------------------------------

fail_case() {
  echo "    $*"
  CASE_FAILED=1
}

assert_eq() {
  [ "$1" = "$2" ] || fail_case "$3: expected [$2], got [$1]"
}

assert_contains() {
  case "$1" in
    *"$2"*) ;;
    *) fail_case "$3: [$1] does not contain [$2]" ;;
  esac
}

host_platform() {
  case "$(uname -s)" in Darwin) o=darwin ;; Linux) o=linux ;; *) o=other ;; esac
  case "$(uname -m)" in x86_64 | amd64) a=amd64 ;; arm64 | aarch64) a=arm64 ;; *) a=other ;; esac
  echo "${o}-$a"
}

# setup gives each case a private HOME, managed root, PATH and fixture tree.
# PATH holds $STUB (per-case shims) and $T/sysbin (links to the system tools
# the launcher needs), never the real /usr/local/bin or /opt/homebrew/bin.
setup() {
  T=$(mktemp -d)
  HOME="$T/home"
  WENDY_CONFIG_DIR="$HOME/.wendy"
  WENDY_LAUNCHER_WELL_KNOWN_DIRS="$T/wellknown"
  export HOME WENDY_CONFIG_DIR WENDY_LAUNCHER_WELL_KNOWN_DIRS
  unset WENDY_CLI WENDY_CLI_DOWNLOAD_BASE FIXTURE_SLOW_SECONDS
  mkdir -p "$HOME" "$T/wellknown" "$T/sysbin"
  STUB="$T/stubbin"
  mkdir -p "$STUB"
  for tool in sh awk cat chmod cmp cp curl cut date dirname expr find grep gzip head \
    kill ln ls mkdir mktemp mv openssl pkill python3 readlink rm sed sha256sum \
    shasum sleep sysctl tar touch tr uname unzip wget; do
    p=$(PATH="$ORIG_PATH" command -v "$tool" 2>/dev/null) && ln -s "$p" "$T/sysbin/$tool"
  done
  PATH="$STUB:$T/sysbin"
  export PATH
  FIX="$T/fixtures"
  mkdir -p "$FIX"
  L="$T/plugin/scripts/wendy"
  mkdir -p "$T/plugin/scripts"
  cp "$LAUNCHER_SRC" "$L"
  chmod 755 "$L"
}

teardown() {
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null
    wait "$SERVER_PID" 2>/dev/null
  fi
  SERVER_PID=""
  PATH=$ORIG_PATH
  export PATH
  rm -rf "$T"
}

# make_stub_cli writes a fake wendy that reports VERSION and echoes its args
# (each in brackets) and the first line of its stdin.
make_stub_cli() {
  mkdir -p "$(dirname "$1")"
  cat >"$1" <<EOF
#!/bin/sh
if [ "\${1:-}" = "--version" ]; then echo "wendy version $2"; exit 0; fi
printf 'stub-wendy %s args:' "$2"
for a in "\$@"; do printf '[%s]' "\$a"; done
echo
if [ -n "\${STUB_PRINT_ENV:-}" ]; then echo "env:OS=\${OS:-} ARCH=\${ARCH:-} CLI=\${CLI:-}"; fi
if [ -n "\${STUB_READ_STDIN:-}" ]; then IFS= read -r line; echo "stdin:\$line"; fi
EOF
  chmod 755 "$1"
}

# make_release builds VERSION's assets in $FIX/VERSION: real stub tarballs for
# every POSIX platform, placeholder files for Windows (only hashed by pin).
make_release() {
  d="$FIX/$1"
  mkdir -p "$d"
  for p in darwin-arm64 linux-amd64 linux-arm64; do
    pkg="$T/pkg/wendy-cli-$p"
    make_stub_cli "$pkg/wendy" "$1"
    echo notice >"$pkg/NOTICE"
    tar -czf "$d/wendy-cli-$p-$1.tar.gz" -C "$T/pkg" "wendy-cli-$p"
    rm -rf "$T/pkg"
  done
  for p in windows-amd64 windows-arm64; do
    echo "placeholder $p" >"$d/wendy-cli-$p-$1.zip"
  done
}

# pin writes VERSION (with MIN) and the fixture hashes into the launcher copy.
pin() {
  sh "$PIN" --launcher "$L" --local "$FIX/$1" --min "$2" "$1" >/dev/null ||
    fail_case "pin-cli.sh failed for $1"
}

# serve starts the fixture server and points the launcher at it.
serve() {
  rm -f "$T/port"
  python3 "$HERE/fixture_server.py" "$FIX" "$T/port" &
  SERVER_PID=$!
  i=0
  while [ ! -s "$T/port" ] && [ "$i" -lt 100 ]; do
    sleep 0.1
    i=$((i + 1))
  done
  WENDY_CLI_DOWNLOAD_BASE="http://127.0.0.1:$(cat "$T/port")"
  export WENDY_CLI_DOWNLOAD_BASE
}

# no_network points downloads at a closed port, so a case fails if it downloads.
no_network() {
  WENDY_CLI_DOWNLOAD_BASE="http://127.0.0.1:9"
  export WENDY_CLI_DOWNLOAD_BASE
}

requests_for() {
  if [ -f "$FIX/requests.log" ]; then grep -c "$1" "$FIX/requests.log"; else echo 0; fi
}

# --- cases: resolution ---------------------------------------------------------

test_wendy_cli_env_runs_it_as_is() {
  make_stub_cli "$T/custom/wendy" "2000.01.01-000000"
  no_network
  out=$(WENDY_CLI="$T/custom/wendy" sh "$L" mcp serve 2>"$T/err")
  assert_eq "$out" "stub-wendy 2000.01.01-000000 args:[mcp][serve]" "stdout is only the CLI's"
  [ ! -e "$WENDY_CONFIG_DIR" ] || fail_case "WENDY_CLI must not create the managed root"
}

test_wendy_cli_env_not_executable_fails() {
  out=$(WENDY_CLI="$T/missing/wendy" sh "$L" mcp serve 2>"$T/err")
  rc=$?
  assert_eq "$rc" "1" "exit code"
  assert_eq "$out" "" "stdout stays empty"
  assert_contains "$(cat "$WENDY_CONFIG_DIR/cli/last-error.txt")" "WENDY_CLI is set to" "error file"
}

test_path_wendy_new_enough_is_used() {
  make_stub_cli "$STUB/wendy" "2026.10.01-000000"
  no_network
  out=$(sh "$L" run --name "a b" 2>"$T/err")
  assert_eq "$out" "stub-wendy 2026.10.01-000000 args:[run][--name][a b]" "PATH wendy runs with args intact"
}

test_path_wendy_equal_to_min_is_used() {
  v=$(sed -n 's/^MIN_VERSION="\(.*\)"$/\1/p' "$L")
  make_stub_cli "$STUB/wendy" "$v"
  no_network
  out=$(sh "$L" --help 2>"$T/err")
  assert_eq "$out" "stub-wendy $v args:[--help]" "a CLI at exactly MIN_VERSION is accepted"
}

test_dev_build_on_path_is_used_with_notice() {
  make_stub_cli "$STUB/wendy" "2026.06.30-133859-dev"
  no_network
  out=$(sh "$L" mcp serve 2>"$T/err")
  assert_eq "$out" "stub-wendy 2026.06.30-133859-dev args:[mcp][serve]" "dev build runs"
  assert_contains "$(cat "$T/err")" "development build" "notice on stderr"
}

test_well_known_dir_is_searched() {
  make_stub_cli "$T/wellknown/wendy" "2026.10.01-000000"
  no_network
  out=$(sh "$L" mcp serve 2>"$T/err")
  assert_eq "$out" "stub-wendy 2026.10.01-000000 args:[mcp][serve]" "well-known dir wendy runs"
}

test_launcher_named_wendy_on_path_is_skipped() {
  ln -s "$L" "$STUB/wendy"
  make_stub_cli "$T/wellknown/wendy" "2026.10.01-000000"
  no_network
  sh "$L" mcp serve >"$T/out" 2>"$T/err" &
  pid=$!
  i=0
  while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid"
    fail_case "launcher recursed into itself"
  fi
  assert_eq "$(cat "$T/out")" "stub-wendy 2026.10.01-000000 args:[mcp][serve]" "falls through to the real CLI"
}

test_stdin_reaches_the_cli() {
  make_stub_cli "$STUB/wendy" "2026.10.01-000000"
  no_network
  out=$(printf '{"jsonrpc":"2.0","id":1,"method":"initialize"}\n' | STUB_READ_STDIN=1 sh "$L" mcp serve 2>"$T/err")
  assert_contains "$out" 'stdin:{"jsonrpc":"2.0","id":1,"method":"initialize"}' "first stdin line reaches the CLI"
}

test_success_clears_a_stale_error() {
  mkdir -p "$WENDY_CONFIG_DIR/cli"
  echo "error: old failure" >"$WENDY_CONFIG_DIR/cli/last-error.txt"
  make_stub_cli "$STUB/wendy" "2026.10.01-000000"
  no_network
  sh "$L" mcp serve >/dev/null 2>&1
  [ ! -e "$WENDY_CONFIG_DIR/cli/last-error.txt" ] || fail_case "stale last-error.txt was kept"
}

# --- cases: managed install -----------------------------------------------------

V=2026.10.01-000000

# host_asset is the fixture asset the launcher downloads on this machine.
host_asset() {
  echo "wendy-cli-$(host_platform)-$V.tar.gz"
}

test_fresh_install_verifies_links_and_runs() {
  make_release "$V"
  pin "$V" "$V"
  serve
  out=$(sh "$L" mcp serve 2>"$T/err")
  assert_eq "$?" "0" "exit code"
  assert_eq "$out" "stub-wendy $V args:[mcp][serve]" "stdout is only the managed CLI's"
  [ -x "$WENDY_CONFIG_DIR/cli/$V/wendy" ] || fail_case "managed CLI missing"
  assert_eq "$(readlink "$WENDY_CONFIG_DIR/bin/wendy")" "$WENDY_CONFIG_DIR/cli/$V/wendy" "bin/wendy link"
  assert_contains "$(cat "$T/err")" "installing the Wendy CLI $V" "progress on stderr"
  [ ! -e "$WENDY_CONFIG_DIR/cli/last-error.txt" ] || fail_case "no error file after success"
}

test_second_run_needs_no_network() {
  make_release "$V"
  pin "$V" "$V"
  serve
  sh "$L" --version >/dev/null 2>&1
  no_network
  out=$(sh "$L" mcp serve 2>/dev/null)
  assert_eq "$out" "stub-wendy $V args:[mcp][serve]" "managed CLI reused"
  assert_eq "$(requests_for "$(host_asset)")" "1" "downloaded once"
}

test_old_path_wendy_falls_back_to_managed() {
  make_release "$V"
  pin "$V" "$V"
  make_stub_cli "$STUB/wendy" "2026.01.01-000000"
  serve
  out=$(sh "$L" mcp serve 2>"$T/err")
  assert_eq "$out" "stub-wendy $V args:[mcp][serve]" "managed CLI used"
  assert_contains "$(cat "$T/err")" "is version 2026.01.01-000000, older than $V" "notice names both versions"
}

test_unrecognised_path_wendy_falls_back_quietly() {
  make_release "$V"
  pin "$V" "$V"
  printf '#!/bin/sh\nexit 3\n' >"$STUB/wendy"
  chmod 755 "$STUB/wendy"
  serve
  out=$(sh "$L" mcp serve 2>"$T/err")
  assert_eq "$out" "stub-wendy $V args:[mcp][serve]" "managed CLI used"
  case "$(cat "$T/err")" in *"older than"*) fail_case "no age notice for an unreadable version" ;; esac
}

test_checksum_mismatch_installs_nothing() {
  make_release "$V"
  pin "$V" "$V"
  echo tampered >>"$FIX/$V/$(host_asset)"
  serve
  out=$(sh "$L" mcp serve 2>"$T/err")
  assert_eq "$?" "1" "exit code"
  assert_eq "$out" "" "stdout stays empty"
  assert_contains "$(cat "$WENDY_CONFIG_DIR/cli/last-error.txt")" "checksum mismatch" "error file"
  assert_contains "$(cat "$T/err")" "checksum mismatch" "stderr"
  [ ! -e "$WENDY_CONFIG_DIR/cli/$V" ] || fail_case "nothing installed"
  [ -z "$(find "$WENDY_CONFIG_DIR/cli" -name '.tmp.*')" ] || fail_case "temp dir removed"
}

test_download_failure_names_the_next_step() {
  make_release "$V"
  pin "$V" "$V"
  rm "$FIX/$V/$(host_asset)"
  serve
  sh "$L" mcp serve >/dev/null 2>"$T/err"
  assert_eq "$?" "1" "exit code"
  err=$(cat "$WENDY_CONFIG_DIR/cli/last-error.txt")
  assert_contains "$err" "could not download" "error"
  assert_contains "$err" "next step: check the network connection" "next step"
}

test_environment_reaches_the_cli_unchanged() {
  make_release "$V"
  pin "$V" "$V"
  serve
  out=$(OS=Windows_NT ARCH=sparc CLI=mine STUB_PRINT_ENV=1 sh "$L" mcp serve 2>"$T/err")
  assert_contains "$out" "env:OS=Windows_NT ARCH=sparc CLI=mine" "the CLI sees the caller's environment unchanged"
}

test_install_that_never_ran_leaves_a_fresh_error() {
  make_release "$V"
  pin "$V" "$V"
  mkdir -p "$WENDY_CONFIG_DIR/cli/install.log"
  echo "error: old failure" >"$WENDY_CONFIG_DIR/cli/last-error.txt"
  serve
  sh "$L" mcp serve >/dev/null 2>"$T/err"
  assert_eq "$?" "1" "exit code"
  err=$(cat "$WENDY_CONFIG_DIR/cli/last-error.txt")
  case "$err" in *"old failure"*) fail_case "stale error reported as the current failure" ;; esac
  assert_contains "$err" "did not finish" "fresh error file"
}

fake_platform() {
  # shellcheck disable=SC2016 # the $1 belongs to the generated uname script
  printf '#!/bin/sh\ncase "$1" in -s) echo %s ;; -m) echo %s ;; *) echo "%s %s" ;; esac\n' "$1" "$2" "$1" "$2" >"$STUB/uname"
  printf '#!/bin/sh\necho %s\n' "$3" >"$STUB/sysctl"
  chmod 755 "$STUB/uname" "$STUB/sysctl"
}

test_intel_mac_is_unsupported() {
  make_release "$V"
  pin "$V" "$V"
  fake_platform Darwin x86_64 0
  out=$(sh "$L" mcp serve 2>"$T/err")
  assert_eq "$?" "1" "exit code"
  assert_eq "$out" "" "stdout stays empty"
  assert_contains "$(cat "$WENDY_CONFIG_DIR/cli/last-error.txt")" "Intel Macs" "error file"
}

test_rosetta_shell_installs_arm64() {
  make_release "$V"
  pin "$V" "$V"
  fake_platform Darwin x86_64 1
  serve
  out=$(sh "$L" mcp serve 2>"$T/err")
  assert_eq "$out" "stub-wendy $V args:[mcp][serve]" "arm64 build runs"
  assert_eq "$(requests_for "wendy-cli-darwin-arm64-$V.tar.gz")" "1" "fetched darwin-arm64"
}

test_other_os_is_unsupported() {
  make_release "$V"
  pin "$V" "$V"
  fake_platform SunOS sparc 0
  sh "$L" mcp serve >/dev/null 2>"$T/err"
  assert_eq "$?" "1" "exit code"
  assert_contains "$(cat "$WENDY_CONFIG_DIR/cli/last-error.txt")" "no build for SunOS/sparc" "error file"
}

test_new_root_is_private() {
  make_release "$V"
  pin "$V" "$V"
  serve
  sh "$L" --version >/dev/null 2>&1
  [ -n "$(find "$WENDY_CONFIG_DIR" -maxdepth 0 -perm 700)" ] || fail_case "root mode is not 0700"
}

# --- cases: pin-cli.sh ---------------------------------------------------------

sha_of() {
  if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{ print $1 }'; else sha256sum "$1" | awk '{ print $1 }'; fi
}

test_pin_rewrites_only_the_block() {
  make_release 2026.10.01-000000
  sed '/^# >>> pinned/,/^# <<< pinned$/d' "$L" >"$T/before"
  pin 2026.10.01-000000 2026.09.30-000000
  sed '/^# >>> pinned/,/^# <<< pinned$/d' "$L" >"$T/after"
  cmp -s "$T/before" "$T/after" || fail_case "lines outside the pinned block changed"
  assert_contains "$(cat "$L")" 'CLI_VERSION="2026.10.01-000000"' "version"
  assert_contains "$(cat "$L")" 'MIN_VERSION="2026.09.30-000000"' "min"
  want=$(sha_of "$FIX/2026.10.01-000000/wendy-cli-linux-amd64-2026.10.01-000000.tar.gz")
  assert_contains "$(cat "$L")" "SHA256_linux_amd64=\"$want\"" "linux-amd64 hash"
  [ -x "$L" ] || fail_case "pin must keep the launcher executable"
}

test_pin_keeps_min_by_default() {
  make_release 2026.10.01-000000
  make_release 2026.11.01-000000
  pin 2026.10.01-000000 2026.09.30-000000
  sh "$PIN" --launcher "$L" --local "$FIX/2026.11.01-000000" 2026.11.01-000000 >/dev/null
  assert_contains "$(cat "$L")" 'MIN_VERSION="2026.09.30-000000"' "min kept"
  assert_contains "$(cat "$L")" 'CLI_VERSION="2026.11.01-000000"' "version moved"
}

test_pin_rejects_min_after_tag() {
  make_release 2026.10.01-000000
  cp "$L" "$T/orig"
  sh "$PIN" --launcher "$L" --local "$FIX/2026.10.01-000000" --min 2026.12.01-000000 2026.10.01-000000 >/dev/null 2>&1
  assert_eq "$?" "2" "exit code"
  cmp -s "$T/orig" "$L" || fail_case "launcher changed on a rejected pin"
}

test_pin_fails_when_an_asset_is_missing() {
  make_release 2026.10.01-000000
  rm "$FIX/2026.10.01-000000/wendy-cli-linux-arm64-2026.10.01-000000.tar.gz"
  cp "$L" "$T/orig"
  sh "$PIN" --launcher "$L" --local "$FIX/2026.10.01-000000" 2026.10.01-000000 >/dev/null 2>&1
  assert_eq "$?" "1" "exit code"
  cmp -s "$T/orig" "$L" || fail_case "launcher changed on a failed pin"
}

test_pin_rejects_a_non_release_tag() {
  sh "$PIN" --launcher "$L" --local "$FIX" v1.2.3 >/dev/null 2>&1
  assert_eq "$?" "2" "exit code"
}

# --- runner --------------------------------------------------------------------

ORIG_PATH=$PATH
run_case() {
  CASE_FAILED=0
  setup
  "$1"
  teardown
  if [ "$CASE_FAILED" = 0 ]; then
    PASSES=$((PASSES + 1))
    echo "ok   $1"
  else
    FAILS=$((FAILS + 1))
    echo "FAIL $1"
  fi
}

if [ $# -gt 0 ]; then
  cases=$*
else
  cases=$(sed -n 's/^\(test_[a-z0-9_]*\)() {$/\1/p' "$0")
fi
for c in $cases; do run_case "$c"; done
echo "$PASSES passed, $FAILS failed"
[ "$FAILS" = 0 ]
