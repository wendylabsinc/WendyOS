#!/usr/bin/env bash
set -euo pipefail

swift_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temp_dir=$(mktemp -d)
trap 'rm -rf "$temp_dir"' EXIT

# Compile only the supervision policy with a substituted ServiceManagement
# service: no launch agent is registered, no app is launched and no
# preferences outside a throwaway suite are written.
swiftc -swift-version 6 -default-isolation MainActor -parse-as-library \
  "$swift_root/WendyAgentMac/Sources/LaunchAgentSupervision.swift" \
  "$swift_root/Scripts/Tests/LaunchAgentSupervisionTests.swift" \
  -o "$temp_dir/launch-agent-supervision-tests"
"$temp_dir/launch-agent-supervision-tests"
