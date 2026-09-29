package rcm

import (
	"errors"
	"strings"
	"testing"
)

func TestVerifyChipIDRequiresTheObservedChipOnClaimedHandle(t *testing.T) {
	read := func() (string, error) { return "ab12CD34", nil }
	if err := VerifyChipID("AB12cd34", read); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"11223344", "7&WINDOWS-LOCATION"} {
		if err := VerifyChipID(expected, read); err == nil {
			t.Fatalf("accepted different/invalid chip %q", expected)
		}
	}
	if err := VerifyChipID("ab12cd34", func() (string, error) { return "", nil }); err == nil {
		t.Fatal("accepted missing claimed-handle identity")
	}
	if err := VerifyChipID("ab12cd34", func() (string, error) { return "", errors.New("USB gone") }); err == nil {
		t.Fatal("accepted unreadable claimed-handle identity")
	}
	if err := VerifyChipID("", func() (string, error) { t.Fatal("optional pin changed existing interactive behavior"); return "", nil }); err != nil {
		t.Fatal(err)
	}
}

// The T234 family enumerates one recovery PID per module SKU (0x7<module>23).
// Detection must accept all of them — matching only the AGX Orin PID made an
// Orin Nano in recovery mode invisible to the CLI (WDY-1888).
func TestT234FamilyRecoveryPIDs(t *testing.T) {
	tests := []struct {
		pid       uint16
		module    string
		agx, nano bool
	}{
		{ProductOrinAGX32, "AGX Orin 32GB", true, false},
		{ProductOrinAGX64, "AGX Orin 64GB", true, false},
		{ProductOrinNX16, "Orin NX 16GB", false, false},
		{ProductOrinNX8, "Orin NX 8GB", false, false},
		{ProductOrinNano8, "Orin Nano 8GB", false, true},
		{ProductOrinNano4, "Orin Nano 4GB", false, true},
	}
	for _, tc := range tests {
		d := RecoveryDevice{Product: tc.pid}
		if !IsT234RecoveryPID(tc.pid) || !d.IsOrin() {
			t.Errorf("PID 0x%04x not recognized as T234", tc.pid)
		}
		if d.IsThor() {
			t.Errorf("PID 0x%04x misclassified as Thor", tc.pid)
		}
		if d.IsOrinAGX() != tc.agx || d.IsOrinNano() != tc.nano {
			t.Errorf("PID 0x%04x: IsOrinAGX=%v IsOrinNano=%v, want %v/%v", tc.pid, d.IsOrinAGX(), d.IsOrinNano(), tc.agx, tc.nano)
		}
		if got := d.Describe(); !strings.Contains(got, tc.module) {
			t.Errorf("Describe() for PID 0x%04x = %q, want module %q", tc.pid, got, tc.module)
		}
	}
}

func TestNonT234PIDsRejected(t *testing.T) {
	thor := RecoveryDevice{Product: ProductThor}
	if thor.IsOrin() || thor.IsOrinAGX() || thor.IsOrinNano() || !thor.IsThor() {
		t.Errorf("Thor PID misclassified: %+v", thor)
	}
	if IsT234RecoveryPID(ProductThor) || IsT234RecoveryPID(0x7123) || IsT234RecoveryPID(0x0104) {
		t.Error("non-T234 PID accepted as T234 recovery PID")
	}
}
