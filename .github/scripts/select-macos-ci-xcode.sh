#!/bin/bash
# Use DEVELOPER_DIR rather than changing the machine's global xcode-select state.
set -euo pipefail
version=${1:?Expected an Xcode version}
[[ "$version" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || { echo 'Invalid Xcode version' >&2; exit 2; }
applications=${XCODES_DIRECTORY:-/Applications}
for app in "$applications"/Xcode*.app; do
  developer="$app/Contents/Developer"
  [[ -x "$developer/usr/bin/xcodebuild" ]] || continue
  installed=$(DEVELOPER_DIR="$developer" "$developer/usr/bin/xcodebuild" -version | awk '/^Xcode / { print $2 }')
  if [[ "${installed%.0}" == "${version%.0}" ]]; then
    export DEVELOPER_DIR="$developer"
    xcrun --sdk macosx --show-sdk-path
    xcrun --find clang
    [[ -z "${GITHUB_ENV:-}" ]] || echo "DEVELOPER_DIR=$developer" >> "$GITHUB_ENV"
    echo "Using Xcode $installed at $developer"
    exit 0
  fi
done
echo "::error::Xcode $version is missing. As the runner user, run: bash .github/scripts/provision-macos-ci.sh $version" >&2
exit 1
