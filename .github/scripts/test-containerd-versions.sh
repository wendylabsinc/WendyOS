#!/usr/bin/env bash
# Compile once in the workflow, then exercise both extracted runtime versions.
# Each version gets a fresh daemon and fresh DNS namespaces. A failed version
# does not suppress the other version's diagnostics, and either failure fails CI.
set -euo pipefail
: "${RUNNER_TEMP:?RUNNER_TEMP must name the disposable CI workspace}"
if [[ $# == 0 ]]; then
  result=0
  for runtime_version in 1.7 current; do
    echo "::group::Containerd $runtime_version lifecycle, DNS, DDS and memory"
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

# Open-only private character nodes exercise cgroup access without configuring a
# tunnel or attaching a loop device. These modules belong to the disposable VM.
sudo modprobe tun
sudo modprobe loop
sudo env PATH="$runtime_path" bash .github/scripts/test-containerd-regressions.sh "$RUNNER_TEMP/containerd.test"

for mutation in cdi-display-omitted cdi-refresh-omitted device-refresh-omitted; do
  test_name='^TestIntegrationDisplayCDIMountsAndRestart$'
  if [[ "$mutation" == device-refresh-omitted ]]; then test_name='^TestIntegrationNPUDeviceNumberRestart$'; fi
  if sudo env PATH="$runtime_path" bash .github/scripts/test-containerd-regressions.sh \
    "$RUNNER_TEMP/$mutation.test" "$test_name" 2>&1 | tee "$logs/$mutation.log"; then
    echo "CDI mutation unexpectedly passed: $mutation" >&2
    exit 1
  fi
  if [[ "$mutation" == cdi-display-omitted ]]; then
    grep -q 'display CDI mounts missing or duplicated' "$logs/$mutation.log"
  elif [[ "$mutation" == device-refresh-omitted ]]; then
    grep -q 'device number refresh did not reach runtime' "$logs/$mutation.log"
  else
    grep -q 'starting CDI fixture:.*libfixture.so.1' "$logs/$mutation.log"
  fi
done

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

sudo env PATH="$runtime_path" GITHUB_ACTIONS="$GITHUB_ACTIONS" RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT" \
  bash .github/scripts/test-memory-budget.sh "$RUNNER_TEMP/containerd.test" "$RUNNER_TEMP/memory-workload" | tee "$logs/memory-positive.log"
grep -q '^--- PASS: TestContainerCombinedMemoryBudget ' "$logs/memory-positive.log"
if sudo env PATH="$runtime_path" GITHUB_ACTIONS="$GITHUB_ACTIONS" RUNNER_ENVIRONMENT="$RUNNER_ENVIRONMENT" \
  bash .github/scripts/test-memory-budget.sh "$RUNNER_TEMP/memory-uncapped.test" "$RUNNER_TEMP/memory-workload" 2>&1 | tee "$logs/memory-negative.log"; then
  echo 'Uncapped swap mutation unexpectedly passed' >&2
  exit 1
fi
grep -q 'combined memory budget was exceeded without OOM enforcement' "$logs/memory-negative.log"
