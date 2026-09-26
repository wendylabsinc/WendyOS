//go:build darwin || linux || windows

package t234

import (
	"reflect"
	"testing"
)

// TestHelperArgsRoundTrip pins the helper argv protocol mechanically: every
// request shape Stage2 issues must serialize (Args) and parse (ParseWriterArgs)
// back to itself — the sudo re-exec boundary on macOS/Linux speaks exactly
// these argument lists.
func TestHelperArgsRoundTrip(t *testing.T) {
	requests := []HelperRequest{
		{Writer: WriterOptions{Device: `\\.\PhysicalDrive3`, Blob: "/tmp/flashpkg.ext4"}},
		{Writer: WriterOptions{Device: "/dev/rdisk4", DumpTo: "/tmp/out", DumpBytes: flashpkgSize}},
		{Writer: WriterOptions{Device: "/dev/sdb", WritePlan: true, LayoutPath: "l.xml", ImagesDir: "/imgs", RootfsDevice: "mmcblk0"}},
		{Unmount: true, Writer: WriterOptions{Device: "/dev/sda"}},
		{Eject: true, Writer: WriterOptions{Device: "/dev/sda"}},
		{PollMedia: true, Session: "12345678"},
	}
	for _, want := range requests {
		got, err := ParseWriterArgs(want.Args())
		if err != nil {
			t.Fatalf("ParseWriterArgs(%v): %v", want.Args(), err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip of %v:\n got %+v\nwant %+v", want.Args(), got, want)
		}
	}
}

func TestParseWriterArgsErrors(t *testing.T) {
	if _, err := ParseWriterArgs([]string{"--bogus"}); err == nil {
		t.Fatal("unknown flag must error")
	}
	if _, err := ParseWriterArgs([]string{"--device"}); err == nil {
		t.Fatal("missing value must error")
	}
}
