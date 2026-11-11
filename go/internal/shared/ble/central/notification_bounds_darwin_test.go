//go:build darwin && cgo

package central

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDarwinNotificationBoundsAndWakeups(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "notification-bounds-test")
	cmd := exec.CommandContext(ctx, "xcrun", "clang", "-fobjc-arc", "-fblocks", "-UNDEBUG", "-framework", "Foundation", "-framework", "CoreBluetooth",
		"testdata/notification_bounds_test.m", "-o", binary)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile native BLE regression: %v\n%s", err, output)
	}
	if output, err := exec.CommandContext(ctx, binary).CombinedOutput(); err != nil {
		t.Fatalf("native BLE regression: %v\n%s", err, output)
	}
}
