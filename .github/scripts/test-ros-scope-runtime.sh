#!/usr/bin/env bash
# Hosted only: all links, route views and daemons live in fresh namespaces.
set -euo pipefail
if [[ ${1:-} != --inside ]]; then
  [[ ${GITHUB_ACTIONS:-} == true && ${RUNNER_ENVIRONMENT:-} == github-hosted && $EUID == 0 && $# == 3 ]] || {
    echo 'usage on disposable hosted Linux: sudo -E test-ros-scope-runtime.sh TEST_BINARY PREPARED_DIR CASE' >&2; exit 2;
  }
  binary=$(realpath "$1")
  prepared=$(realpath "$2")
  case_name=$3
  [[ -x $binary && -f $prepared/manifest.json ]]
  [[ $case_name == normal || $case_name == old-loopback || $case_name == fail-open-routes ]]
  work=$(mktemp -d /tmp/wendy-ros-scope.XXXXXX)
  namespace_started=0
  namespace_drained=0
  interrupted=0
  # shellcheck disable=SC2329
  cleanup() {
    status=$?
    trap - EXIT
    trap '' INT TERM
    retained=0
    # A normal unshare return reaps the namespace init. Direct interruption
    # or a timeout does not prove drain here: parent findmnt cannot see private
    # mounts. Retain the owned work rather than deleting beneath live tasks.
    if [[ $interrupted == 1 || ( $namespace_started == 1 && $namespace_drained != 1 ) ]]; then
      echo "Retaining owned work because namespace drain is uncertain: $work" >&2
      status=1
      retained=1
    elif findmnt -rn -o TARGET | awk -v root="$work" '$0 == root || index($0, root "/") == 1 { found=1 } END {exit !found}'; then
      echo "Refusing to delete mounted fixture work: $work" >&2
      status=1
      retained=1
    else
      rm -rf -- "$work" || { status=1; retained=1; }
    fi
    python3 - "$prepared" "$case_name" "$retained" "$work" <<'CLEANUP'
import json,pathlib,sys
root,name,retained,work=sys.argv[1:]
(pathlib.Path(root)/(name+'-cleanup.json')).write_text(json.dumps({'status':'retained-uncertain' if retained=='1' else 'removed','work':work},indent=2)+'\n')
CLEANUP
    exit "$status"
  }
  trap cleanup EXIT
  trap 'interrupted=1; exit 130' INT
  trap 'interrupted=1; exit 143' TERM
  trap 'interrupted=1; exit 129' HUP
  export WENDY_ROS_SCOPE_PARENT_mnt WENDY_ROS_SCOPE_PARENT_net WENDY_ROS_SCOPE_PARENT_pid
  WENDY_ROS_SCOPE_PARENT_mnt=$(readlink /proc/self/ns/mnt)
  WENDY_ROS_SCOPE_PARENT_net=$(readlink /proc/self/ns/net)
  WENDY_ROS_SCOPE_PARENT_pid=$(readlink /proc/self/ns/pid)
  python3 - "$prepared" <<'VERIFY'
import hashlib,json,pathlib,sys
root=pathlib.Path(sys.argv[1]);manifest=json.loads((root/'manifest.json').read_text())
assert manifest['status']=='prepared' and not manifest['cleanup_errors']
hash=hashlib.sha256()
with (root/'ros-image.tar').open('rb') as archive:
    for block in iter(lambda: archive.read(1<<20),b''):hash.update(block)
assert hash.hexdigest()==manifest['archive_sha256'], 'ROS image archive changed'
VERIFY
  set +e
  namespace_started=1
  timeout --kill-after=5s 150s unshare --mount --net --pid --fork --kill-child=KILL --propagation private --mount-proc \
    bash "$(realpath "$0")" --inside "$binary" "$prepared" "$case_name" "$work" 2>&1 | tee "$prepared/$case_name.log"
  result=${PIPESTATUS[0]}
  # Exit codes below timeout's reserved range come from a waited namespace
  # init (including an expected Go assertion failure). Signal/timeout paths
  # remain uncertain and cleanup conservatively retains their private work.
  if (( result < 124 )); then namespace_drained=1; fi
  set -e
  if [[ $case_name == normal ]]; then
    [[ $result == 0 ]]
    grep -q '^--- PASS: TestROS2ScopeRuntimeVisibility ' "$prepared/$case_name.log"
    if grep -q '^--- SKIP:' "$prepared/$case_name.log"; then exit 1; fi
  else
    [[ $result != 0 && $result != 124 && $result != 137 ]]
    grep -q '^--- FAIL: TestROS2ScopeRuntimeVisibility ' "$prepared/$case_name.log"
    if [[ $case_name == old-loopback ]]; then
      grep -q 'scope-network missing fresh device payloads' "$prepared/$case_name.log"
    else
      grep -q 'scope-unreadable-v6 leaked excluded' "$prepared/$case_name.log"
    fi
  fi
  python3 - "$prepared" "$case_name" "$result" <<'RESULT'
import json,pathlib,sys
root,name,code=sys.argv[1:]
(pathlib.Path(root)/(name+'-result.json')).write_text(json.dumps({'status':'passed','case':name,'test_exit_code':int(code),'negative_control':name!='normal'},indent=2)+'\n')
RESULT
  exit
fi
[[ $EUID == 0 && $# == 5 ]]
binary=$2
prepared=$3
case_name=$4
work=$5
for kind in mnt net pid; do
  parent_var="WENDY_ROS_SCOPE_PARENT_$kind"
  [[ $(readlink "/proc/self/ns/$kind") != "${!parent_var}" ]]
done
mount --make-rprivate /
mount -t tmpfs tmpfs /run
mkdir -p /run/netns /run/ros-cgroup /run/ros-runtime
mount --rbind /sys/fs/cgroup /run/ros-cgroup
mount -t sysfs -o ro,nosuid,nodev,noexec sysfs /sys
mount --rbind /run/ros-cgroup /sys/fs/cgroup
export XDG_RUNTIME_DIR=/run/ros-runtime
ip link set lo up
# Named namespaces are private too: /run is this mount namespace's tmpfs.
for peer in uplink peripheral; do
  ip netns add "$peer"
  ip netns exec "$peer" ip link set lo up
done
ip link add uplink0 type veth peer name external0
ip link set external0 netns uplink
ip addr add 192.0.2.1/24 dev uplink0
ip -6 addr add fd00:2266:1::1/64 dev uplink0 nodad
ip link set uplink0 up
ip netns exec uplink ip addr add 192.0.2.2/24 dev external0
ip netns exec uplink ip -6 addr add fd00:2266:1::2/64 dev external0 nodad
ip netns exec uplink ip link set external0 up
ip route add default via 192.0.2.2 dev uplink0
ip -6 route add default via fd00:2266:1::2 dev uplink0
ip link add periph0 type veth peer name cable0
ip link set cable0 netns peripheral
ip addr add 198.51.100.1/24 dev periph0
ip link set periph0 up
ip netns exec peripheral ip addr add 198.51.100.2/24 dev cable0
ip netns exec peripheral ip link set cable0 up
# Veth has no physical-device symlink. Emulate only that kernel classification
# marker in this private mount; actual interfaces/routes and DDS remain real.
mkdir -p /run/ros-sys-net/{lo,uplink0/device,periph0/device}
mount --bind /run/ros-sys-net /sys/class/net
export WENDY_ROS_SCOPE_NAMESPACE=1 WENDY_ROS_SCOPE_WORK="$work"
export WENDY_ROS_SCOPE_ARCHIVE="$prepared/ros-image.tar"
WENDY_ROS_SCOPE_IMAGE=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["image"])' "$prepared/manifest.json")
export WENDY_ROS_SCOPE_IMAGE
socket="$work/containerd.sock"
cat > "$work/config.toml" <<'CONFIG'
version = 2
disabled_plugins = ["io.containerd.grpc.v1.cri"]
CONFIG
containerd --config "$work/config.toml" --root "$work/root" --state "$work/state" \
  --address "$socket" > "$prepared/$case_name-containerd.log" 2>&1 &
# Once the final exec exits, Linux kills every remaining private PID namespace
# descendant, including a failed test's daemon/shims. No saved host PID signals.
for ((attempt=0; attempt<100; attempt++)); do
  if ctr --address "$socket" version >/dev/null 2>&1; then break; fi
  sleep .05
done
ctr --address "$socket" version
export WENDY_CONTAINERD_INTEGRATION="$socket"
exec "$binary" -test.run '^TestROS2ScopeRuntimeVisibility$' -test.v -test.count=1 -test.timeout=120s
