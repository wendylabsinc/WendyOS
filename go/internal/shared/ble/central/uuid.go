package central

import "strings"

// bluetoothBaseUUID is the template every 16- and 32-bit Bluetooth UUID
// expands against: the short value occupies the first group and the rest is
// fixed (Core Specification, Vol 3, Part B, 2.5.1).
const bluetoothBaseUUID = "00000000-0000-1000-8000-00805F9B34FB"

// canonicalUUID normalizes a service or characteristic UUID to uppercase
// 128-bit form, expanding the 16-bit ("180F") and 32-bit ("0000180F")
// shorthands against the Bluetooth base UUID. Input that is not a recognizable
// UUID is returned uppercased and otherwise untouched, so an unexpected
// spelling degrades to an exact-match comparison rather than being silently
// dropped.
//
// Both sides of every comparison go through this. BlueZ reports full lowercase
// 128-bit while callers pass uppercase, so raw string comparison would miss
// every match on Linux.
//
// This is a logic copy of scan.CanonicalUUID and must produce identical
// output for identical input: central deliberately does not import scan
// (that would invert the address-producer/consumer relationship and pull
// scan's cgo into central's build graph), but a UUID that passes the scan
// filter has to match what service discovery finds. The two test tables are
// kept in sync for that reason.
//
// This file carries no build tag so its test runs on every platform.
func canonicalUUID(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")

	switch len(s) {
	case 4, 8:
		if !isHexUUID(s) {
			return s
		}
		// Left-pad a 16-bit value to the base UUID's 8-character first group.
		return strings.Repeat("0", 8-len(s)) + s + bluetoothBaseUUID[8:]
	case 36:
		return s
	case 32:
		// Dashless 128-bit form.
		if !isHexUUID(s) {
			return s
		}
		return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
	default:
		return s
	}
}

func isHexUUID(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
