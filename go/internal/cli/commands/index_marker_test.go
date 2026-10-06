//go:build darwin

package commands

import (
	"reflect"
	"testing"
)

func TestParseDiskutilSlices(t *testing.T) {
	out := `/dev/disk4 (external, physical):
   #:                       TYPE NAME                    SIZE       IDENTIFIER
   0:      GUID_partition_scheme                        *31.9 GB    disk4
   1:       Microsoft Basic Data BOOT                    134.2 MB   disk4s1
   2:       Microsoft Basic Data config                  67.1 MB    disk4s2
   3:           Linux Filesystem                         2.1 GB     disk4s3
`
	got := parseDiskutilSlices(out, "/dev/disk4")
	want := []string{"/dev/disk4s1", "/dev/disk4s2", "/dev/disk4s3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("slices = %v, want %v", got, want)
	}
	// disk41's slices must not be taken for disk4's.
	if got := parseDiskutilSlices("   1:  Microsoft Basic Data BOOT  1 GB  disk41s1\n", "/dev/disk4"); len(got) != 0 {
		t.Fatalf("matched another disk's slice: %v", got)
	}
}

func TestParseBundleType(t *testing.T) {
	info := "   Device Identifier:         disk4s1\n   Type (Bundle):             msdos\n   Name (User Visible):       MS-DOS (FAT32)\n"
	if got := parseBundleType(info); got != "msdos" {
		t.Fatalf("bundle = %q, want msdos", got)
	}
	if got := parseBundleType("   Device Identifier: disk4s3\n"); got != "" {
		t.Fatalf("bundle = %q, want empty", got)
	}
}
