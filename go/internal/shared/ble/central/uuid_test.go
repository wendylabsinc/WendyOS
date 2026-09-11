package central

import "testing"

// The table below is scan/uuid_test.go's, verbatim. canonicalUUID is a copy of
// scan.CanonicalUUID and the two must not drift: a UUID that passes the scan
// filter has to match the service discovery finds.
func TestCanonicalUUID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// CoreBluetooth's CBUUID.UUIDString reports a 16-bit UUID as four
			// hex characters; BlueZ reports the same service in full form.
			name: "16-bit expands against the base UUID",
			in:   "180F",
			want: "0000180F-0000-1000-8000-00805F9B34FB",
		},
		{
			name: "16-bit lowercase is uppercased",
			in:   "180f",
			want: "0000180F-0000-1000-8000-00805F9B34FB",
		},
		{
			name: "32-bit expands against the base UUID",
			in:   "0000180F",
			want: "0000180F-0000-1000-8000-00805F9B34FB",
		},
		{
			name: "full 128-bit lowercase is uppercased",
			in:   "7565e9eb-4c20-4b67-9272-d708b397b631",
			want: "7565E9EB-4C20-4B67-9272-D708B397B631",
		},
		{
			name: "full 128-bit uppercase is unchanged",
			in:   "7565E9EB-4C20-4B67-9272-D708B397B631",
			want: "7565E9EB-4C20-4B67-9272-D708B397B631",
		},
		{
			name: "surrounding whitespace is trimmed",
			in:   "  180F  ",
			want: "0000180F-0000-1000-8000-00805F9B34FB",
		},
		{
			name: "registry-style braces are stripped",
			in:   "{7565e9eb-4c20-4b67-9272-d708b397b631}",
			want: "7565E9EB-4C20-4B67-9272-D708B397B631",
		},
		{
			name: "dashless 128-bit form gains dashes",
			in:   "7565e9eb4c204b679272d708b397b631",
			want: "7565E9EB-4C20-4B67-9272-D708B397B631",
		},
		{
			// Not a UUID at all: uppercased and passed through, so it can only
			// ever match itself rather than being silently dropped.
			name: "unrecognized input degrades to exact match",
			in:   "not-a-uuid",
			want: "NOT-A-UUID",
		},
		{
			name: "non-hex of UUID-ish length is left alone",
			in:   "ZZZZ",
			want: "ZZZZ",
		},
		{
			name: "empty stays empty",
			in:   "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalUUID(tt.in); got != tt.want {
				t.Errorf("canonicalUUID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCanonicalUUIDWendyServices covers the shape this package actually sees:
// callers pass the Wendy UUIDs uppercase, BlueZ hands them back lowercase, and
// the characteristic lookup compares the two.
func TestCanonicalUUIDWendyServices(t *testing.T) {
	const (
		fromCaller = "4E57454E-4459-0002-0000-000000000000"
		fromBlueZ  = "4e57454e-4459-0002-0000-000000000000"
	)
	if canonicalUUID(fromCaller) != canonicalUUID(fromBlueZ) {
		t.Errorf("caller spelling %q and BlueZ spelling %q disagree after canonicalization",
			canonicalUUID(fromCaller), canonicalUUID(fromBlueZ))
	}
}

// TestCanonicalUUIDCrossPlatformAgreement is the reason this normalization
// exists: the same service spelled the way each platform spells it must
// canonicalize to one value.
func TestCanonicalUUIDCrossPlatformAgreement(t *testing.T) {
	coreBluetooth := canonicalUUID("180F")                         // macOS 16-bit shorthand
	bluez := canonicalUUID("0000180f-0000-1000-8000-00805f9b34fb") // BlueZ lowercase
	winrt := canonicalUUID("0000180F-0000-1000-8000-00805F9B34FB") // WinRT GUID
	if coreBluetooth != bluez || bluez != winrt {
		t.Errorf("platforms disagree after canonicalization: macOS=%q bluez=%q winrt=%q",
			coreBluetooth, bluez, winrt)
	}
}
