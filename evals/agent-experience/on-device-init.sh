#!/bin/sh
# PID 1 of the model-tool-report app: supervise buildkitd, run one benchmark pass, then stay alive so
# `wendy device attach` can read /results/latest/report.md. Same buildkitd handling as
# Examples/ClaudeOnDevice/init.sh.
set -e

mkdir -p /run/buildkit /var/lib/buildkit /results /models
BUILDKITD_PID=""

# buildkitd's lock lives on a persist volume and outlives the container. After a SIGKILL, a crash loop
# or a reboot it is left behind and buildkitd refuses to start forever. Only this container uses the
# volume, so with no daemon of ours running, any lock is stale.
clear_stale_lock() {
  if [ -n "$BUILDKITD_PID" ] && kill -0 "$BUILDKITD_PID" 2>/dev/null; then
    return
  fi
  if [ -e /var/lib/buildkit/buildkitd.lock ]; then
    echo "note: clearing stale buildkitd.lock (no buildkitd running)" >&2
    rm -f /var/lib/buildkit/buildkitd.lock
  fi
}

start_buildkitd() {
  clear_stale_lock
  buildkitd ${BUILDKIT_SNAPSHOTTER:+--oci-worker-snapshotter="$BUILDKIT_SNAPSHOTTER"} >&2 2>&1 &
  BUILDKITD_PID=$!
}

# Stop the pass and the daemon together when the app is stopped.
PASS_PID=""
trap 'kill "$PASS_PID" 2>/dev/null; kill "$BUILDKITD_PID" 2>/dev/null; exit 0' TERM INT

start_buildkitd
i=0
while [ "$i" -lt 10 ]; do
  [ -S /run/buildkit/buildkitd.sock ] && break
  i=$((i + 1))
  sleep 0.5
done
[ -S /run/buildkit/buildkitd.sock ] || echo "warning: buildkitd socket not up yet; the first build may need a retry" >&2

# One pass, in the background so buildkitd stays supervised. Whatever it returns, this script keeps
# running afterwards: a finished pass has nothing left to do, a failed one left /results/last-error.txt,
# and `wendy device attach` needs the container alive either way.
(
  cd /opt/harness
  python3 on_device.py
  echo "pass exited with code $?" >&2
) &
PASS_PID=$!

# Supervise buildkitd forever: restart it with a short backoff if it exits.
set +e
while true; do
  wait "$BUILDKITD_PID"
  echo "warning: buildkitd exited (code $?); restarting in 1s" >&2
  sleep 1
  start_buildkitd
done
