#!/usr/bin/env bash
# Existing hosted containerd row only. No host daemon, device, or global config.
set -euo pipefail
if [[ ${1:-} != --inside ]]; then
  [[ $(uname -s) == Linux && ${GITHUB_ACTIONS:-} == true && ${RUNNER_ENVIRONMENT:-} == github-hosted && $EUID == 0 && $# == 3 ]] || {
    echo 'usage on disposable hosted Linux: sudo -E test-watch-container.sh CONTAINERD_TEST_BINARY PREPARED_DIR CASE' >&2; exit 2;
  }
  binary=$(realpath "$1")
  prepared=$(realpath "$2")
  case_name=$3
  [[ $case_name == normal || $case_name == forced-detach || $case_name == restart-sibling || $case_name == multiapp-normal || $case_name == early-readiness || $case_name == ignored-service-env || $case_name == retained-isolation ]]
  cli_case=$case_name
  if [[ $case_name == multiapp-normal || $case_name == retained-isolation ]]; then cli_case=normal; fi
  if [[ $case_name == retained-isolation ]]; then binary=$prepared/retained-isolation.test; fi
  [[ -x $binary && -x $prepared/$cli_case ]]
  work=$(mktemp -d /tmp/wendy-watch-container.XXXXXX)
  namespace_started=0
  namespace_drained=0
  interrupted=0
  # shellcheck disable=SC2329
  cleanup() {
    status=$?
    trap - EXIT
    trap '' INT TERM
    if [[ $interrupted == 1 || ( $namespace_started == 1 && $namespace_drained != 1 ) ]]; then
      echo "Retaining owned work: namespace drain is uncertain: $work" >&2
      status=1
    elif findmnt -rn -o TARGET | awk -v root="$work" '$0 == root || index($0, root "/") == 1 { found=1 } END {exit !found}'; then
      echo "Retaining mounted fixture work: $work" >&2
      status=1
    else
      rm -rf -- "$work"
    fi
    exit "$status"
  }
  trap cleanup EXIT
  trap 'interrupted=1; exit 130' INT
  trap 'interrupted=1; exit 143' TERM
  trap 'interrupted=1; exit 129' HUP
  for kind in mnt net pid; do
    printf -v "WENDY_WATCH_PARENT_$kind" '%s' "$(readlink "/proc/self/ns/$kind")"
    export "WENDY_WATCH_PARENT_$kind"
  done
  namespace_started=1
  set +e
  timeout --kill-after=5s 180s unshare --mount --net --pid --fork --kill-child=KILL --propagation private --mount-proc \
    bash "$(realpath "$0")" --inside "$binary" "$prepared" "$case_name" "$work" 2>&1 | tee "$prepared/$case_name.log"
  result=${PIPESTATUS[0]}
  set -e
  if [[ $result == 0 || $result == 1 || $result == 2 ]]; then namespace_drained=1; fi
  [[ $namespace_drained == 1 ]]
  if [[ $case_name == multiapp-normal || $case_name == early-readiness || $case_name == ignored-service-env || $case_name == retained-isolation ]]; then
    if [[ $case_name == multiapp-normal ]]; then
      [[ $result == 0 ]]
      grep -q '^--- PASS: TestMultiAppSlowReadinessIsolationLifecycle ' "$prepared/$case_name.log"
      grep -q 'MULTIAPP_LIFECYCLE_VERIFIED ' "$prepared/$case_name.log"
      grep -q 'ROLLING_ISOLATION_VERIFIED ' "$prepared/$case_name.log"
    else
      [[ $result != 0 ]]
      grep -q '^--- FAIL: TestMultiAppSlowReadinessIsolationLifecycle ' "$prepared/$case_name.log"
      case $case_name in
        early-readiness) grep -q 'multiapp boundary failed: each delayed service becomes ready and runs its own hook' "$prepared/$case_name.log" ;;
        ignored-service-env) grep -q 'service effective environment mismatch:' "$prepared/$case_name.log" ;;
        retained-isolation) grep -q 'removed isolation restarted the healthy sibling' "$prepared/$case_name.log" ;;
      esac
    fi
  elif [[ $case_name == normal ]]; then
    [[ $result == 0 ]]
    grep -q '^--- PASS: TestWatchComposeContainerSession ' "$prepared/$case_name.log"
    grep -q 'WATCH_COMPOSE_VERIFIED ' "$prepared/$case_name.log"
  else
    [[ $result != 0 ]]
    grep -q '^--- FAIL: TestWatchComposeContainerSession ' "$prepared/$case_name.log"
    if [[ $case_name == forced-detach ]]; then
      grep -q 'watch container boundary failed: initial actual container output' "$prepared/$case_name.log"
      grep -q 'detached mode' "$prepared/$case_name.log"
    else
      grep -q 'unchanged Compose service was restarted' "$prepared/$case_name.log"
    fi
  fi
  if grep -q '^--- SKIP:' "$prepared/$case_name.log"; then exit 1; fi
  exit
fi
[[ $EUID == 0 && $# == 5 ]]
binary=$2
prepared=$3
case_name=$4
work=$5
for kind in mnt net pid; do
  parent_var="WENDY_WATCH_PARENT_$kind"
  [[ $(readlink "/proc/self/ns/$kind") != "${!parent_var}" ]]
done
mount --make-rprivate /
# Hide system D-Bus, host runtime sockets and all production agent state.
mount -t tmpfs tmpfs /run
mount -t tmpfs tmpfs /var/lib
mkdir -p /var/lib/wendy /run/netns
# Host runner XDG paths disappear behind the private /run mount. runc exec
# writes its process specification here, so give every child a private directory.
export XDG_RUNTIME_DIR=/run/watch-runtime
mkdir -m 700 "$XDG_RUNTIME_DIR"
ip link set lo up
socket="$work/containerd.sock"
cat > "$work/config.toml" <<'CONFIG'
version = 2
disabled_plugins = ["io.containerd.grpc.v1.cri"]
CONFIG
containerd --config "$work/config.toml" --root "$work/root" --state "$work/state" \
  --address "$socket" > "$prepared/$case_name-containerd.log" 2>&1 &
for ((attempt=0; attempt<100; attempt++)); do
  if ctr --address "$socket" version >/dev/null 2>&1; then break; fi
  sleep .05
done
ctr --address "$socket" version
export WENDY_CONTAINERD_INTEGRATION="$socket" WENDY_WATCH_CONTAINER_NAMESPACE=1
cli_case=$case_name
fixture=TestWatchComposeContainerSession
case $case_name in
  multiapp-normal|retained-isolation) cli_case=normal; fixture=TestMultiAppSlowReadinessIsolationLifecycle ;;
  early-readiness|ignored-service-env) fixture=TestMultiAppSlowReadinessIsolationLifecycle ;;
esac
export WENDY_WATCH_CLI="$prepared/$cli_case" WENDY_WATCH_WORK="$work"
# PID1 exits only after the test finishes; Linux then kills every private
# namespace descendant, including detached shims. Never signal saved host PIDs.
exec "$binary" -test.run "^$fixture$" -test.v -test.count=1 -test.timeout=150s
