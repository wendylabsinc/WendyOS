//go:build linux

package commands

import (
	"encoding/json"
	"fmt"
	"testing"
)

// ── flexBool ────────────────────────────────────────────────────────

func TestFlexBool(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"bool true", `true`, true},
		{"bool false", `false`, false},
		{"string 1", `"1"`, true},
		{"string 0", `"0"`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got flexBool
			if err := json.Unmarshal([]byte(tt.input), &got); err != nil {
				t.Fatalf("Unmarshal(%s) error: %v", tt.input, err)
			}
			if bool(got) != tt.want {
				t.Fatalf("Unmarshal(%s) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestFlexBoolRejectsInvalidInput(t *testing.T) {
	var got flexBool
	if err := json.Unmarshal([]byte(`[]`), &got); err == nil {
		t.Fatal("expected error for array input, got nil")
	}
}

// ── lsblk JSON parsing ─────────────────────────────────────────────

func TestParseLsblkOutput(t *testing.T) {
	tests := []struct {
		name          string
		json          string
		wantDevices   int
		wantName      string
		wantRemovable bool
	}{
		{
			name: "string fields (older lsblk)",
			json: `{
				"blockdevices": [{
					"name": "sda",
					"size": "256060514304",
					"type": "disk",
					"rm": "1",
					"hotplug": "1",
					"tran": "usb",
					"mountpoint": null
				}]
			}`,
			wantDevices:   1,
			wantName:      "sda",
			wantRemovable: true,
		},
		{
			name: "bool fields (newer lsblk)",
			json: `{
				"blockdevices": [{
					"name": "sda",
					"size": "256060514304",
					"type": "disk",
					"rm": true,
					"hotplug": true,
					"tran": "usb",
					"mountpoint": null
				}]
			}`,
			wantDevices:   1,
			wantName:      "sda",
			wantRemovable: true,
		},
		{
			name: "non-removable bool",
			json: `{
				"blockdevices": [{
					"name": "nvme0n1",
					"size": "1000204886016",
					"type": "disk",
					"rm": false,
					"hotplug": false,
					"tran": "nvme",
					"mountpoint": null
				}]
			}`,
			wantDevices:   1,
			wantName:      "nvme0n1",
			wantRemovable: false,
		},
		{
			name: "non-removable string",
			json: `{
				"blockdevices": [{
					"name": "nvme0n1",
					"size": "1000204886016",
					"type": "disk",
					"rm": "0",
					"hotplug": "0",
					"tran": "nvme",
					"mountpoint": null
				}]
			}`,
			wantDevices:   1,
			wantName:      "nvme0n1",
			wantRemovable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result lsblkOutput
			if err := json.Unmarshal([]byte(tt.json), &result); err != nil {
				t.Fatalf("Unmarshal error: %v", err)
			}
			if len(result.Blockdevices) != tt.wantDevices {
				t.Fatalf("got %d devices, want %d", len(result.Blockdevices), tt.wantDevices)
			}
			dev := result.Blockdevices[0]
			if dev.Name != tt.wantName {
				t.Fatalf("Name = %q, want %q", dev.Name, tt.wantName)
			}
			if bool(dev.Removable) != tt.wantRemovable {
				t.Fatalf("Removable = %v, want %v", dev.Removable, tt.wantRemovable)
			}
		})
	}
}

// TestParseLsblkChildrenUnmarshaled verifies that the Children field is populated
// when lsblk emits hierarchical JSON (without -l), so that unmountLsblkDevice
// can recurse into nested partitions (fix for bug_2683497e).
func TestParseLsblkChildrenUnmarshaled(t *testing.T) {
	const hierarchical = `{
		"blockdevices": [
			{
				"name": "sdb",
				"size": "16000000000",
				"type": "disk",
				"rm": true,
				"hotplug": true,
				"tran": "usb",
				"mountpoint": null,
				"children": [
					{
						"name": "sdb1",
						"size": "536870912",
						"type": "part",
						"rm": true,
						"hotplug": true,
						"tran": "usb",
						"mountpoint": "/boot/efi"
					},
					{
						"name": "sdb2",
						"size": "15463129088",
						"type": "part",
						"rm": true,
						"hotplug": true,
						"tran": "usb",
						"mountpoint": "/media/user/data"
					}
				]
			}
		]
	}`

	var result lsblkOutput
	if err := json.Unmarshal([]byte(hierarchical), &result); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(result.Blockdevices) != 1 {
		t.Fatalf("got %d top-level devices, want 1", len(result.Blockdevices))
	}
	disk := result.Blockdevices[0]
	if len(disk.Children) != 2 {
		t.Fatalf("got %d children, want 2 — Children field not unmarshaled (this is the root cause of bug_2683497e)", len(disk.Children))
	}
	if disk.Children[0].Name != "sdb1" {
		t.Fatalf("Children[0].Name = %q, want \"sdb1\"", disk.Children[0].Name)
	}
	if disk.Children[1].Mountpoint != "/media/user/data" {
		t.Fatalf("Children[1].Mountpoint = %q, want \"/media/user/data\"", disk.Children[1].Mountpoint)
	}
}

// TestUnmountLsblkDeviceRecursesIntoChildren verifies that unmountLsblkDevice
// attempts to unmount ALL nested child partitions, not just the first one.
// A mock umountCmd records every path that was passed to it, allowing the test
// to assert that both sdb1 and sdb2 were attempted without invoking any
// privileged host commands.
func TestUnmountLsblkDeviceRecursesIntoChildren(t *testing.T) {
	// Install a mock that records calls and returns a simulated error so
	// unmountLsblkDevice behaves as if umount failed (normal in unit tests).
	var attempted []string
	orig := umountCmd
	umountCmd = func(mountpoint string) ([]byte, error) {
		attempted = append(attempted, mountpoint)
		return []byte("mock error"), fmt.Errorf("exit status 1")
	}
	defer func() { umountCmd = orig }()

	parent := lsblkDevice{
		Name:       "sdb",
		Type:       "disk",
		Mountpoint: "", // parent disk itself is not mounted
		Children: []lsblkDevice{
			{
				Name:       "sdb1",
				Type:       "part",
				Mountpoint: "/boot/efi",
			},
			{
				Name:       "sdb2",
				Type:       "part",
				Mountpoint: "/media/user/data",
			},
		},
	}

	err := unmountLsblkDevice(parent)
	if err == nil {
		t.Fatal("expected an error (mock umountCmd always fails), got nil — recursive unmount may not have been attempted")
	}

	// Verify that BOTH child mountpoints were attempted.  We check the
	// recorded attempted slice rather than the error message because the depth
	// sort may cause either child to be processed first, meaning only the
	// deeper one's path ends up in firstErr.
	if len(attempted) != 2 {
		t.Fatalf("expected 2 unmount attempts, got %d (attempted: %v)", len(attempted), attempted)
	}
	wantAttempted := map[string]bool{"/boot/efi": false, "/media/user/data": false}
	for _, p := range attempted {
		if _, ok := wantAttempted[p]; ok {
			wantAttempted[p] = true
		}
	}
	for path, seen := range wantAttempted {
		if !seen {
			t.Errorf("unmount was not attempted for %s (attempted: %v) — recursion stopped after first failure", path, attempted)
		}
	}
}

// TestUnmountDiskCallsLsblkCmd verifies that unmountDisk calls the injected
// lsblkCmd with the target device path and then unmounts the partitions
// returned in the JSON payload via umountCmd.  The -l flag coverage (flat
// output mode) is verified separately by TestLsblkArgsIncludeFlatFlag.
func TestUnmountDiskCallsLsblkCmd(t *testing.T) {
	const fakeJSON = `{
		"blockdevices": [
			{"name": "sdb",  "mountpoint": null},
			{"name": "sdb1", "mountpoint": "/boot/efi"},
			{"name": "sdb2", "mountpoint": "/media/user/data"}
		]
	}`

	// Capture the device path forwarded to lsblkCmd.
	var capturedArgs []string
	origLsblk := lsblkCmd
	lsblkCmd = func(devPath string) ([]byte, error) {
		capturedArgs = append(capturedArgs, devPath)
		return []byte(fakeJSON), nil
	}
	defer func() { lsblkCmd = origLsblk }()

	var unmounted []string
	origUmount := umountCmd
	umountCmd = func(mountpoint string) ([]byte, error) {
		unmounted = append(unmounted, mountpoint)
		return nil, nil // success
	}
	defer func() { umountCmd = origUmount }()

	if err := unmountDisk("/dev/sdb"); err != nil {
		t.Fatalf("unmountDisk returned unexpected error: %v", err)
	}

	// lsblkCmd should have been called with the target device path.
	if len(capturedArgs) == 0 || capturedArgs[0] != "/dev/sdb" {
		t.Fatalf("lsblkCmd called with %v, want [\"/dev/sdb\"]", capturedArgs)
	}

	// Both mounted partitions must have been unmounted by mountpoint.
	wantUnmounted := map[string]bool{"/boot/efi": false, "/media/user/data": false}
	for _, p := range unmounted {
		if _, ok := wantUnmounted[p]; ok {
			wantUnmounted[p] = true
		}
	}
	for path, seen := range wantUnmounted {
		if !seen {
			t.Errorf("expected unmount of %s but it was not attempted (unmounted: %v)", path, unmounted)
		}
	}
}

// TestLsblkArgsIncludeFlatFlag verifies that buildLsblkArgs produces an
// argument list that contains the -l (flat/list) flag.  This test will fail
// if -l is ever removed from buildLsblkArgs, making the primary bug fix
// directly observable without requiring a real lsblk process.
func TestLsblkArgsIncludeFlatFlag(t *testing.T) {
	args := buildLsblkArgs("/dev/sdb")

	// The first element must be the command name.
	if len(args) == 0 {
		t.Fatal("buildLsblkArgs returned empty slice")
	}
	if args[0] != "lsblk" {
		t.Errorf("buildLsblkArgs(%q)[0] = %q, want \"lsblk\"", "/dev/sdb", args[0])
	}

	// The device path must be forwarded as the last argument.
	if args[len(args)-1] != "/dev/sdb" {
		t.Fatalf("buildLsblkArgs(%q) last arg = %q, want \"/dev/sdb\"", "/dev/sdb", args[len(args)-1])
	}

	// The -l flag must be present so lsblk returns a flat list of partitions
	// rather than a nested hierarchy.
	found := false
	for _, a := range args {
		if a == "-l" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("buildLsblkArgs(%q) = %v — missing required -l flag (flat output flag is the primary fix for WDY-774)", "/dev/sdb", args)
	}
}

// TestParseLsblkOutputMatchesLinuxReport reproduces the exact lsblk -J output
// from the bug report (WDY-774) to verify it parses without error.
func TestParseLsblkOutputMatchesLinuxReport(t *testing.T) {
	// Trimmed version of the real output from the bug report: one USB disk
	// (sda) with boolean rm/hotplug fields and one NVMe disk.
	const reported = `{
		"blockdevices": [
			{
				"name": "sda",
				"size": "256060514304",
				"type": "disk",
				"rm": false,
				"hotplug": false,
				"tran": "usb",
				"mountpoint": null
			},
			{
				"name": "nvme2n1",
				"size": "2000398934016",
				"type": "disk",
				"rm": false,
				"hotplug": false,
				"tran": "nvme",
				"mountpoint": null
			}
		]
	}`

	var result lsblkOutput
	if err := json.Unmarshal([]byte(reported), &result); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(result.Blockdevices) != 2 {
		t.Fatalf("got %d devices, want 2", len(result.Blockdevices))
	}
}
