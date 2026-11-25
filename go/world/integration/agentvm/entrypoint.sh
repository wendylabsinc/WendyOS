#!/bin/bash
# Starts only private runtime infrastructure. Enrollment and the real Wendy
# agent are explicit, separately logged steps performed by the exercise runner.
set -Eeuo pipefail
umask 027

readonly run_dir=/run/agentvm
readonly log_dir=/var/lib/agentvm/logs
mkdir -p "$run_dir" "$log_dir" /run/containerd /run/buildkit /var/lib/buildkit
rm -f "$run_dir/ready.json"
containerd_pid=
buildkit_pid=
cleanup() {
    local result=$?
    trap - EXIT INT TERM
    rm -f "$run_dir/ready.json"
    for pid in "$buildkit_pid" "$containerd_pid"; do
        if [[ -n "$pid" ]]; then kill -TERM "$pid" 2>/dev/null || true; fi
    done
    wait 2>/dev/null || true
    exit "$result"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

python3 /usr/local/libexec/agentvm-preflight.py > "$run_dir/preflight.json"
containerd --root /var/lib/containerd --state /run/containerd \
    --address /run/containerd/containerd.sock > "$log_dir/containerd.log" 2>&1 &
containerd_pid=$!

wait_runtime() {
    local name=$1 pid=$2
    shift 2
    for ((attempt=0; attempt<120; attempt++)); do
        if ! kill -0 "$pid" 2>/dev/null; then
            printf '%s exited before readiness; see %s/%s.log\n' "$name" "$log_dir" "$name" >&2
            return 1
        fi
        if timeout 2 "$@" > "$run_dir/$name-check.txt" 2>&1; then return 0; fi
        sleep 0.25
    done
    printf '%s readiness timed out; see %s/%s.log\n' "$name" "$log_dir" "$name" >&2
    return 1
}
wait_runtime containerd "$containerd_pid" ctr --address /run/containerd/containerd.sock version

cat > "$run_dir/buildkit.toml" <<'EOF'
[worker.oci]
  enabled = false
[worker.containerd]
  enabled = true
  address = "/run/containerd/containerd.sock"
  namespace = "default"
  snapshotter = "native"
EOF
buildkitd --config "$run_dir/buildkit.toml" --root /var/lib/buildkit \
    --addr unix:///run/buildkit/buildkitd.sock > "$log_dir/buildkit.log" 2>&1 &
buildkit_pid=$!
wait_runtime buildkit "$buildkit_pid" buildctl --addr unix:///run/buildkit/buildkitd.sock debug workers

python3 - "$containerd_pid" "$buildkit_pid" <<'PY'
import json
import os
from pathlib import Path
import sys
import time
path = Path("/run/agentvm")
data = {
    "version": 1,
    "ready": True,
    "ready_unix_seconds": time.time(),
    "agent_started": False,
    "containerd": {"pid": int(sys.argv[1]), "address": "/run/containerd/containerd.sock", "namespace": "default"},
    "buildkit": {"pid": int(sys.argv[2]), "address": "unix:///run/buildkit/buildkitd.sock", "snapshotter": "native"},
    "log_directory": "/var/lib/agentvm/logs",
    "preflight": json.loads((path / "preflight.json").read_text()),
}
temporary = path / "ready.json.tmp"
temporary.write_text(json.dumps(data, sort_keys=True) + "\n")
os.replace(temporary, path / "ready.json")
PY
printf 'Private node runtime ready: %s/ready.json\n' "$run_dir"
wait -n "$containerd_pid" "$buildkit_pid"
# Even an orderly daemon exit makes this node unavailable.
exit 1
