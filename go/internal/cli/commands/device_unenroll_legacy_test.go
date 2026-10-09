package commands

import "testing"

func TestUnenrollPreservesNumericCloudV1Routing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		org, asset int32
		legacy     bool
	}{
		{"legacy", 55, 277, true},
		{"minimum valid numeric IDs", 1, 1, true},
		{"UUID enrollment", 0, 0, false},
		{"missing organization", 0, 277, false},
		{"missing asset", 55, 0, false},
		{"invalid organization", -1, 277, false},
		{"invalid asset", 55, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLegacyCloudEnrollment(tc.org, tc.asset); got != tc.legacy {
				t.Fatalf("legacy routing = %v, want %v", got, tc.legacy)
			}
		})
	}
}
