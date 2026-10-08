package commands

import "os"

// runNoCloudFallbackEnv is set to "1" by the MCP run tool
// (internal/cli/mcp/run_target.go) for the `wendy run` it spawns. That run
// must deploy to the device the MCP session selected: after a transient
// direct-connect failure it fails instead of reaching a cloud device that
// merely has the same name.
const runNoCloudFallbackEnv = "WENDY_RUN_NO_CLOUD_FALLBACK"

// cloudFallbackDisabled reports whether resolveWithCloudFallback must not
// tunnel for cloudName. Only the deploy target (cloudName == "") is pinned: a
// build host or fleet member is named explicitly and may be cloud-only.
func cloudFallbackDisabled(cloudName string) bool {
	return cloudName == "" && os.Getenv(runNoCloudFallbackEnv) == "1"
}

// cloudFallbackConnectFn is the tunnel resolveWithCloudFallback falls back
// to; a variable so tests can observe whether the fallback was attempted.
var cloudFallbackConnectFn = connectToCloudAgentExpecting
