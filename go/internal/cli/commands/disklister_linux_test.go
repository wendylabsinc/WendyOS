//go:build linux

package commands

import (
	"encoding/json"
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
