package scan

import (
	"reflect"
	"testing"
)

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
			if got := CanonicalUUID(tt.in); got != tt.want {
				t.Errorf("CanonicalUUID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCanonicalUUIDCrossPlatformAgreement is the reason this normalization
// exists: the same service spelled the way each platform spells it must
// canonicalize to one value.
func TestCanonicalUUIDCrossPlatformAgreement(t *testing.T) {
	coreBluetooth := CanonicalUUID("180F")                         // macOS 16-bit shorthand
	bluez := CanonicalUUID("0000180f-0000-1000-8000-00805f9b34fb") // BlueZ lowercase
	winrt := CanonicalUUID("0000180F-0000-1000-8000-00805F9B34FB") // WinRT GUID
	if coreBluetooth != bluez || bluez != winrt {
		t.Errorf("platforms disagree after canonicalization: macOS=%q bluez=%q winrt=%q",
			coreBluetooth, bluez, winrt)
	}
}

func TestCanonicalUUIDs(t *testing.T) {
	got := canonicalUUIDs([]string{"180F", "", "0000180f-0000-1000-8000-00805f9b34fb", "180A"})
	want := []string{
		"0000180F-0000-1000-8000-00805F9B34FB",
		"0000180A-0000-1000-8000-00805F9B34FB",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("canonicalUUIDs deduped to %v, want %v", got, want)
	}

	if got := canonicalUUIDs(nil); got != nil {
		t.Errorf("canonicalUUIDs(nil) = %v, want nil", got)
	}
	if got := canonicalUUIDs([]string{"", "  "}); got != nil {
		t.Errorf("canonicalUUIDs of only-empties = %v, want nil", got)
	}
}

func TestMatchesServices(t *testing.T) {
	agent := CanonicalUUID("7565e9eb-4c20-4b67-9272-d708b397b631")
	other := CanonicalUUID("180F")

	tests := []struct {
		name       string
		advertised []string
		want       []string
		matches    bool
	}{
		{
			name:       "empty filter matches everything",
			advertised: nil,
			want:       nil,
			matches:    true,
		},
		{
			name:       "empty filter matches a device with services",
			advertised: []string{agent},
			want:       nil,
			matches:    true,
		},
		{
			name:       "matching service is reported",
			advertised: []string{other, agent},
			want:       []string{agent},
			matches:    true,
		},
		{
			name:       "device advertising nothing is filtered out",
			advertised: nil,
			want:       []string{agent},
			matches:    false,
		},
		{
			name:       "unrelated service is filtered out",
			advertised: []string{other},
			want:       []string{agent},
			matches:    false,
		},
		{
			name:       "any of several wanted services suffices",
			advertised: []string{other},
			want:       []string{agent, other},
			matches:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesServices(tt.advertised, tt.want); got != tt.matches {
				t.Errorf("matchesServices(%v, %v) = %v, want %v",
					tt.advertised, tt.want, got, tt.matches)
			}
		})
	}
}
