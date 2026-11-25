//go:build windows

package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/env"
)

func TestParseDiskNumber(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{name: "single digit", input: `\\.\PhysicalDrive1`, want: 1},
		{name: "multi digit", input: `\\.\PhysicalDrive42`, want: 42},
		{name: "zero", input: `\\.\PhysicalDrive0`, want: 0},
		{name: "trailing junk rejected", input: `\\.\PhysicalDrive1abc`, wantErr: true},
		{name: "trailing space rejected", input: `\\.\PhysicalDrive1 `, wantErr: true},
		{name: "missing prefix", input: `PhysicalDrive1`, wantErr: true},
		{name: "wrong prefix case", input: `\\.\physicaldrive1`, wantErr: true},
		{name: "empty", input: ``, wantErr: true},
		{name: "no digits", input: `\\.\PhysicalDrive`, wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseDiskNumber(c.input)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseDiskNumber(%q) = %d, want error", c.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDiskNumber(%q) error: %v", c.input, err)
			}
			if got != c.want {
				t.Fatalf("parseDiskNumber(%q) = %d, want %d", c.input, got, c.want)
			}
		})
	}
}

func TestResolvePowershellExe(t *testing.T) {
	got := env.PowershellExe()
	if got == "" {
		t.Fatal("resolvePowershellExe() returned empty string")
	}
	// The fallback "powershell" or an absolute path are both valid; we just
	// require a non-empty string so misconfigured callers can't end up
	// invoking exec.Command("") and getting a confusing error.
}

// TestHandleWriterAtConcurrentWrites runs the seekable block-map writer with
// the NVMe writer count through handleWriterAt on a synchronous handle to a
// temporary file, opened the way openLockedDisk opens a drive. When
// handleWriterAt seeked the handle's shared file pointer before each write,
// concurrent writers put chunks at each other's offsets.
func TestHandleWriterAtConcurrentWrites(t *testing.T) {
	oldChunk, oldWorkers := bmapChunkSize, bmapWriteConcurrency
	bmapChunkSize = 512 // many small writes, so writers overlap often
	bmapWriteConcurrency = writersForStorage(StorageNVMe)
	defer func() { bmapChunkSize, bmapWriteConcurrency = oldChunk, oldWorkers }()

	img, b := buildRandomBmapImage()
	si, err := openSeekableZstdFromReader(bytes.NewReader(encodeSeekable(t, img, int(b.BlockSize))))
	if err != nil {
		t.Fatalf("open seekable: %v", err)
	}
	defer si.Close()

	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, make([]byte, b.ImageSize), 0o644); err != nil {
		t.Fatal(err)
	}
	path16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(
		path16,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL|0x80000000, // FILE_FLAG_WRITE_THROUGH
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	writeErr := applyBmapSeekable(si, handleWriterAt(h), b, func(int64) {})
	if err := syscall.CloseHandle(h); err != nil {
		t.Fatalf("CloseHandle: %v", err)
	}
	if writeErr != nil {
		t.Fatalf("applyBmapSeekable: %v", writeErr)
	}

	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if wrong := wrongBlocks(disk, img, b); wrong != 0 {
		t.Fatalf("%d of %d mapped blocks written at the wrong offset", wrong, mappedBytes(b)/b.BlockSize)
	}
}
