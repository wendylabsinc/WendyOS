#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/../../go/macos"
ruby Scripts/menu-ui-project.rb
xcodegen generate --spec Build/MenuUI/project.json --project-root . --project .
args=(-project WendyMenuUI.xcodeproj -scheme WendyMenuUI -configuration Debug
      -destination 'platform=macOS' -derivedDataPath Build/MenuUI/DerivedData
      -parallel-testing-enabled NO -test-timeouts-enabled YES
      -maximum-test-execution-time-allowance 180 CODE_SIGNING_ALLOWED=NO)
xcodebuild "${args[@]}" -resultBundlePath Build/MenuUI/positive.xcresult test
xcrun xcresulttool get test-results summary --path Build/MenuUI/positive.xcresult > Build/MenuUI/positive.json
python3 - <<'PY'
import json
from pathlib import Path
result = json.loads(Path('Build/MenuUI/positive.json').read_text())
assert result['totalTestCount'] == 6, result
assert result['failedTests'] == 0 and result['skippedTests'] == 0, result
assert result['result'] == 'Passed', result
PY

# A small icon-only hit target must fail an actual edge activation, not a frame
# assertion. Restore the production file even when building/testing is interrupted.
source=Sources/App/PlatformServices.swift
backup=$(mktemp)
cp "$source" "$backup"
trap 'cp "$backup" "$source"; rm -f "$backup"' EXIT
python3 - <<'PY'
from pathlib import Path
path = Path('Sources/App/PlatformServices.swift')
source = path.read_text()
original = '.frame(width: 44, height: 44)'
assert source.count(original) == 1
path.write_text(source.replace(original, '.frame(width: 16, height: 16)'))
PY
set +e
xcodebuild "${args[@]}" -only-testing:WendyMenuUITests/MenuActivationTests/testAppMenuEdges \
  -resultBundlePath Build/MenuUI/icon-only.xcresult test > Build/MenuUI/icon-only.log 2>&1
mutation_status=$?
set -e
test "$mutation_status" -ne 0
xcrun xcresulttool get test-results summary --path Build/MenuUI/icon-only.xcresult > Build/MenuUI/icon-only.json
python3 - <<'PY'
import json
from pathlib import Path
result = json.loads(Path('Build/MenuUI/icon-only.json').read_text())
assert result['totalTestCount'] == 1 and result['failedTests'] == 1, result
assert result['skippedTests'] == 0, result
assert any('Edge click did not open the production app menu' in failure.get('failureText', '')
           for failure in result['testFailures']), result
PY
