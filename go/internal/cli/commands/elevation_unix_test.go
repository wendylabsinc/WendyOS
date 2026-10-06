//go:build darwin || linux

package commands

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestThorElevationDecision(t *testing.T) {
	tests := []struct {
		name        string
		goos        string
		euid        int
		hasUdevRule bool
		interactive bool
		want        thorElevationAction
	}{
		{"root proceeds (darwin)", "darwin", 0, false, true, thorElevationProceed},
		{"root proceeds (linux, no rule)", "linux", 0, false, false, thorElevationProceed},
		{"darwin non-root interactive re-execs", "darwin", 501, false, true, thorElevationReexec},
		{"darwin non-root non-interactive fails early", "darwin", 501, false, false, thorElevationFailEarly},
		{"darwin ignores udev rule", "darwin", 501, true, true, thorElevationReexec},
		{"linux with rule proceeds", "linux", 1000, true, true, thorElevationProceed},
		{"linux no rule interactive re-execs", "linux", 1000, false, true, thorElevationReexec},
		{"linux no rule non-interactive fails early", "linux", 1000, false, false, thorElevationFailEarly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := thorElevationDecision(tt.goos, tt.euid, tt.hasUdevRule, tt.interactive)
			if got != tt.want {
				t.Errorf("thorElevationDecision(%q,%d,%v,%v) = %v, want %v",
					tt.goos, tt.euid, tt.hasUdevRule, tt.interactive, got, tt.want)
			}
		})
	}
}

func TestHasWendyJetsonUdevRule(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "70-wendy-jetson.rules")
	if err := os.WriteFile(present, []byte("rule"), 0o644); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "does-not-exist.rules")

	if !hasWendyJetsonUdevRule([]string{absent, present}) {
		t.Error("want true when at least one rule path exists")
	}
	if hasWendyJetsonUdevRule([]string{absent}) {
		t.Error("want false when no rule path exists")
	}
	if hasWendyJetsonUdevRule(nil) {
		t.Error("want false for empty path list")
	}
}

