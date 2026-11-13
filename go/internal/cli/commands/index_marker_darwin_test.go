//go:build darwin

package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMarkFATVolumes_DiskImage runs the marker code against a two-volume FAT
// card image and checks Spotlight honours the result after a re-attach.
// Needs Spotlight enabled:
//
//	WENDY_E2E_DISKIMAGE=1 go test ./internal/cli/commands -run DiskImage -v
func TestMarkFATVolumes_DiskImage(t *testing.T) {
	if os.Getenv("WENDY_E2E_DISKIMAGE") == "" {
		t.Skip("set WENDY_E2E_DISKIMAGE=1 to attach a disk image")
	}

	t.Run("after flash", func(t *testing.T) {
		img, disk := newCardImage(t)
		t.Run("system tools ignore PATH", func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			if err := markFATVolumesUnindexed(drive{DevicePath: disk}); err != nil {
				t.Fatalf("markFATVolumesUnindexed: %v", err)
			}
		})
		assertMarked(t, waitFATMounts(t, disk))

		detachImage(t, disk)
		disk = attachImage(t, img, false)
		for _, mp := range waitFATMounts(t, disk) {
			out, _ := exec.Command("mdutil", "-s", mp).CombinedOutput()
			if !strings.Contains(string(out), "Indexing and searching disabled") {
				t.Errorf("%s after re-attach: %s", mp, strings.TrimSpace(string(out)))
			}
		}
	})
}

// newCardImage attaches a fresh 256 MiB raw image partitioned like a card: two
// FAT32 volumes, both left mounted.
func newCardImage(t *testing.T) (img, disk string) {
	t.Helper()
	img = filepath.Join(t.TempDir(), "card.img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(256 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	disk = attachImage(t, img, true)
	if out, err := exec.Command("diskutil", "partitionDisk", disk, "GPT", "FAT32", "BOOT", "128M", "FAT32", "CONFIG", "R").CombinedOutput(); err != nil {
		t.Fatalf("partitionDisk: %v\n%s", err, out)
	}
	return img, disk
}

func attachImage(t *testing.T, img string, nomount bool) string {
	t.Helper()
	args := []string{"attach", "-imagekey", "diskimage-class=CRawDiskImage", img}
	if nomount {
		args = append(args, "-nomount")
	}
	out, err := exec.Command("hdiutil", args...).Output()
	if err != nil {
		t.Fatalf("hdiutil attach: %v", err)
	}
	disk := strings.Fields(string(out))[0]
	t.Cleanup(func() { exec.Command("hdiutil", "detach", "-force", disk).Run() })
	return disk
}

func detachImage(t *testing.T, disk string) {
	t.Helper()
	if out, err := exec.Command("hdiutil", "detach", disk).CombinedOutput(); err != nil {
		t.Fatalf("hdiutil detach: %v\n%s", err, out)
	}
}

func waitFATMounts(t *testing.T, disk string) []string {
	t.Helper()
	for i := 0; i < 30; i++ {
		out, _ := exec.Command("/sbin/mount").Output()
		if mps := parseMountedFAT(string(out), disk); len(mps) == 2 {
			return mps
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s: FAT volumes never mounted", disk)
	return nil
}

func assertMarked(t *testing.T, mps []string) {
	t.Helper()
	for _, mp := range mps {
		if _, err := os.Stat(filepath.Join(mp, spotlightMarker)); err != nil {
			t.Errorf("%s: %v", mp, err)
		}
	}
}
