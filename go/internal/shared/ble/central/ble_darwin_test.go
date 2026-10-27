//go:build darwin && cgo

package central

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestL2CAPNativeWriteBackpressure(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "l2cap-write-test")
	cmd := exec.Command("clang", "-fobjc-arc", "-framework", "Foundation", "-framework", "CoreBluetooth",
		"testdata/l2cap_write_test.m", "-o", binary)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile native BLE regression: %v\n%s", err, output)
	}
	if output, err := exec.Command(binary).CombinedOutput(); err != nil {
		t.Fatalf("native BLE regression: %v\n%s", err, output)
	}
}
