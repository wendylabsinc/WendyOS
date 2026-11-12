#!/usr/bin/env bash
# Compile once in the workflow, then exercise both extracted runtime versions.
# Each version gets a fresh daemon and fresh DNS namespaces. A failed version
# does not suppress the other version's diagnostics, and either failure fails CI.
set -euo pipefail
: "${RUNNER_TEMP:?RUNNER_TEMP must name the disposable CI workspace}"
if [[ $# == 0 ]]; then
  result=0
  for runtime_version in 1.7 current; do
    echo "::group::Containerd $runtime_version lifecycle, DNS and DDS"
    if bash "$0" --version "$runtime_version"; then
      echo "Containerd $runtime_version: all required checks passed"
    else
      echo "Containerd $runtime_version: required checks failed" >&2
      result=1
    fi
    echo '::endgroup::'
  done
  exit "$result"
fi
[[ $# == 2 && $1 == --version ]]
runtime_version=$2
case "$runtime_version" in 1.7|current) ;; *) exit 2 ;; esac
runtime="$RUNNER_TEMP/wendy-runtime/$runtime_version/root"
common="$RUNNER_TEMP/wendy-runtime/common/root"
runtime_path="$runtime/usr/bin:$runtime/usr/sbin:$runtime/bin:$runtime/sbin:$common/usr/bin:$common/usr/sbin:$common/bin:$common/sbin:$PATH"
logs="$RUNNER_TEMP/containerd-$runtime_version"
mkdir -p "$logs"

sudo env PATH="$runtime_path" bash .github/scripts/test-containerd-regressions.sh "$RUNNER_TEMP/containerd.test"

tests='TestHostNetworkContainersAndBatteryShareTwoParticipants|TestIsolatedNamespaceUsesSeparateParticipant|TestAutomaticHostInterfaceResolvesBeforePoolLookup'
sudo "$RUNNER_TEMP/rtps.test" -test.run "^($tests)$" -test.v -test.count=1 -test.timeout=60s | tee "$logs/rtps.log"
for test in ${tests//|/ }; do
  grep -F -- "--- PASS: $test (" "$logs/rtps.log"
done

sudo env PATH="$runtime_path" bash .github/scripts/test-host-network-dns.sh "$RUNNER_TEMP/containerd.test" | tee "$logs/dns-positive.log"
grep -q '^--- PASS: TestHostNetworkDNSOutage ' "$logs/dns-positive.log"

if sudo env PATH="$runtime_path" bash .github/scripts/test-host-network-dns.sh "$RUNNER_TEMP/host-dns-pinned.test" 2>&1 | tee "$logs/dns-negative.log"; then
  echo 'Pinned resolver mutation unexpectedly passed' >&2
  exit 1
fi
grep -q 'container DNS did not recover for after-upstream-replacement' "$logs/dns-negative.log"
