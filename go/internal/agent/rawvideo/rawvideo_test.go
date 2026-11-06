package rawvideo

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFrameValidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame Frame
		want  string // substring of the error; empty means valid
	}{
		{"gray8", Frame{Data: make([]byte, 4*3), Width: 4, Height: 3, Format: FormatGRAY8}, ""},
		{"yuy2", Frame{Data: make([]byte, 4*3*2), Width: 4, Height: 3, Format: FormatYUY2}, ""},
		{"gray16", Frame{Data: make([]byte, 5*3*2), Width: 5, Height: 3, Format: FormatGRAY16LE}, ""},
		{"thermal", Frame{Data: make([]byte, 256*192*2), Width: 256, Height: 192, Format: FormatYUY2}, ""},
		{"unknown format", Frame{Data: make([]byte, 12), Width: 4, Height: 3, Format: "NV12"}, "unsupported"},
		{"lowercase format", Frame{Data: make([]byte, 12), Width: 4, Height: 3, Format: "gray8"}, "unsupported"},
		{"short data", Frame{Data: make([]byte, 11), Width: 4, Height: 3, Format: FormatGRAY8}, "has 11 bytes, want 12"},
		{"padded rows", Frame{Data: make([]byte, 16*3), Width: 14, Height: 3, Format: FormatGRAY8}, "want 42"},
		{"zero size", Frame{Width: 0, Height: 3, Format: FormatGRAY8}, "out of range"},
		{"too large", Frame{Width: MaxDimension + 1, Height: 1, Format: FormatGRAY8}, "out of range"},
		{"odd yuy2", Frame{Data: make([]byte, 3*2*2), Width: 3, Height: 2, Format: FormatYUY2}, "odd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.frame.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want valid", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if err := (Frame{Format: "RGB", Width: 1, Height: 1}).Validate(); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("unknown format error %v is not ErrUnsupportedFormat", err)
	}
}

func gray16(values ...uint16) []byte {
	out := make([]byte, 2*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint16(out[2*i:], v)
	}
	return out
}

func TestStretchGray16(t *testing.T) {
	// A thermal-like frame: all the signal sits in a narrow band high in the
	// 16-bit range. Keeping the high byte would show it as one flat grey.
	got := StretchGray16(nil, gray16(18000, 18100, 18200, 18400))
	want := []byte{0, 64, 128, 255}
	if string(got) != string(want) {
		t.Errorf("stretch = %v, want %v", got, want)
	}

	// The extremes map to 0 and 255 whatever the range.
	if got := StretchGray16(nil, gray16(0, 0xffff)); got[0] != 0 || got[1] != 255 {
		t.Errorf("full range = %v", got)
	}

	// A flat frame is mid grey, not black.
	for _, v := range StretchGray16(nil, gray16(500, 500, 500)) {
		if v != 128 {
			t.Fatalf("flat frame pixel = %d, want 128", v)
		}
	}

	// dst is reused when it is large enough.
	buf := make([]byte, 0, 8)
	out := StretchGray16(buf, gray16(1, 2))
	if &out[0] != &buf[:1][0] {
		t.Error("StretchGray16 reallocated a large enough dst")
	}
	if len(StretchGray16(nil, nil)) != 0 {
		t.Error("empty frame produced pixels")
	}
}

func receive(t *testing.T, ch <-chan Frame) Frame {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("no frame reached the subscriber")
		return Frame{}
	}
}

// TestFeedNeverBlocksTheDriver pins the backpressure contract: a subscriber that
// does not read never stalls Publish, it keeps the NEWEST frames, and each frame
// it holds is a copy the driver can no longer overwrite.
func TestFeedNeverBlocksTheDriver(t *testing.T) {
	feed := NewFeed()
	frames, cancel := feed.Subscribe()
	defer cancel()

	buf := []byte("frame-0")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			buf[len(buf)-1] = byte('0' + i%10)
			feed.Publish(Frame{Data: buf})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a subscriber that is not reading")
	}
	buf[len(buf)-1] = 'X' // the driver reuses its buffer

	var got []string
	for i := 0; i < SubscriberBuffer; i++ {
		got = append(got, string(receive(t, frames).Data))
	}
	if want := []string{"frame-8", "frame-9"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("subscriber kept %v, want the newest %v", got, want)
	}
}

func TestFeedFansOutAndUnsubscribes(t *testing.T) {
	var feed Feed // the zero value is usable
	if feed.Watched() {
		t.Fatal("a new feed is watched")
	}
	a, cancelA := feed.Subscribe()
	b, cancelB := feed.Subscribe()
	if !feed.Watched() {
		t.Fatal("feed with subscribers is not watched")
	}
	feed.Publish(Frame{Data: []byte("one")})
	if string(receive(t, a).Data) != "one" || string(receive(t, b).Data) != "one" {
		t.Fatal("a subscriber missed the frame")
	}

	cancelA()
	cancelA() // idempotent
	feed.Publish(Frame{Data: []byte("two")})
	select {
	case f := <-a:
		t.Errorf("cancelled subscriber received %q", f.Data)
	default:
	}
	if string(receive(t, b).Data) != "two" {
		t.Fatal("remaining subscriber missed the frame")
	}
	cancelB()
	if feed.Watched() {
		t.Error("feed still watched after every subscriber left")
	}
}

func TestFeedMeasuresFrameInterval(t *testing.T) {
	feed := NewFeed()
	if feed.FrameInterval() != 0 {
		t.Fatal("interval known before any frame")
	}
	start := time.Unix(1_700_000_000, 0)
	feed.Publish(Frame{Timestamp: start})
	if feed.FrameInterval() != 0 {
		t.Fatal("interval known after one frame")
	}
	// 40 ms frames (25 fps), with a frame lacking a timestamp, a shutter pause
	// and the wall clock stepping backwards mixed in: none of those may move
	// the measurement.
	ts := start
	for i := 0; i < 50; i++ {
		ts = ts.Add(40 * time.Millisecond)
		switch i {
		case 10:
			feed.Publish(Frame{})
		case 20:
			ts = ts.Add(5 * time.Second)
		case 30:
			ts = ts.Add(-time.Second)
		}
		feed.Publish(Frame{Timestamp: ts})
	}
	if got := feed.FrameInterval(); got < 39*time.Millisecond || got > 41*time.Millisecond {
		t.Errorf("FrameInterval = %v, want about 40ms", got)
	}
}
