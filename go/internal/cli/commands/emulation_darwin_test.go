package commands

import (
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// TestHostBuildArchOnThisMac: on Apple silicon the host is arm64 whether the
// test binary is native or an amd64 build under Rosetta
// (GOARCH=amd64 go test ...); on an Intel Mac it is amd64.
func TestHostBuildArchOnThisMac(t *testing.T) {
	appleSilicon, err := unix.SysctlUint32("hw.optional.arm64")
	if err != nil || appleSilicon != 1 {
		if rosettaTranslated() || hostBuildArch() != "amd64" {
			t.Fatalf("Intel Mac: rosetta=%v host=%q, want false and amd64", rosettaTranslated(), hostBuildArch())
		}
		return
	}
	if want := runtime.GOARCH == "amd64"; rosettaTranslated() != want {
		t.Fatalf("rosettaTranslated() = %v for a %s binary on Apple silicon", !want, runtime.GOARCH)
	}
	if got := hostBuildArch(); got != "arm64" {
		t.Fatalf("hostBuildArch() = %q on Apple silicon, want arm64", got)
	}
}
