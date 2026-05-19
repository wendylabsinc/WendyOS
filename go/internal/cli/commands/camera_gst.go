package commands

import (
	"fmt"
	"os"
	"os/exec"
)

// gstLaunchFallbackPathsFn indirects gstLaunchFallbackPaths so tests can stub
// the platform-specific candidate list.
var gstLaunchFallbackPathsFn = gstLaunchFallbackPaths

// resolveGSTLaunch locates the gst-launch-1.0 binary used for local camera
// playback. It checks PATH first, then falls back to well-known install
// locations (see gstLaunchFallbackPaths, which is platform-specific). The
// fallback matters on Windows: the GStreamer installer (and the winget
// "gstreamer" package that wraps it) does not add its bin directory to PATH —
// it only sets the GSTREAMER_1_0_ROOT_* environment variables — so a bare
// exec.LookPath would fail even on a correctly installed system.
func resolveGSTLaunch() (string, error) {
	if path, err := exec.LookPath(gstLaunchName); err == nil {
		return path, nil
	}
	for _, candidate := range gstLaunchFallbackPathsFn() {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found; install GStreamer or use --stdout to pipe raw video", gstLaunchName)
}
