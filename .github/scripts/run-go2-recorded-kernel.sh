#!/bin/bash
# Only the coordinator starts this inside its owned PID/mount namespace.
set -euo pipefail
[[ $EUID == 0 && $# == 3 ]]
[[ $(readlink /proc/self/ns/pid) != "$3" ]]
mount --make-rprivate /
mount -t tmpfs -o mode=755 tmpfs /run
export XDG_RUNTIME_DIR=/run/go2-runtime
mkdir -m700 "$XDG_RUNTIME_DIR"
export WENDY_GO2_H264_KERNEL=1 WENDY_GO2_CAPTURE_BINARY=$2
bash .github/scripts/test-containerd-regressions.sh "$1" '^TestGo2H264KernelServiceAndContainerDelivery$'
