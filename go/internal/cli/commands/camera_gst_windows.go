//go:build windows

package commands

import (
	"os"
	"path/filepath"
)

// gstLaunchName is the executable name searched on PATH. The .exe suffix is
// required when probing fallback paths directly via os.Stat (exec.LookPath
// applies PATHEXT itself, but os.Stat does not).
const gstLaunchName = "gst-launch-1.0.exe"

// gstRootEnvVars are the environment variables the GStreamer Windows installer
// sets to point at its install root. The binaries live under "<root>\bin".
// Declared as a var so tests can override it.
var gstRootEnvVars = []string{
	"GSTREAMER_1_0_ROOT_MSVC_X86_64",
	"GSTREAMER_1_0_ROOT_MINGW_X86_64",
	"GSTREAMER_1_0_ROOT_X86_64",
	"GSTREAMER_1_0_ROOT_MSVC_X86",
	"GSTREAMER_1_0_ROOT_MINGW_X86",
}

// gstDefaultRoots are the default install roots used as a backstop when the
// environment variables are unset (e.g. PATH/env not refreshed in the current
// shell after install). Declared as a var so tests can override it.
var gstDefaultRoots = []string{
	`C:\gstreamer\1.0\msvc_x86_64`,
	`C:\gstreamer\1.0\mingw_x86_64`,
	`C:\gstreamer\1.0\x86_64`,
}

// gstLaunchFallbackPaths returns full candidate paths to gst-launch-1.0.exe,
// derived first from the installer's environment variables, then from the
// default install roots (including under %ProgramFiles%).
func gstLaunchFallbackPaths() []string {
	var paths []string

	for _, env := range gstRootEnvVars {
		if root := os.Getenv(env); root != "" {
			paths = append(paths, filepath.Join(root, "bin", gstLaunchName))
		}
	}

	roots := append([]string{}, gstDefaultRoots...)
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		roots = append(roots,
			filepath.Join(pf, "GStreamer", "1.0", "msvc_x86_64"),
			filepath.Join(pf, "GStreamer", "1.0", "mingw_x86_64"),
		)
	}
	for _, root := range roots {
		paths = append(paths, filepath.Join(root, "bin", gstLaunchName))
	}

	return paths
}
