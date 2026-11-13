//go:build linux

package t234

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Explicit opt-in is required: this test mounts a partition on a disposable
// loop disk. It never selects an existing physical drive.
func TestRawWriterExclusiveMountedPartition(t *testing.T) {
	if os.Getenv("WENDY_TEST_LOOP_DEVICES") != "1" || os.Geteuid() != 0 {
		t.Skip("requires root and WENDY_TEST_LOOP_DEVICES=1")
	}
	for _, tool := range []string{"losetup", "mkfs.ext4", "mount", "umount"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatal(err)
		}
	}
	run := func(tool string, args ...string) string {
		t.Helper()
		out, err := exec.Command(tool, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", tool, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	dir := t.TempDir()
	image := filepath.Join(dir, "disk.img")
	f, err := os.Create(image)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(32 << 20); err != nil {
		f.Close()
		t.Fatal(err)
	}
	mbr := make([]byte, 512)
	mbr[446+4] = 0x83
	binary.LittleEndian.PutUint32(mbr[446+8:], 2048)
	binary.LittleEndian.PutUint32(mbr[446+12:], (32<<20)/512-2048)
	mbr[510], mbr[511] = 0x55, 0xaa
	if _, err := f.WriteAt(mbr, 0); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	loop := run("losetup", "--find", "--show", "--partscan", image)
	t.Cleanup(func() {
		out, err := exec.Command("losetup", "--detach", loop).CombinedOutput()
		if err != nil {
			t.Errorf("detaching %s: %v: %s", loop, err, out)
		}
	})
	partition := loop + "p1"
	run("mkfs.ext4", "-q", "-F", partition)
	target := filepath.Join(dir, "mounted")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	mounted := false
	covered := false
	t.Cleanup(func() {
		if covered {
			out, err := exec.Command("umount", "--", target).CombinedOutput()
			if err != nil {
				t.Errorf("unmounting test cover: %v: %s", err, out)
			}
		}
		if mounted {
			out, err := exec.Command("umount", "--", target).CombinedOutput()
			if err != nil {
				t.Errorf("unmounting test partition: %v: %s", err, out)
			}
		}
	})
	run("mount", "--", partition, target)
	mounted = true
	// A covering mount belongs to a different device and must be retained.
	run("mount", "-t", "tmpfs", "tmpfs", target)
	covered = true
	if err := unmountUMSDisk(UMSDisk{DevPath: loop}); err == nil || !strings.Contains(err.Error(), "unrelated filesystem") {
		t.Fatalf("unmounted a filesystem covering the target: %v", err)
	}
	var stat syscall.Stat_t
	if err := syscall.Stat(target, &stat); err != nil {
		t.Fatal(err)
	}
	// If the guard left the cover alone, its device is still different from
	// the underlying loop partition after removing just the cover ourselves.
	coverDevice := stat.Dev
	run("umount", "--", target)
	covered = false
	if err := syscall.Stat(target, &stat); err != nil || stat.Dev == coverDevice {
		t.Fatalf("covering mount was not retained: %v", err)
	}
	// Model a desktop mounting after the parent's successful unmount pass.
	// Both actual writer paths must refuse the mounted disk before any write.
	blob := filepath.Join(dir, "rootfs.img")
	if err := os.WriteFile(blob, []byte("must not be written"), 0600); err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(dir, "layout.xml")
	xml := `<?xml version="1.0"?><partition_layout version="01.00.0000"><device type="external" instance="0" sector_size="512"><partition name="APP" id="1" type="data"><allocation_policy>sequential</allocation_policy><filesystem_type>basic</filesystem_type><size>8388608</size><allocation_attribute>8</allocation_attribute><filename>rootfs.img</filename></partition></device></partition_layout>`
	if err := os.WriteFile(layout, []byte(xml), 0600); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []WriterOptions{
		{Device: loop, Blob: blob},
		{Device: loop, WritePlan: true, LayoutPath: layout, ImagesDir: dir, RootfsDevice: "nvme0n1"},
	} {
		if err := RunWriter(opts); !errors.Is(err, syscall.EBUSY) {
			t.Fatalf("writer allowed a mounted partition: %v", err)
		}
	}
	run("umount", "--", target)
	mounted = false
	// Once opened exclusively, the disk must also reject a later mount.
	dev, err := os.OpenFile(loop, rawWriteFlags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	out, err := exec.Command("mount", "--", partition, target).CombinedOutput()
	if err == nil {
		mounted = true
		t.Fatal("partition mounted during exclusive raw-device access")
	}
	t.Logf("mount refused while exclusively held: %s", strings.TrimSpace(string(out)))
	if err := dev.Close(); err != nil {
		t.Fatal(err)
	}
	run("mount", "--", partition, target)
	mounted = true
	if err := unmountUMSDisk(UMSDisk{DevPath: loop}); err != nil {
		t.Fatal(err)
	}
	mounted = false
}
