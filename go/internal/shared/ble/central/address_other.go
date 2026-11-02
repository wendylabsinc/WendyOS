//go:build !darwin

package central

// ValidateL2CAPAddress leaves LE address validation to the platform transport.
// In particular, Linux hardware MAC addresses must not be rejected as UUIDs.
func ValidateL2CAPAddress(string) error { return nil }
