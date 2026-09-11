package scan

import (
	"reflect"
	"testing"
)

func TestParseWinRTLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want BLEDeviceInfo
		ok   bool
	}{
		{
			name: "full record",
			line: `{"address":"AABBCCDDEEFF","name":"Wendy-1234","uuids":["0000180F-0000-1000-8000-00805F9B34FB"],"rssi":-52}`,
			want: BLEDeviceInfo{
				Address:      "AA:BB:CC:DD:EE:FF",
				Name:         "Wendy-1234",
				ServiceUUIDs: []string{"0000180F-0000-1000-8000-00805F9B34FB"},
				RSSI:         -52,
			},
			ok: true,
		},
		{
			// A device that advertises neither a name nor any service UUID is
			// still a valid sighting; Windows has no cached name to fall back on.
			name: "no name and no services",
			line: `{"address":"AABBCCDDEEFF","name":"","uuids":[],"rssi":-70}`,
			want: BLEDeviceInfo{Address: "AA:BB:CC:DD:EE:FF", RSSI: -70},
			ok:   true,
		},
		{
			name: "lowercase address is uppercased",
			line: `{"address":"aabbccddeeff","name":"x","uuids":[],"rssi":-40}`,
			want: BLEDeviceInfo{Address: "AA:BB:CC:DD:EE:FF", Name: "x", RSSI: -40},
			ok:   true,
		},
		{
			name: "several service UUIDs are preserved in order",
			line: `{"address":"010203040506","name":"","uuids":["A","B"],"rssi":-10}`,
			want: BLEDeviceInfo{
				Address:      "01:02:03:04:05:06",
				ServiceUUIDs: []string{"A", "B"},
				RSSI:         -10,
			},
			ok: true,
		},
		{name: "blank line is skipped", line: "", ok: false},
		{name: "whitespace is skipped", line: "   ", ok: false},
		{
			// PowerShell chatter must not fail the scan.
			name: "non-JSON chatter is skipped",
			line: "WARNING: something happened",
			ok:   false,
		},
		{name: "malformed JSON is skipped", line: `{"address":`, ok: false},
		{
			name: "missing address is skipped",
			line: `{"name":"x","uuids":[],"rssi":-40}`,
			ok:   false,
		},
		{
			name: "short address is skipped",
			line: `{"address":"AABBCC","name":"x","uuids":[],"rssi":-40}`,
			ok:   false,
		},
		{
			name: "non-hex address is skipped",
			line: `{"address":"ZZBBCCDDEEFF","name":"x","uuids":[],"rssi":-40}`,
			ok:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseWinRTLine(tt.line)
			if ok != tt.ok {
				t.Fatalf("parseWinRTLine(%q) ok = %v, want %v", tt.line, ok, tt.ok)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseWinRTLine(%q) = %+v, want %+v", tt.line, got, tt.want)
			}
		})
	}
}

func TestFormatBTAddress(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"AABBCCDDEEFF", "AA:BB:CC:DD:EE:FF"},
		{"aabbccddeeff", "AA:BB:CC:DD:EE:FF"},
		{"000000000000", "00:00:00:00:00:00"},
		{"  AABBCCDDEEFF  ", "AA:BB:CC:DD:EE:FF"},
		{"AABBCCDDEE", ""},     // too short
		{"AABBCCDDEEFFAA", ""}, // too long
		{"ZZBBCCDDEEFF", ""},   // not hex
		{"", ""},
	}

	for _, tt := range tests {
		if got := formatBTAddress(tt.in); got != tt.want {
			t.Errorf("formatBTAddress(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
