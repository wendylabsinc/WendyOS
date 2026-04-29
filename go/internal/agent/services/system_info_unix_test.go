//go:build !windows

package services

import "testing"

func TestParseLinuxMemInfo(t *testing.T) {
	values := parseLinuxMemInfo(`MemTotal:        1000 kB
MemFree:          100 kB
MemAvailable:     700 kB
Buffers:           50 kB
Cached:           150 kB
`)

	if values["MemTotal"] != 1000 {
		t.Fatalf("MemTotal = %d; want 1000", values["MemTotal"])
	}
	if values["MemAvailable"] != 700 {
		t.Fatalf("MemAvailable = %d; want 700", values["MemAvailable"])
	}
}

func TestParseLinuxMountInfoFiltersPseudoFilesystems(t *testing.T) {
	mounts := parseLinuxMountInfo(`25 0 8:1 / / rw,relatime - ext4 /dev/sda1 rw
26 25 0:22 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
27 25 8:2 / /media/My\040Drive rw,relatime - ext4 /dev/sdb1 rw
`)

	if len(mounts) != 2 {
		t.Fatalf("len(mounts) = %d; want 2", len(mounts))
	}
	if mounts[1].mountPoint != "/media/My Drive" {
		t.Fatalf("mountPoint = %q; want /media/My Drive", mounts[1].mountPoint)
	}
	if mounts[1].source != "/dev/sdb1" {
		t.Fatalf("source = %q; want /dev/sdb1", mounts[1].source)
	}
}
