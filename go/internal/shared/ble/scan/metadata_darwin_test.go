//go:build darwin

package scan

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDarwinRawAdvertisementBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "metadata-bridge")
	cmd := exec.CommandContext(ctx, "clang", "-fobjc-arc", "-framework", "CoreBluetooth", "-framework", "Foundation", "testdata/metadata_bridge_test.m", "-o", binary)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile: %v\n%s", err, output)
	}
	if output, err := exec.CommandContext(ctx, binary).CombinedOutput(); err != nil {
		t.Fatalf("bridge: %v\n%s", err, output)
	}
}
