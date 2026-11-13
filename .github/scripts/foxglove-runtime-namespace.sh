#!/usr/bin/env bash
# Invoked only by test-foxglove-runtime.py on the disposable hosted runner.
set -euo pipefail
[[ $EUID == 0 && ${GITHUB_ACTIONS:-} == true && ${RUNNER_ENVIRONMENT:-} == github-hosted && $# == 4 ]]
binary=$1 work=$2 output=$3 docker_socket=$4
for kind in mnt net pid; do
  parent="WENDY_FOXGLOVE_PARENT_$kind"
  [[ -n ${!parent:-} && $(readlink "/proc/self/ns/$kind") != "${!parent}" ]]
done
unset DOCKER_CONTEXT DOCKER_TLS_VERIFY DOCKER_CERT_PATH
mount --make-rprivate /
# Preserve only the explicitly selected local Docker control socket. Its daemon
# builds the generated image; it never receives the private containerd socket.
touch "$work/docker.sock"
mount --bind "$docker_socket" "$work/docker.sock"
[[ $docker_socket == /run/* ]]
mount -t tmpfs -o mode=755 tmpfs /run
mkdir -p "$(dirname "$docker_socket")"
touch "$docker_socket"
mount --bind "$work/docker.sock" "$docker_socket"
# Retain the original endpoint spelling across fresh namespaces/cases so the
# owned buildx registration never points at a deleted per-case work directory.
export DOCKER_HOST="unix://$docker_socket"
mount -t tmpfs -o mode=755 tmpfs /var/lib
mkdir -p /run/foxglove-runtime /run/foxglove-cgroup /var/lib/wendy "$work/home" "$work/cache" "$work/config"
chmod 700 /run/foxglove-runtime "$work/home" "$work/cache" "$work/config"
mount --rbind /sys/fs/cgroup /run/foxglove-cgroup
mount -t sysfs -o ro,nosuid,nodev,noexec sysfs /sys
mount --rbind /run/foxglove-cgroup /sys/fs/cgroup
ip link set lo up
export XDG_RUNTIME_DIR=/run/foxglove-runtime HOME="$work/home" XDG_CACHE_HOME="$work/cache"
export WENDY_CONFIG_DIR="$work/config" WENDY_ANALYTICS=false WENDY_NO_UPDATE_CHECK=1 WENDY_SECRET_STORE=file
export WENDY_FOXGLOVE_RUNTIME=1 WENDY_FOXGLOVE_WORK="$work" WENDY_FOXGLOVE_OUTPUT="$output"
cat > "$work/containerd.toml" <<'CONFIG'
version = 2
disabled_plugins = ["io.containerd.grpc.v1.cri"]
CONFIG
containerd --config "$work/containerd.toml" --root "$work/root" --state "$work/state" --address "$work/containerd.sock" > "$output/containerd.log" 2>&1 &
for ((i=0; i<100; i++)); do
  if ctr --address "$work/containerd.sock" version >/dev/null 2>&1; then break; fi
  sleep .05
done
ctr --address "$work/containerd.sock" version
# PID1 exit kills the private daemon, runc shims and every remaining ROS task.
# The parent requires a waited namespace exit before deleting backing files.
exec "$binary" -test.run '^TestFoxgloveGeneratedAppDeploysAndCarriesROS$' -test.count=1 -test.v -test.timeout=0
