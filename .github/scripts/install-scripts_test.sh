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
  *install.wendy.sh/manifest.json) src="$SERVE_DIR/manifest.json";;
  *api.github.com/*) src="$SERVE_DIR/github.json";;
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
contains "resolve.gcs.hit_manifest" "$(cat "$REQ_LOG")" "install.wendy.sh/manifest.json"

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
absent "defer.no_manifest" "$(cat "$REQ_LOG")" "install.wendy.sh/manifest.json"

exit $fail
