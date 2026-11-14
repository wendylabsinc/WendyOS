#!/usr/bin/env bash
# Run in an existing Go service job. All sockets and PKI are local fixtures.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
output="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/wendy-notification-contract"
mkdir -p "$output"
tests='^(TestAppNotificationsCrossUnixSocketAndVerifiedCloudMTLS|TestCampaignNotificationsRetryThroughVerifiedCloudMTLS|TestCampaignCloudFailureRedactsStatusAndWarnings)$'
go test -race ./go/internal/agent/services -run "$tests" -v -count=1 -timeout 60s | tee "$output/normal.log"
for test_name in TestAppNotificationsCrossUnixSocketAndVerifiedCloudMTLS TestCampaignNotificationsRetryThroughVerifiedCloudMTLS TestCampaignCloudFailureRedactsStatusAndWarnings; do
  grep -q "^--- PASS: $test_name " "$output/normal.log"
done
if grep -q '^--- SKIP:' "$output/normal.log"; then
  echo 'A required notification contract regression was skipped' >&2
  exit 1
fi

python3 - "$output" <<'PY'
import json
from pathlib import Path
import sys

out = Path(sys.argv[1]).resolve()
services = Path('go/internal/agent/services').resolve()
mutations = {
    'corrupt-signed-payload': (
        services / 'notification_service.go',
        'canonical = append(canonical, requestBytes...)',
        'canonical = append(canonical, requestBytes[:len(requestBytes)-1]...)',
    ),
    'disable-campaign-retry': (
        services / 'data_inference.go',
        'for attempt := 0; attempt < 3 && ctx.Err() == nil; attempt++ {',
        'for attempt := 0; attempt < 1 && ctx.Err() == nil; attempt++ {',
    ),
    'retry-duplicate-notification': (
        services / 'data_inference.go',
        'codes.FailedPrecondition, codes.AlreadyExists, codes.Unimplemented, codes.DataLoss:',
        'codes.FailedPrecondition, codes.Unimplemented, codes.DataLoss:',
    ),
    'expose-cloud-error': (
        services / 'data_inference.go',
        'err = status.Error(status.Code(err), "Cloud notification delivery failed")',
        'err = fmt.Errorf("%w", err)',
    ),
}
for name, (source, before, after) in mutations.items():
    original = source.read_text()
    if original.count(before) != 1:
        raise SystemExit(f'{name}: source mutation no longer identifies exactly one site')
    replacement = out / (name + '.go')
    replacement.write_text(original.replace(before, after))
    (out / (name + '.json')).write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
PY

for mutation in corrupt-signed-payload disable-campaign-retry retry-duplicate-notification expose-cloud-error; do
  case "$mutation" in
    corrupt-signed-payload) expected='received notification proof failed verification' ;;
    disable-campaign-retry) expected='campaign Cloud retry outcome:' ;;
    retry-duplicate-notification) expected='Cloud attempt counts: transient=2 duplicate=3' ;;
    expose-cloud-error) expected='campaign error exposed private Cloud response or lost status code:' ;;
  esac
  if go test -race -overlay "$output/$mutation.json" ./go/internal/agent/services -run "$tests" -v -count=1 -timeout 60s > "$output/$mutation.log" 2>&1; then
    cat "$output/$mutation.log"
    echo "Regression did not reject $mutation" >&2
    exit 1
  fi
  cat "$output/$mutation.log"
  grep -q '^--- FAIL: Test' "$output/$mutation.log"
  grep -q "$expected" "$output/$mutation.log"
done
