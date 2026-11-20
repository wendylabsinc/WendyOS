//go:build darwin

package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		card := newCardImage(t)
		t.Run("system tools ignore PATH", func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			if err := markFATVolumesUnindexed(drive{DevicePath: card.disk}); err != nil {
				t.Fatalf("markFATVolumesUnindexed: %v", err)
			}
		})
		assertMarked(t, waitFATMounts(t, card.disk))

		if err := card.detach(); err != nil {
			t.Fatal(err)
		}
		card.attach(t, false)
		for _, mp := range waitFATMounts(t, card.disk) {
			out, _ := exec.Command("/usr/bin/mdutil", "-s", mp).CombinedOutput()
			if !strings.Contains(string(out), "Indexing and searching disabled") {
				t.Errorf("%s after re-attach: %s", mp, strings.TrimSpace(string(out)))
			}
		}
	})
}

// A fixture tracks its current attachment, rather than registering a stale disk
// number for each attach. macOS can reuse that number as soon as detach returns.
type cardImageFixture struct {
	image string
	disk  string
}

// The raw-device fixture also uses the verified, normally detached attachment.
func attachImage(t *testing.T, image string, nomount bool) string {
	t.Helper()
	card := &cardImageFixture{image: image}
	t.Cleanup(func() {
		if err := card.detach(); err != nil {
			t.Error(err)
		}
	})
	card.attach(t, nomount)
	return card.disk
}

// newCardImage attaches a fresh 256 MiB raw image partitioned like a card: two
// FAT32 volumes, both left mounted. Failed detach retains only this fixture's
// backing file for diagnosis; it never force-detaches another mounted volume.
func newCardImage(t *testing.T) *cardImageFixture {
	t.Helper()
	directory, err := os.MkdirTemp("", "wendy-index-marker-")
	if err != nil {
		t.Fatal(err)
	}
	card := &cardImageFixture{image: filepath.Join(directory, "card.img")}
	t.Cleanup(func() {
		if err := card.detach(); err != nil {
			t.Errorf("fixture cleanup: %v; retained %s", err, card.image)
			return
		}
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	f, err := os.Create(card.image)
	if err != nil {
		t.Fatal(err)
	}
	truncateErr := f.Truncate(256 << 20)
	closeErr := f.Close()
	if err := errors.Join(truncateErr, closeErr); err != nil {
		t.Fatal(err)
	}
	card.attach(t, true)
	// Avoid collisions with an existing user's BOOT or CONFIG volume.
	suffix := directory[len(directory)-8:]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "/usr/sbin/diskutil", "partitionDisk", card.disk, "GPT", "FAT32", "WB"+suffix, "128M", "FAT32", "WC"+suffix, "R").CombinedOutput(); err != nil {
		t.Fatalf("partitionDisk: %v\n%s", err, out)
	}
	return card
}

func (card *cardImageFixture) attach(t *testing.T, nomount bool) {
	t.Helper()
	if card.disk != "" {
		t.Fatal("fixture still has an attached disk")
	}
	args := []string{"attach", "-nobrowse", "-imagekey", "diskimage-class=CRawDiskImage", card.image}
	if nomount {
		args = append(args, "-nomount")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/hdiutil", args...).Output()
	if err != nil {
		t.Fatalf("hdiutil attach: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/dev/disk") {
		t.Fatalf("hdiutil attach returned no disk: %s", out)
	}
	card.disk = fields[0]
	if err := card.verifyAttachment(ctx); err != nil {
		t.Fatal(err)
	}
}

func (card *cardImageFixture) verifyAttachment(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "/usr/bin/hdiutil", "info", "-plist").Output()
	if err != nil {
		return fmt.Errorf("checking fixture ownership: %w", err)
	}
	convert := exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "json", "-o", "-", "-")
	convert.Stdin = bytes.NewReader(out)
	encoded, err := convert.Output()
	if err != nil {
		return fmt.Errorf("decoding fixture ownership: %w", err)
	}
	var info struct {
		Images []struct {
			Path     string `json:"image-path"`
			Entities []struct {
				Device string `json:"dev-entry"`
			} `json:"system-entities"`
		} `json:"images"`
	}
	if err := json.Unmarshal(encoded, &info); err != nil {
		return err
	}
	expected, err := filepath.EvalSymlinks(card.image)
	if err != nil {
		return err
	}
	for _, image := range info.Images {
		actual, err := filepath.EvalSymlinks(image.Path)
		if err != nil || actual != expected {
			continue
		}
		for _, entity := range image.Entities {
			if entity.Device == card.disk {
				return nil
			}
		}
	}
	return fmt.Errorf("refusing to detach or partition %s: it is not backed by fixture %s", card.disk, card.image)
}

func (card *cardImageFixture) detach() error {
	if card.disk == "" {
		return nil
	}
	// Marker creation does not synchronously stop Spotlight/Finder activity.
	// Retry only EBUSY, retaining the normal detach contract and checking the
	// backing image before every attempt. Never force an unmount to pass a test.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		if err := card.verifyAttachment(ctx); err != nil {
			return err
		}
		out, err := exec.CommandContext(ctx, "/usr/bin/hdiutil", "detach", card.disk).CombinedOutput()
		if err == nil {
			card.disk = ""
			return nil
		}
		var status *exec.ExitError
		if !errors.As(err, &status) || status.ExitCode() != 16 {
			return fmt.Errorf("hdiutil detach owned fixture: %w\n%s", err, out)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("hdiutil detach owned fixture remained busy: %w\n%s", ctx.Err(), out)
		case <-time.After(250 * time.Millisecond):
		}
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
