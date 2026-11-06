#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT
cat "$ROOT/WendyAgentMac/Sources/NetworkAccessPreferences.swift" > "$TEST_DIR/test.swift"
cat >> "$TEST_DIR/test.swift" <<'SWIFT'
let suite = "dev.wendy.network-access-test." + UUID().uuidString
let defaults = UserDefaults(suiteName: suite)!
defer { defaults.removePersistentDomain(forName: suite) }
let preferences = NetworkAccessPreferences(defaults: defaults)
assert(preferences.prepareForStartup(existingInstallation: true) == false)
assert(preferences.allowsNetworkAccess, "legacy install must preserve network access")
preferences.allowsNetworkAccess = false
assert(preferences.prepareForStartup(existingInstallation: true) == false)
assert(!preferences.allowsNetworkAccess, "migration must preserve explicit revocation")
defaults.removePersistentDomain(forName: suite)
assert(preferences.prepareForStartup(existingInstallation: false))
assert(!preferences.allowsNetworkAccess, "fresh install starts locally")
assert(preferences.prepareForStartup(existingInstallation: true), "an unanswered prompt is asked again")
assert(!preferences.allowsNetworkAccess, "new state must not be mistaken for an upgrade on restart")
preferences.markPromptAnswered()
assert(!preferences.prepareForStartup(existingInstallation: true), "an answered prompt is not repeated")
preferences.allowsNetworkAccess = true
assert(!preferences.prepareForStartup(existingInstallation: false))
assert(preferences.allowsNetworkAccess, "saved opt-in must survive restart")
print("Passed network-access migration, revocation, fresh install, prompt and restart tests")
SWIFT
swift -swift-version 6 "$TEST_DIR/test.swift"
