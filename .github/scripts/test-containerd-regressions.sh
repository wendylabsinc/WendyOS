#!/usr/bin/env bash
# Run on an ephemeral Linux CI runner, using only a private containerd daemon.
set -euo pipefail
binary=${1:?usage: test-containerd-regressions.sh /path/to/containerd.test}
if [[ $(id -u) != 0 ]]; then
  echo 'Containerd snapshot integration tests require root on the CI runner.' >&2
  exit 1
fi
for command in containerd ctr runc busybox; do
  command -v "$command" >/dev/null
done
work=$(mktemp -d /tmp/wendy-containerd.XXXXXX)
socket="$work/containerd.sock"
pid=
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n "$pid" ]]; then
    # Only this private daemon is addressed. Reap tasks after a test timeout too.
    while IFS= read -r namespace; do
      while IFS= read -r task; do
        [[ -n "$task" ]] || continue
        ctr --address "$socket" --namespace "$namespace" tasks kill --signal SIGKILL "$task" >/dev/null 2>&1 || true
        ctr --address "$socket" --namespace "$namespace" tasks delete --force "$task" >/dev/null 2>&1 || true
      done < <(ctr --address "$socket" --namespace "$namespace" tasks list --quiet 2>/dev/null || true)
    done < <(ctr --address "$socket" namespaces list --quiet 2>/dev/null || true)
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
  if [[ $status != 0 ]]; then
    cat "$work/containerd.log" >&2
  fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
cat > "$work/config.toml" <<EOF_CONFIG
version = 2
disabled_plugins = ["io.containerd.grpc.v1.cri"]
EOF_CONFIG
containerd --version
runc --version
containerd --config "$work/config.toml" --root "$work/root" --state "$work/state" \
  --address "$socket" > "$work/containerd.log" 2>&1 &
pid=$!
ready=false
for ((attempt=0; attempt<100; attempt++)); do
  if ctr --address "$socket" version >/dev/null 2>&1; then
    ready=true
    break
  fi
  kill -0 "$pid"
  sleep 0.1
done
if [[ $ready != true ]]; then
  echo 'Private containerd did not become ready.' >&2
  exit 1
fi
"$binary" -test.list '^TestIntegration' > "$work/expected"
if ! grep -q '^TestIntegration' "$work/expected"; then
  echo 'The test binary contains no containerd integration tests.' >&2
  exit 1
fi
WENDY_CONTAINERD_INTEGRATION="$socket" "$binary" \
  -test.run '^TestIntegration' -test.v -test.count=1 -test.timeout=300s | tee "$work/results"
# A skipped or accidentally unmatched test must not produce a green CI job.
if grep -q '^--- SKIP:' "$work/results"; then
  echo 'An integration test was skipped.' >&2
  exit 1
fi
while IFS= read -r name; do
  [[ "$name" == TestIntegration* ]] || continue
  if ! grep -Fq -- "--- PASS: $name (" "$work/results"; then
    echo "Missing passing result for $name" >&2
    exit 1
  fi
done < "$work/expected"
