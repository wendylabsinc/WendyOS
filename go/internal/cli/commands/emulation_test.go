package commands

import (
	"strings"
	"sync"
	"testing"
)

func TestNativeHostArch(t *testing.T) {
	for _, tc := range []struct {
		goarch  string
		rosetta bool
		want    string
	}{
		{"amd64", false, "amd64"},
		{"amd64", true, "arm64"}, // amd64 CLI under Rosetta on Apple silicon
		{"arm64", false, "arm64"},
		{"arm64", true, "arm64"},
	} {
		if got := nativeHostArch(tc.goarch, tc.rosetta); got != tc.want {
			t.Errorf("nativeHostArch(%q, %v) = %q, want %q", tc.goarch, tc.rosetta, got, tc.want)
		}
	}
}

func TestEmulatedBuildNotice(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hostOS   string
		host     string
		platform string
		want     []string // substrings; nil means no notice
	}{
		{"x86 host, arm64 device", "linux", "amd64", "linux/arm64", []string{"RUN steps for linux/arm64", "amd64 machine", "CPU emulation (typically QEMU)", "usually much slower than a native build", "--build-host=DEVICE", "WendyOS device with a native arm64 CPU"}},
		{"x86_64 spelling", "linux", "x86_64", "linux/arm64", []string{"CPU emulation"}},
		{"Apple silicon, x86 device", "darwin", "arm64", "linux/amd64", []string{"RUN steps for linux/amd64", "arm64 machine", "native amd64 CPU"}},
		{"arm64 Linux host, 32-bit arm device: compat mode runs it natively, no QEMU", "linux", "arm64", "linux/arm/v7", nil},
		{"arm64 Windows host, 32-bit arm device: not Apple silicon, no QEMU", "windows", "arm64", "linux/arm/v7", nil},
		// An arm64 WendyOS device (a Pi 4 or 5, an Orin) builds arm/v7
		// natively, so the hint must not ask for a 32-bit device.
		{"Apple silicon, 32-bit arm device: no AArch32 support, really QEMU", "darwin", "arm64", "linux/arm/v7", []string{"RUN steps for linux/arm/v7", "native arm64 or 32-bit arm CPU"}},
		{"native arm64", "linux", "arm64", "linux/arm64", nil},
		{"native arm64 with variant", "linux", "arm64", "linux/arm64/v8", nil},
		{"native aarch64 spelling", "linux", "arm64", "linux/aarch64", nil},
		{"native amd64, x86_64 spelling", "linux", "amd64", "linux/x86_64", nil},
		{"no architecture in the platform", "linux", "amd64", "linux", nil},
		{"unknown host", "linux", "", "linux/arm64", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := emulatedBuildNotice(tc.hostOS, tc.host, tc.platform, true)
			if tc.want == nil {
				if got != "" {
					t.Fatalf("unexpected notice %q", got)
				}
				return
			}
			for _, s := range tc.want {
				if !strings.Contains(got, s) {
					t.Fatalf("notice %q lacks %q", got, s)
				}
			}
			if strings.Contains(got, "\n") {
				t.Fatalf("notice %q spans more than one line", got)
			}
		})
	}

	// Multi-service and Compose projects cannot use --build-host yet, so the
	// notice must not tell them to.
	got := emulatedBuildNotice("linux", "amd64", "linux/arm64", false)
	if strings.Contains(got, "--build-host=DEVICE") || !strings.Contains(got, "single-service projects only") {
		t.Fatalf("group notice %q", got)
	}
}

// TestNoteEmulatedBuildPrintsOnce: `wendy watch` and multi-service groups
// build many times per process; the notice appears once, and a native build
// first does not use it up.
func TestNoteEmulatedBuildPrintsOnce(t *testing.T) {
	var once sync.Once
	var printed []string
	print := func(msg string) { printed = append(printed, msg) }

	noteEmulatedBuildWith(&once, "linux", "amd64", "linux/amd64", true, print)
	if len(printed) != 0 {
		t.Fatalf("a native build printed %q", printed)
	}
	for range 3 {
		noteEmulatedBuildWith(&once, "linux", "amd64", "linux/arm64", true, print)
	}
	if len(printed) != 1 {
		t.Fatalf("printed %d notices, want 1: %q", len(printed), printed)
	}
}
