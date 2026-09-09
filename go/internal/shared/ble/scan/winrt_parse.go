package scan

// This file is intentionally NOT build-tagged for windows. The Windows backend
// cannot run in CI — the test job is Linux-only and only cross-compiles the
// Windows files — so the one part with real parsing logic lives here where the
// Linux test run exercises it.

import (
	"encoding/json"
	"strings"
)

// winrtSighting is one line of the PowerShell scanner's stdout. Address is 12
// hex characters because a WinRT BluetoothAddress is a UInt64 and JSON numbers
// are floats; formatting it to hex in the script sidesteps the question
// entirely.
type winrtSighting struct {
	Address string   `json:"address"`
	Name    string   `json:"name"`
	UUIDs   []string `json:"uuids"`
	RSSI    int      `json:"rssi"`
}

// parseWinRTLine parses one stdout line into a device. It reports ok=false for
// anything unusable — a blank line, a PowerShell warning, a record with no
// address — so the reader can skip noise instead of failing the scan.
func parseWinRTLine(line string) (BLEDeviceInfo, bool) {
	line = strings.TrimSpace(line)
	// Cheap guard so ordinary PowerShell chatter is not fed to the JSON decoder.
	if !strings.HasPrefix(line, "{") {
		return BLEDeviceInfo{}, false
	}

	var s winrtSighting
	if err := json.Unmarshal([]byte(line), &s); err != nil {
		return BLEDeviceInfo{}, false
	}

	address := formatBTAddress(s.Address)
	if address == "" {
		return BLEDeviceInfo{}, false
	}

	uuids := s.UUIDs
	if len(uuids) == 0 {
		// A JSON "[]" decodes to an empty non-nil slice. Normalize it away so a
		// Windows sighting with no services is indistinguishable from the nil
		// the macOS and Linux backends produce for the same thing.
		uuids = nil
	}

	return BLEDeviceInfo{
		Address:      address,
		Name:         s.Name,
		ServiceUUIDs: uuids,
		RSSI:         s.RSSI,
	}, true
}

// formatBTAddress renders 12 hex characters as "AA:BB:CC:DD:EE:FF", the form
// central.Connect parses on Linux and Windows. Anything else yields "".
func formatBTAddress(hex string) string {
	hex = strings.ToUpper(strings.TrimSpace(hex))
	if len(hex) != 12 || !isHex(hex) {
		return ""
	}
	var b strings.Builder
	b.Grow(17)
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hex[i : i+2])
	}
	return b.String()
}
