#!/usr/bin/env bash
# Native resolver and cache visibility regression checks. Uses private mDNS
# service types and local test events; never enrolls or connects to a device.
set -euo pipefail
if [[ "$(uname -s)" != Darwin ]]; then
  echo 'Darwin discovery regression gate requires macOS.' >&2
  exit 2
fi
cd "$(git rev-parse --show-toplevel)"
output="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wendy-darwin-discovery.XXXXXX")"
# Select native Bonjour/interface and shared mDNS cases explicitly. The same
# package also contains an opt-in physical BLE scan, which is a separate gap.
go test -race ./go/internal/shared/discovery -run '^Test(BrowseMDNS|MDNS|PreferInterfaceRoutedAddr|ParseDarwinHardwarePorts)' -v -count=1 -timeout 120s | tee "$output/native.log"
for test_name in TestBrowseMDNSServicesContinuousStreamsLateArrival TestMDNSStreamBackend TestMDNSStreamResolveAndEmitSkipsFailedResolve TestMDNSStreamResolveAndEmitAllowsResolvedServiceWithoutTXT; do
  grep -q "^--- PASS: $test_name " "$output/native.log"
done
if grep -q -- '--- SKIP:' "$output/native.log"; then exit 1; fi
go test -race ./go/internal/cli/commands -run '^(TestDiscoverStream_CachedDevicesStayHiddenUntilConfirmed|TestLANPickerCachedDevicesStayHiddenUntilConfirmed)$' -v -count=1 -timeout 120s | tee "$output/cache.log"
for test_name in TestDiscoverStream_CachedDevicesStayHiddenUntilConfirmed TestLANPickerCachedDevicesStayHiddenUntilConfirmed; do
  grep -q "^--- PASS: $test_name " "$output/cache.log"
done
if grep -q -- '--- SKIP:' "$output/cache.log"; then exit 1; fi
python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys
out = Path(sys.argv[1])
source = Path('go/internal/shared/discovery/mdns_darwin.go').resolve()
original = source.read_text()
before = '\tsvc, err := resolveServiceFn(resolveCtx, inst, serviceType)\n\tif err != nil {\n\t\treturn\n\t}'
if original.count(before) != 1:
    raise SystemExit('native resolve control no longer identifies exactly one site')
after = '\tsvc, err := resolveServiceFn(resolveCtx, inst, serviceType)\n\tif err != nil {\n\t\tsvc = MDNSService{InstanceName: inst.instanceName, Hostname: inst.instanceName + ".local"}\n\t}'
replacement = out / 'ghost-resolve.go'
replacement.write_text(original.replace(before, after))
(out / 'ghost-resolve.json').write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY
if go test -race -overlay "$output/ghost-resolve.json" ./go/internal/shared/discovery -run '^TestMDNSStreamResolveAndEmitSkipsFailedResolve$' -v -count=1 -timeout 30s > "$output/ghost-resolve.log" 2>&1; then
  cat "$output/ghost-resolve.log"
  echo 'Native resolver accepted a fabricated live device after failed resolution.' >&2
  exit 1
fi
cat "$output/ghost-resolve.log"
grep -q '^--- FAIL: TestMDNSStreamResolveAndEmitSkipsFailedResolve ' "$output/ghost-resolve.log"
grep -q 'failed resolve emitted a ghost service' "$output/ghost-resolve.log"
echo "Darwin discovery evidence: $output"
