#!/usr/bin/env bash
set -euo pipefail

swift_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temp_dir=$(mktemp -d)
trap 'rm -rf "$temp_dir"' EXIT

# Compile only the authorization-wait helper: no app launch, TCC, preferences,
# provisioning, Keychain or Secure Enclave access is involved.
swiftc -swift-version 6 -parse-as-library \
  "$swift_root/WendyAgentMac/Sources/LocationPermissionRequest.swift" \
  "$swift_root/Scripts/Tests/LocationPermissionRequestTests.swift" \
  -o "$temp_dir/location-permission-tests"
"$temp_dir/location-permission-tests"

if [[ "$(uname -s)" == Darwin ]]; then
  # Exercise the real observable model with a substituted Core Location source.
  # Initialization only reads other permissions; no request API is invoked.
  swiftc -swift-version 6 -default-isolation MainActor -parse-as-library \
    "$swift_root/WendyAgentMac/Sources/LocationPermissionRequest.swift" \
    "$swift_root/WendyAgentMac/Sources/WelcomeAndPermissions.swift" \
    "$swift_root/Scripts/Tests/LocationPermissionSourceTests.swift" \
    -o "$temp_dir/location-permission-source-tests"
  "$temp_dir/location-permission-source-tests"
fi