func TestHasDeviceTypeFlag(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"install", "--device-type", "jetson-agx-thor"}, true},
		{[]string{"install", "--device-type=jetson-agx-thor"}, true},
		{[]string{"install", "--nightly"}, false},
		{nil, false},
	}
	for _, tt := range tests {
		if got := hasDeviceTypeFlag(tt.args); got != tt.want {
			t.Errorf("hasDeviceTypeFlag(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

func TestErrThorNeedsRoot(t *testing.T) {
	err := errThorNeedsRoot()
	if err == nil {
		t.Fatal("errThorNeedsRoot() must return a non-nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "administrator access") {
		t.Errorf("error should explain the privilege need: %q", msg)
	}
	if !strings.Contains(msg, "sudo wendy install --device-type "+thorDeviceType) {
		t.Errorf("error should give the exact re-run command: %q", msg)
	}
	if strings.Contains(msg, "usb-setup") {
		t.Errorf("error must not point at usb-setup, which does not install the Jetson rule: %q", msg)
	}
	if runtime.GOOS == "linux" && !strings.Contains(msg, usbUdevRulePath) {
		t.Errorf("linux error should give the udev rule install commands: %q", msg)
	}
}

func TestThorElevationReason(t *testing.T) {
	if !strings.Contains(thorElevationReason("darwin"), "root on macOS") {
		t.Errorf("darwin reason should mention macOS root: %q", thorElevationReason("darwin"))
	}
	linux := thorElevationReason("linux")
	for _, want := range []string{usbUdevRulePath, usbUdevRule, "udevadm control --reload-rules"} {
		if !strings.Contains(linux, want) {
			t.Errorf("linux reason should give the udev rule install commands (missing %q): %q", want, linux)
		}
	}
	// `wendy device usb-setup` configures the USB-C network link, not the
	// Jetson recovery rule, so the hint must not send people there.
	if strings.Contains(linux, "usb-setup") {
		t.Errorf("linux reason must not point at usb-setup: %q", linux)
	}
}

func TestBuildSudoReexecArgs(t *testing.T) {
	self := "/usr/local/bin/wendy"

	// Interactive picker case: no --device-type in original args -> inject it.
	got := buildSudoReexecArgs(self, []string{"install", "--nightly"})
	if got[0] != thorSudoPreserveEnv {
		t.Errorf("arg[0] = %q, want preserve-env %q", got[0], thorSudoPreserveEnv)
	}
	if got[1] != self {
		t.Errorf("arg[1] = %q, want self %q", got[1], self)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "install --nightly") {
		t.Errorf("original args not preserved: %v", got)
	}
	if !strings.Contains(joined, "--device-type "+thorDeviceType) {
		t.Errorf("device-type not injected: %v", got)
	}

	// Flag case: --device-type already present -> do not duplicate.
	got = buildSudoReexecArgs(self, []string{"install", "--device-type", thorDeviceType, "--force"})
	if n := strings.Count(strings.Join(got, " "), thorDeviceType); n != 1 {
		t.Errorf("device-type duplicated (%d occurrences): %v", n, got)
	}

	// Equals form is also recognized as present.
	got = buildSudoReexecArgs(self, []string{"install", "--device-type=" + thorDeviceType})
	if n := strings.Count(strings.Join(got, " "), "--device-type"); n != 1 {
		t.Errorf("device-type duplicated for equals form: %v", got)
	}
}

// sudo's env_reset drops variables it isn't told to keep. Losing
// WENDY_ANALYTICS made the elevated run treat an opted-out user as a first run:
// it sent analytics and wrote a root-owned ~/.wendy/config.json. Losing
// WENDY_CONFIG_DIR sent its writes to $HOME/.wendy instead.
func TestThorSudoPreserveEnvKeepsWendySettings(t *testing.T) {
	kept := strings.Split(strings.TrimPrefix(thorSudoPreserveEnv, "--preserve-env="), ",")
	for _, want := range []string{"WENDY_ANALYTICS", "WENDY_CONFIG_DIR"} {
		found := false
		for _, k := range kept {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not preserved through sudo: %q", want, thorSudoPreserveEnv)
		}
	}
}

// The elevated run hands what it creates back to the user when it exits, but
// not if it's killed. The directories it writes into are created beforehand, as
// the user, so a killed flash can't leave ~/.cache itself root-owned.
func TestCreateElevatedRunDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}

	createElevatedRunDirs()

	for _, dir := range []string{filepath.Join(base, "wendy", "os-images"), filepath.Join(base, "wendy", "logs")} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Errorf("%s was not created before elevating: %v", dir, err)
		}
	}
}

func TestPinCacheDirEnv(t *testing.T) {
	base := "/home/alice/.cache"

	// XDG_CACHE_HOME absent -> appended.
	got := pinCacheDirEnv([]string{"HOME=/home/alice", "PATH=/usr/bin"}, base)
	if !containsEnv(got, "XDG_CACHE_HOME="+base) {
		t.Errorf("XDG_CACHE_HOME not pinned when absent: %v", got)
	}
	if !containsEnv(got, "HOME=/home/alice") || !containsEnv(got, "PATH=/usr/bin") {
		t.Errorf("existing env vars not preserved: %v", got)
	}

	// XDG_CACHE_HOME present -> overridden, exactly once.
	got = pinCacheDirEnv([]string{"XDG_CACHE_HOME=/old", "HOME=/home/alice"}, base)
	n := 0
	for _, e := range got {
		if strings.HasPrefix(e, "XDG_CACHE_HOME=") {
			n++
			if e != "XDG_CACHE_HOME="+base {
				t.Errorf("XDG_CACHE_HOME not overridden: %q", e)
			}
		}
	}
	if n != 1 {
		t.Errorf("want exactly one XDG_CACHE_HOME entry, got %d: %v", n, got)
	}
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
