package main

import (
	"crypto/sha256"
	"fmt"
	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreezePrivateFATBootConfiguration(t *testing.T) {
	d, err := diskfs.Create(filepath.Join(t.TempDir(), "guest.img"), 33*1024*1024, diskfs.SectorSizeDefault)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	fs, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 0, FSType: filesystem.TypeFat32, VolumeLabel: "ESP"})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	if err = fs.Mkdir("/EFI/BOOT"); err != nil {
		t.Fatal(err)
	}
	original := strings.Repeat("linux /boot/Image root=PARTLABEL=rootfsA console=ttyAMA0,115200 ; then\n", 3)
	f, err := fs.OpenFile(grubPath, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write([]byte(original)); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = freeze(fs, strings.Repeat("0", 64)); err == nil {
		t.Fatal("accepted unpinned config")
	}
	raw, _ := fs.ReadFile(grubPath)
	if string(raw) != original {
		t.Fatal("refusal modified image")
	}
	receipt, err := freeze(fs, fmt.Sprintf("%x", sha256.Sum256([]byte(original))))
	if err != nil {
		t.Fatal(err)
	}
	raw, err = fs.ReadFile(grubPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), mask) != 3 || strings.ReplaceAll(string(raw), " "+mask, "") != original {
		t.Fatal("changed more than updater mask")
	}
	if receipt["kernel_argument"] != mask {
		t.Fatal(receipt)
	}
	if _, err = freeze(fs, receipt["original_grub_sha256"]); err == nil {
		t.Fatal("accepted already modified config")
	}
}
