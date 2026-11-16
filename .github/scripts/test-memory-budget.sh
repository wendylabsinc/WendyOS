#!/usr/bin/env bash
# This fixture adds only its own bounded swap file on an ephemeral hosted VM.
# Never disable existing swap or alter host overcommit/swappiness policy.
set -euo pipefail
[[ $EUID == 0 && ${GITHUB_ACTIONS:-} == true && ${RUNNER_ENVIRONMENT:-} == github-hosted ]] || {
  echo 'Memory-budget integration requires a disposable GitHub-hosted Linux runner.' >&2
  exit 2
}
[[ $# == 2 ]]
binary=$(realpath "$1")
export WENDY_MEMORY_BUDGET_WORKLOAD
WENDY_MEMORY_BUDGET_WORKLOAD=$(realpath "$2")
[[ -x $binary && -x $WENDY_MEMORY_BUDGET_WORKLOAD ]]
[[ -f /sys/fs/cgroup/cgroup.controllers ]]
grep -qw memory /sys/fs/cgroup/cgroup.controllers
work=$(mktemp -d /tmp/wendy-memory-budget.XXXXXX)
swap="$work/swap"
active=false
cleanup() {
  local status=$?
  trap - EXIT
  if [[ $active == true ]]; then
    # Do not unlink a swap file that the kernel still uses if teardown fails.
    if ! swapoff "$swap"; then exit 1; fi
  fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
fallocate -l 256M "$swap"
chmod 600 "$swap"
mkswap "$swap"
swapon "$swap"
active=true
bash .github/scripts/test-containerd-regressions.sh "$binary" '^TestContainerCombinedMemoryBudget$'
