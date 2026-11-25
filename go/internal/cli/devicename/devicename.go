// Package devicename holds the device name rule shared by the terminal
// installer and install jobs.
package devicename

import "fmt"

// MaxLen keeps "wendyos-<name>", the hostname the agent derives, within a
// 63-octet DNS label.
const MaxLen = 55

// Validate reports why name cannot be a device name, or nil.
func Validate(name string) error {
	if len(name) < 3 || len(name) > MaxLen {
		return fmt.Errorf("device name must be 3–%d characters", MaxLen)
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z':
		case (c >= '0' && c <= '9') || c == '-':
			if i == 0 {
				return fmt.Errorf("device name must start with a lowercase letter")
			}
		default:
			return fmt.Errorf("device name may only contain lowercase letters, digits, and hyphens")
		}
	}
	if name[len(name)-1] == '-' {
		return fmt.Errorf("device name must not end with a hyphen")
	}
	return nil
}
