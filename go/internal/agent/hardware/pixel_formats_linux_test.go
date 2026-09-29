//go:build linux

package hardware

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestDiscoverFmtdescMatchesKernelLayout(t *testing.T) {
	// VIDIOC_ENUM_FMT encodes the struct size (0x40) in the request number.
	if got := unsafe.Sizeof(v4l2Fmtdesc{}); got != 64 {
		t.Fatalf("sizeof(v4l2Fmtdesc) = %d, want 64", got)
	}
}

func TestDiscoverPixelFormatsRejectsNonV4L2Node(t *testing.T) {
	// A regular file opens but refuses the ioctl: an error, not an empty list.
	path := filepath.Join(t.TempDir(), "video0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if formats, err := v4l2PixelFormats(path); err == nil {
		t.Fatalf("got %v, nil; want an error", formats)
	}
	if _, err := v4l2PixelFormats(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing node: want an error")
	}
}
