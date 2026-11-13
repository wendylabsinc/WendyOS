//go:build linux

package t234

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func mountDiskFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, dev := range map[string]string{"sdb/dev": "8:16", "sdb/sdb1/dev": "8:17", "sdb/sdb16/dev": "8:32"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(dev+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestUnmountLinuxDiskIncludesPartitionsAndAliases(t *testing.T) {
	// Device numbers identify the filesystem even through /dev/disk aliases.
	// Include a bind mount, escaped path characters, and unrelated filesystems.
	mounts := `10 1 8:32 / /run/media/tk/config\040disk rw - vfat /dev/disk/by-label/config rw
11 10 8:32 / /run/media/tk/config\040disk/nested rw - vfat /dev/sdb16 rw
12 1 8:17 / /rootfs rw - ext4 /dev/sdb1 rw
13 1 8:16 / /commands\134image rw - ext4 /dev/sdb rw
14 1 8:48 / /unrelated rw - ext4 /dev/sdc rw
15 1 0:25 / /tmp rw - tmpfs tmpfs rw
`
	var got []string
	err := unmountLinuxDisk("/dev/sdb", mountDiskFixture(t), strings.NewReader(mounts), func(target, device string) error {
		got = append(got, target)
		return nil
	})
	want := []string{"/run/media/tk/config disk/nested", "/run/media/tk/config disk", "/commands\\image", "/rootfs"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("unmounts = %q, %v; want %q", got, err, want)
	}
}

func TestUnmountLinuxDiskNoMountIsSuccess(t *testing.T) {
	err := unmountLinuxDisk("/dev/sdb", mountDiskFixture(t), strings.NewReader("10 1 8:48 / /other rw - ext4 /dev/sdc rw\n"), func(string, string) error {
		t.Fatal("unmounted an unrelated filesystem")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnmountLinuxDiskStopsOnBusyMount(t *testing.T) {
	busy := errors.New("target is busy")
	mounts := "10 1 8:32 / /config rw - vfat /dev/sdb16 rw\n11 1 8:17 / /app rw - ext4 /dev/sdb1 rw\n"
	calls := 0
	err := unmountLinuxDisk("/dev/sdb", mountDiskFixture(t), strings.NewReader(mounts), func(string, string) error {
		calls++
		return busy
	})
	if !errors.Is(err, busy) || calls != 1 {
		t.Fatalf("unmount = %v, calls = %d", err, calls)
	}
}

func TestUnmountLinuxDiskRequiresCompleteMountInformation(t *testing.T) {
	for _, mounts := range []string{"invalid entry", "10 1 8:32 / /config rw - vfat /dev/sdb16 rw\ninvalid entry"} {
		err := unmountLinuxDisk("/dev/sdb", mountDiskFixture(t), strings.NewReader(mounts), func(string, string) error {
			t.Fatal("unmounted with incomplete mount information")
			return nil
		})
		if err == nil {
			t.Fatal("accepted malformed mountinfo")
		}
	}
}
