#!/usr/bin/env bash
# Replace only the already job-owned module; retain the candidate for every
# existing media consumer and the workflow's final always() module cleanup.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != true || "${RUNNER_ENVIRONMENT:-}" != github-hosted || "${WENDY_LOOPBACK_JOB_OWNED:-}" != 1 || "$(uname -s)" != Linux ]]; then
  echo 'Output queue comparison requires the disposable hosted media job and its owned loaded module.' >&2
  exit 2
fi
cd "$(git rev-parse --show-toplevel)"
source="$(realpath "${1:?exact upstream checkout required}")"
builder="$(realpath "${2:?exact Builder checkout required}")"
if [[ ! -c /dev/v4l2loopback || "$(cat /sys/module/v4l2loopback/version)" != 0.15.4 ]]; then
  echo 'Expected the original job-owned v4l2loopback 0.15.4 module.' >&2
  exit 2
fi
if [[ -e /dev/video200 || -L /dev/video200 ]]; then
  echo 'Refusing an existing output queue fixture node.' >&2
  exit 2
fi
output="$(mktemp -d "${RUNNER_TEMP:?}/wendy-output-queue.XXXXXX")"
trap 'rm -f "$output/services.test"' EXIT
python3 .github/scripts/v4l2-output-queue-provenance.py "$source" "$builder" "$output/provenance.json"
go test -race -tags regression_camera_fixture -c -o "$output/services.test" ./go/internal/agent/services
python3 - "$output" <<'PYRECORD'
import hashlib,json,subprocess,sys
from pathlib import Path
output=Path(sys.argv[1])
files=[Path('go/internal/agent/services/ipcam_output_queue_linux_test.go'),Path('.github/scripts/run-owned-media-test.py'),output/'services.test']
data={'wendyos_revision':subprocess.check_output(['git','rev-parse','HEAD'],text=True,timeout=10).strip(),
      'files':{str(path):hashlib.sha256(path.read_bytes()).hexdigest() for path in files}}
(output/'fixture.json').write_text(json.dumps(data,indent=2)+'\n')
PYRECORD
selection='^TestIPCameraOutputQueueLifecycle$'
run_queue() {
  sudo env GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted WENDY_IPCAM_DELIVERY_DIAGNOSTIC=output-queue-lifecycle \
    python3 .github/scripts/run-owned-media-test.py --timeout 25 -- \
    "$output/services.test" -test.run "$selection" -test.v -test.count=1 -test.timeout=0
}
# Preserve both modules' actual bytes and kernel ABI identity without putting
# binaries in the artifact. The workflow loaded this original module itself.
record_module() {
  python3 - "$source/v4l2loopback.ko" "$output/$1-module.json" <<'PY'
import hashlib,json,platform,subprocess,sys
from pathlib import Path
module=Path(sys.argv[1]); data={'sha256':hashlib.sha256(module.read_bytes()).hexdigest(),'kernel_release':platform.release()}
for key in ('version','vermagic','srcversion'):
    data[key]=subprocess.check_output(['modinfo','-F',key,str(module)],text=True,timeout=10).strip()
if data['version']!='0.15.4' or data['vermagic'].split()[0]!=data['kernel_release']:
    raise SystemExit('module version/kernel ABI mismatch')
Path(sys.argv[2]).write_text(json.dumps(data,indent=2)+'\n')
PY
}
record_module original
status=0
run_queue > "$output/original.log" 2>&1 || status=$?
cat "$output/original.log"
if [[ "$status" != 1 ]] || ! grep -q 'OUTPUT_QUEUE_CONTRACT:' "$output/original.log" || ! grep -q '^--- FAIL: TestIPCameraOutputQueueLifecycle ' "$output/original.log"; then
  echo 'Original module did not fail at the required output completion boundary.' >&2
  exit 1
fi
if grep -Eq -- '--- SKIP:|WARNING: DATA RACE|panic:|test timed out|OUTPUT_QUEUE_LIFECYCLE_VERIFIED' "$output/original.log"; then
  echo 'Original module comparison produced invalid failure evidence.' >&2
  exit 1
fi
if [[ -e /dev/video200 || -L /dev/video200 ]]; then
  echo 'Original fixture node cleanup is unproven; refusing module replacement.' >&2
  exit 1
fi
# The guardian has returned an ordinary Go test failure and the owned node is
# gone. rmmod still refuses any remaining active users rather than forcing them.
sudo rmmod v4l2loopback
if [[ -d /sys/module/v4l2loopback ]]; then
  echo 'Original module remained loaded.' >&2
  exit 1
fi
patch="$builder/recipes-kernel/v4l2loopback/files/0001-output-completion-queue.patch"
make -C "$source" clean > "$output/clean.log" 2>&1
git -C "$source" apply --check "$patch"
git -C "$source" apply "$patch"
python3 - "$source/v4l2loopback.c" "$output/provenance.json" <<'PY'
import hashlib,json,sys
from pathlib import Path
actual=hashlib.sha256(Path(sys.argv[1]).read_bytes()).hexdigest()
expected=json.loads(Path(sys.argv[2]).read_text())['patched_source_sha256']
if actual!=expected:raise SystemExit('patched source differs from reviewed Builder candidate')
PY
# Match the shared Builder recipe's MAKE_TARGETS; never compile a different
# driver only for the tests or install it into the runner's module tree.
make -C "$source" v4l2loopback.ko > "$output/candidate-build.log" 2>&1
record_module candidate
sudo insmod "$source/v4l2loopback.ko" devices=0 exclusive_caps=1
sudo udevadm settle --timeout=10
test -c /dev/v4l2loopback
status=0
run_queue > "$output/candidate.log" 2>&1 || status=$?
cat "$output/candidate.log"
if [[ "$status" != 0 ]] || ! grep -q '^--- PASS: TestIPCameraOutputQueueLifecycle ' "$output/candidate.log" || ! grep -q 'OUTPUT_QUEUE_LIFECYCLE_VERIFIED' "$output/candidate.log"; then
  echo 'Candidate did not satisfy the actual output queue lifecycle.' >&2
  exit 1
fi
if grep -Eq -- '--- SKIP:|WARNING: DATA RACE' "$output/candidate.log" || [[ -e /dev/video200 || -L /dev/video200 ]]; then
  echo 'Candidate evidence or owned node cleanup is invalid.' >&2
  exit 1
fi
echo "OUTPUT_QUEUE_MODULE_COMPARISON_VERIFIED logs=$output; existing decoded/shared consumer gates remain required"
