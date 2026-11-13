#!/usr/bin/env bash
# Audit 2318: actual CLI to production Swift startup, with disposable PKI.
set -euo pipefail
[[ $(uname -s) == Darwin && $# -ge 3 && $# -le 4 ]] || { echo 'usage: test-mac-enrollment.sh NEW_OUTPUT SWIFT_SCRATCH BUILT_CLI [default|native]' >&2; exit 2; }
mkdir "$1"
output=$(cd "$1" && pwd -P)
mkdir -p "$2"
scratch=$(cd "$2" && pwd -P)
cli=$(python3 -c 'import pathlib,sys; print(pathlib.Path(sys.argv[1]).resolve(strict=True))' "$3")
[[ -f $cli && -x $cli ]]
swift_build=(swift build)
swift_test=(swift test)
case ${4:-default} in
  default) ;;
  native) swift_build+=(--build-system native); swift_test+=(--build-system native) ;;
  *) echo 'unsupported Swift build system' >&2; exit 2 ;;
esac
root=$(pwd -P)
guardian="$root/.github/scripts/run-owned-media-test.py"
python3 "$guardian" --timeout 180 -- go build -o "$output/peer" ./go/internal/testfixture/macenrollment
# Reuse the existing core row's scratch and CLI; this is incremental, not a new
# dependency checkout/runner. Build/test must use the same selected build system.
python3 "$guardian" --timeout 600 -- "${swift_build[@]}" --package-path swift/WendyAgentCore --scratch-path "$scratch" --build-tests > "$output/build.log" 2>&1
python3 "$guardian" --timeout 140 -- env WENDY_ENROLLMENT_CLI="$cli" WENDY_ENROLLMENT_PEER="$output/peer" \
  "${swift_test[@]}" --package-path swift/WendyAgentCore --scratch-path "$scratch" --skip-build \
  --filter MacEnrollmentTransportTests 2>&1 | tee "$output/runtime.log"
python3 - "$output" "$cli" <<'PY'
import hashlib,json,pathlib,subprocess,sys
out=pathlib.Path(sys.argv[1]);cli=pathlib.Path(sys.argv[2]);text=(out/'runtime.log').read_text()
for marker in ('MAC_ENROLLMENT_VERIFIED ', 'MAC_ENROLLMENT_LEGACY_REFUSED ', 'MAC_ENROLLMENT_UNAVAILABLE_REFUSED ', 'Test run with 1 test in 1 suite passed'):
    if text.count(marker)!=1:raise ValueError('missing or duplicate enrollment proof: '+marker)
if 'skipped' in text.lower() or 'MAC_ENROLLMENT_CLEANUP_INCOMPLETE' in text:raise ValueError('skipped/incomplete enrollment runtime')
paths=['go/internal/testfixture/macenrollment/main.go','swift/WendyAgentCore/Sources/WendyAgent/WendyAgent.swift','swift/WendyAgentCore/Sources/WendyAgent/Services/ProvisioningService.swift','swift/WendyAgentCore/Sources/WendyAgent/Services/ProvisioningServiceV2.swift','swift/WendyAgentCore/Tests/WendyAgentTests/MacEnrollmentTransportTests.swift','.github/scripts/test-mac-enrollment.sh']
def hash_file(p):
    digest=hashlib.sha256()
    with pathlib.Path(p).open('rb') as stream:
        for chunk in iter(lambda:stream.read(1024*1024),b''):digest.update(chunk)
    return digest.hexdigest()
evidence={'entry':'2318','software_runtime':'passed','signed_package_execution':'not_run','production_pki':'not_run','source_commit':subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip(),'sources':{p:hash_file(p) for p in paths},'cli_sha256':hash_file(cli),'peer_sha256':hash_file(out/'peer'),'runtime_log_sha256':hash_file(out/'runtime.log'),'controls':['v1-only service refused before EAB','unavailable capability refused before EAB','restarted mTLS refuses absent client certificate']}
(out/'evidence.json').write_text(json.dumps(evidence,indent=2)+'\n')
PY
