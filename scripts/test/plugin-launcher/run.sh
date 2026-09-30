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
  for tool in sh awk cat chmod cmp cp curl cut date dirname expr find grep head \
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
