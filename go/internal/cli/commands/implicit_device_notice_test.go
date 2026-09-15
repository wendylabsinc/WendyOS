package commands

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestImplicitDeviceLinesNamesTheDevice(t *testing.T) {
	lines := implicitDeviceLines("thor.local", implicitDefaultDevice, false)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1 without the hint: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "thor.local") {
		t.Errorf("line = %q, want it to name the device", lines[0])
	}
	if !strings.Contains(lines[0], "default device") {
		t.Errorf("line = %q, want it to say the default was used", lines[0])
	}
}

// The cloud path never consults the configured default, so calling its choice a
// "default device" would tell the user something untrue.
func TestImplicitDeviceLinesDoesNotCallSoleCloudDeviceADefault(t *testing.T) {
	lines := implicitDeviceLines("thor.local", implicitSoleCloudDevice, false)
	if strings.Contains(lines[0], "default") {
		t.Errorf("line = %q, must not describe a sole cloud device as a default", lines[0])
	}
	if !strings.Contains(lines[0], "only device currently online") {
		t.Errorf("line = %q, want it to say the choice was among ONLINE devices, not enrolled ones", lines[0])
	}
	if strings.Contains(lines[0], "enrolled") {
		t.Errorf("line = %q, must not claim the org has only one enrolled device (offline devices are hidden)", lines[0])
	}
}

func TestImplicitDeviceHintForSoleCloudDevicePointsAtOfflineDevices(t *testing.T) {
	lines := implicitDeviceLines("thor.local", implicitSoleCloudDevice, true)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want the device line plus a hint", len(lines))
	}
	hint := lines[1]
	if !strings.Contains(hint, "--device") || !strings.Contains(hint, "wendy cloud discover --all") {
		t.Errorf("hint = %q, want --device and 'wendy cloud discover --all'", hint)
	}
	if strings.Contains(hint, "set-default") {
		t.Errorf("hint = %q, must not suggest set-default: the cloud path never consults it", hint)
	}
}

func TestImplicitDeviceLinesHintIsSeparateAndOptional(t *testing.T) {
	with := implicitDeviceLines("thor.local", implicitDefaultDevice, true)
	if len(with) != 2 {
		t.Fatalf("got %d lines, want 2 with the hint: %q", len(with), with)
	}
	if !strings.Contains(with[1], "--device") {
		t.Errorf("hint = %q, want it to mention the override flag", with[1])
	}
	if !strings.Contains(with[1], "wendy device set-default") {
		t.Errorf("hint = %q, want it to mention how to change the default", with[1])
	}
}

// The identity line must never be throttled away: knowing which device was
// touched is the entire point, so only the explanation is rate-limited.
func TestImplicitDeviceLinesAlwaysIncludesIdentityLine(t *testing.T) {
	for _, withHint := range []bool{true, false} {
		lines := implicitDeviceLines("thor.local", implicitDefaultDevice, withHint)
		if len(lines) == 0 || !strings.Contains(lines[0], "thor.local") {
			t.Errorf("withHint=%v: identity line missing from %q", withHint, lines)
		}
	}
}

func TestNoteImplicitDeviceSuppressedInJSONMode(t *testing.T) {
	origJSON, origNoticed := jsonOutput, noticedImplicitDevice
	t.Cleanup(func() { jsonOutput, noticedImplicitDevice = origJSON, origNoticed })

	jsonOutput = true
	noticedImplicitDevice = false
	noteImplicitDevice("thor.local", implicitDefaultDevice)
	if noticedImplicitDevice {
		t.Error("notice was emitted in JSON mode; machine-readable output must stay clean")
	}
}

// connectResolvedAgent runs again on the retry paths in connectToAgent, so
// without the guard one command could announce the same device several times.
func TestNoteImplicitDeviceOnlyFiresOncePerInvocation(t *testing.T) {
	origJSON, origNoticed := jsonOutput, noticedImplicitDevice
	t.Cleanup(func() { jsonOutput, noticedImplicitDevice = origJSON, origNoticed })

	jsonOutput = true // keep the test quiet; the guard is what is under test
	noticedImplicitDevice = false
	noteImplicitDevice("thor.local", implicitDefaultDevice)
	noticedImplicitDevice = true
	before := noticedImplicitDevice
	noteImplicitDevice("other.local", implicitDefaultDevice)
	if noticedImplicitDevice != before {
		t.Error("guard did not hold across repeated calls")
	}
}

func TestNoteImplicitDeviceIgnoresEmptyName(t *testing.T) {
	origNoticed := noticedImplicitDevice
	t.Cleanup(func() { noticedImplicitDevice = origNoticed })

	noticedImplicitDevice = false
	noteImplicitDevice("", implicitDefaultDevice)
	if noticedImplicitDevice {
		t.Error("announced a device with no name")
	}
}

// resolveCloudGRPCFlag must prefer whichever --cloud-grpc definition is in
// effect on the command being run, since enroll/unenroll/rename shadow the
// group's persistent flag with their own.
func TestResolveCloudGRPCFlagPrefersTheExecutingCommandsFlag(t *testing.T) {
	cmd := &cobra.Command{Use: "unenroll"}
	var local string
	cmd.Flags().StringVar(&local, "cloud-grpc", "", "")
	if err := cmd.Flags().Set("cloud-grpc", "local.example:443"); err != nil {
		t.Fatalf("setting flag: %v", err)
	}

	if got := resolveCloudGRPCFlag(cmd, "persistent.example:443"); got != "local.example:443" {
		t.Errorf("resolveCloudGRPCFlag = %q, want the shadowing local value", got)
	}
}

func TestResolveCloudGRPCFlagFallsBackWhenUnset(t *testing.T) {
	cmd := &cobra.Command{Use: "info"}
	if got := resolveCloudGRPCFlag(cmd, "persistent.example:443"); got != "persistent.example:443" {
		t.Errorf("resolveCloudGRPCFlag = %q, want the group's persistent value", got)
	}
	if got := resolveCloudGRPCFlag(nil, "persistent.example:443"); got != "persistent.example:443" {
		t.Errorf("resolveCloudGRPCFlag(nil) = %q, want the fallback", got)
	}
}
