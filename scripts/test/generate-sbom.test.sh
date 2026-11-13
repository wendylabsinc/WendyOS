#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/../generate-sbom.sh"
export SYFT_BIN="$HERE/fake-syft.sh"
chmod +x "$HERE/fake-syft.sh" "$SCRIPT"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
fail=0
check() { if eval "$2"; then echo "ok - $1"; else echo "FAIL - $1"; fail=1; fi; }

# usage error when no subcommand
rc=0; "$SCRIPT" >/dev/null 2>&1 || rc=$?
check "no-args exits 2" '[ "$rc" -eq 2 ]'

# unknown subcommand -> 2
rc=0; "$SCRIPT" bogus >/dev/null 2>&1 || rc=$?
check "unknown subcommand exits 2" '[ "$rc" -eq 2 ]'

# binary: missing file -> 1
rc=0; "$SCRIPT" binary "$TMP/nope" "$TMP/out.spdx.json" >/dev/null 2>&1 || rc=$?
check "binary missing-file exits 1" '[ "$rc" -eq 1 ]'

# binary: happy path writes valid SPDX
touch "$TMP/wendy"; chmod +x "$TMP/wendy"
"$SCRIPT" binary "$TMP/wendy" "$TMP/bin.spdx.json" >/dev/null 2>&1
check "binary writes output" '[ -s "$TMP/bin.spdx.json" ]'
check "binary output is SPDX" 'grep -q spdxVersion "$TMP/bin.spdx.json"'

# scan failure propagates
rc=0; FAKE_SYFT_FAIL=1 "$SCRIPT" binary "$TMP/wendy" "$TMP/x.spdx.json" >/dev/null 2>&1 || rc=$?
check "syft failure exits 1" '[ "$rc" -eq 1 ]'

# --repo-root without a value is a usage error
rc=0; "$SCRIPT" source "$TMP/x.spdx.json" --repo-root >/dev/null 2>&1 || rc=$?
check "source --repo-root missing value exits 2" '[ "$rc" -eq 2 ]'

# Swift requires an actual resolution and rejects an empty source directory.
rc=0; "$SCRIPT" swift "$TMP/swift.spdx.json" --repo-root "$TMP" >/dev/null 2>&1 || rc=$?
check "swift without resolution fails" '[ "$rc" -ne 0 ]'
mkdir -p "$TMP/swift/WendyAgentCore" "$TMP/swift/WendyAgent.xcworkspace/xcshareddata/swiftpm"
cat > "$TMP/swift/WendyAgentCore/Package.swift" <<'EOF'
.package(url: "https://github.com/apple/swift-log.git", from: "1.6.3")
EOF
cat > "$TMP/swift/WendyAgent.xcworkspace/xcshareddata/swiftpm/Package.resolved" <<'EOF'
{"version":3,"pins":[{"identity":"swift-log","kind":"remoteSourceControl","location":"https://github.com/apple/swift-log.git","state":{"version":"1.6.3","revision":"1234567890123456789012345678901234567890"}}]}
EOF
mkdir -p "$TMP/swift/Build/Xcode/SourcePackages"
cat > "$TMP/swift/Build/Xcode/SourcePackages/workspace-state.json" <<'EOF'
{"object":{"dependencies":[{"packageRef":{"identity":"swift-log","kind":"remoteSourceControl"},"state":{"checkoutState":{"revision":"1234567890123456789012345678901234567890"}}}]}}
EOF
# swift + source write output
"$SCRIPT" swift "$TMP/swift.spdx.json" --repo-root "$TMP" >/dev/null 2>&1
check "swift writes output" '[ -s "$TMP/swift.spdx.json" ]'
"$SCRIPT" source "$TMP/src.spdx.json" --repo-root "$TMP" >/dev/null 2>&1
check "source writes output" '[ -s "$TMP/src.spdx.json" ]'

exit $fail
