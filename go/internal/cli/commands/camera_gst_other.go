//go:build !windows

package commands

import "path/filepath"

// gstLaunchName is the executable name searched on PATH.
const gstLaunchName = "gst-launch-1.0"

// gstUnixFallbackDirs mirrors the agent-side gstFallbackDirs: standard system
// bin directories searched when the binary is not on PATH. Declared as a var so
// tests can override it.
var gstUnixFallbackDirs = []string{"/usr/bin", "/usr/local/bin", "/usr/sbin"}

// gstLaunchFallbackPaths returns full candidate paths to gst-launch-1.0.
func gstLaunchFallbackPaths() []string {
	paths := make([]string, 0, len(gstUnixFallbackDirs))
	for _, dir := range gstUnixFallbackDirs {
		paths = append(paths, filepath.Join(dir, gstLaunchName))
	}
	return paths
}
