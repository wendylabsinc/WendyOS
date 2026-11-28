package camerashare

import (
	"reflect"
	"testing"
)

func TestSizes_AreReadBackAsWritten(t *testing.T) {
	sizes := []SizeAtRate{{640, 480, 30}, {1280, 720, 30}, {1920, 1080, 15}}
	value := FormatSizes(sizes)
	if want := "640x480@30, 1280x720@30, 1920x1080@15"; value != want {
		t.Fatalf("FormatSizes = %q, want %q", value, want)
	}
	if got := ParseSizes(value); !reflect.DeepEqual(got, sizes) {
		t.Fatalf("ParseSizes(%q) = %v, want %v", value, got, sizes)
	}
	if got := ParseSizes("640x480@30, wide, 0x480@30, 1280x720, 1920x1080@15"); !reflect.DeepEqual(got, []SizeAtRate{{640, 480, 30}, {1920, 1080, 15}}) {
		t.Fatalf("entries that cannot be read were kept: %v", got)
	}
	if got := ParseSizes(""); got != nil {
		t.Fatalf("ParseSizes of nothing = %v", got)
	}
}

func TestModeAt_KeepsTheLayoutAndTakesTheSizeAndRate(t *testing.T) {
	for mode, want := range map[string]string{
		"1280x720 YUYV, 30 fps": "1920x1080 YUYV, 15 fps",
		"1280x720 MJPG, 30 fps": "1920x1080 MJPG, 15 fps",
		"640x480 H264, 60 fps":  "1920x1080 H264, 15 fps",
		"unreadable":            "unreadable",
	} {
		if got := ModeAt(mode, SizeAtRate{1920, 1080, 15}); got != want {
			t.Errorf("ModeAt(%q) = %q, want %q", mode, got, want)
		}
	}
}

func TestSizeSentences(t *testing.T) {
	if got, want := DescribeSizeServedInstead("640x480", "1920x1080", SizeSharedWith([]string{"cv-a", "cv-b"}, "1920x1080")),
		"asked for size 640x480; serving 1920x1080 because the camera is shared with cv-a, cv-b at 1920x1080, and a camera is captured at one size"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if got, want := SizeNotAmong("640x480@30, 1280x720@30"), "a node of this camera can be made at 640x480@30, 1280x720@30"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
