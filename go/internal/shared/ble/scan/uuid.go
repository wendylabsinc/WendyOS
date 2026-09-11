package scan

import "strings"

// bluetoothBaseUUID is the template every 16- and 32-bit Bluetooth UUID
// expands against: the short value occupies the first group and the rest is
// fixed (Core Specification, Vol 3, Part B, 2.5.1).
const bluetoothBaseUUID = "00000000-0000-1000-8000-00805F9B34FB"

// CanonicalUUID normalizes a service UUID to uppercase 128-bit form, expanding
// the 16-bit ("180F") and 32-bit ("0000180F") shorthands against the Bluetooth
// base UUID. Input that is not a recognizable UUID is returned uppercased and
// otherwise untouched, so an unexpected spelling degrades to an exact-match
// comparison rather than being silently dropped.
//
// Normalizing matters because the platforms disagree on spelling for the very
// same service: BlueZ reports full lowercase 128-bit, CoreBluetooth's
// CBUUID.UUIDString returns four hex characters for a 16-bit UUID and full
// form otherwise, and WinRT returns full uppercase GUIDs. Comparing raw
// strings across them would miss matches.
func CanonicalUUID(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")

	switch len(s) {
	case 4, 8:
		if !isHex(s) {
			return s
		}
		// Left-pad a 16-bit value to the base UUID's 8-character first group.
		return strings.Repeat("0", 8-len(s)) + s + bluetoothBaseUUID[8:]
	case 36:
		return s
	case 32:
		// Dashless 128-bit form.
		if !isHex(s) {
			return s
		}
		return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
	default:
		return s
	}
}

// canonicalUUIDs normalizes a list, dropping empties and duplicates while
// preserving the order the platform reported.
func canonicalUUIDs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, u := range in {
		c := CanonicalUUID(u)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// matchesServices reports whether a device advertising the given UUIDs should
// be reported for a scan filtered on want. An empty want matches everything —
// that is how a caller asks for every device in range. Both sides are assumed
// already canonical.
func matchesServices(advertised, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for _, w := range want {
		for _, a := range advertised {
			if a == w {
				return true
			}
		}
	}
	return false
}

func isHex(s string) bool {
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
