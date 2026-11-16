//go:build darwin

package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestRelocateGPTDarwinRawDiskImage(t *testing.T) {
	if os.Getenv("WENDY_E2E_DISKIMAGE") != "1" {
		t.Skip("set WENDY_E2E_DISKIMAGE=1 to attach a disposable disk image")
	}
	const size = 32 << 20
	image := buildTestGPTImage(t, 512, jetsonLikeSectors, jetsonLikeParts)
	path := filepath.Join(t.TempDir(), "gpt.img")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
	disk := attachImage(t, path, true)
	if !strings.HasPrefix(disk, "/dev/disk") {
		t.Fatalf("unexpected attached image device %q", disk)
	}
	raw := "/dev/r" + strings.TrimPrefix(disk, "/dev/")
	device, err := os.OpenFile(raw, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	// Record the real kernel behavior that the pure ENOTTY fixture models.
	if err := device.Sync(); !errors.Is(err, syscall.ENOTTY) {
		t.Fatalf("expected raw-device ENOTTY regression path, got %v", err)
	}
	if err := relocateGPTOnDevice(device); err != nil {
		t.Fatalf("raw-device rewrite failed: %v", err)
	}
	actual := make([]byte, size)
	if _, err := device.ReadAt(actual, 0); err != nil {
		t.Fatal(err)
	}
	verifyTestGPT(t, actual, 512, jetsonLikeParts)
	requirePartitionData(t, actual, image, 512, jetsonLikeParts)
}
