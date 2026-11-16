package services

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The synthetic stream in video_webm_init_test.go proves which bytes are sent
// and written. This proves the point of them with the real thing on both ends:
// the agent's VP8 encoder segment, and the demuxer and decoder that `camera
// view` and anyone opening an episode's segment run. Reads are cut mid-element,
// as a pipe cuts a frame larger than its buffer, and some are lost.
func TestGStreamerVP8DecodesAcrossLostReadsForViewersAndEpisodes(t *testing.T) {
	launch, err := exec.LookPath("gst-launch-1.0")
	if err != nil {
		if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
			t.Fatal(err)
		}
		t.Skip("GStreamer is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	unavailable := func(what string, err error, detail []byte) {
		t.Helper()
		if os.Getenv("WENDY_GSTREAMER_REQUIRED") == "1" {
			t.Fatalf("%s: %v: %s", what, err, detail)
		}
		t.Skipf("%s: %v: %s", what, err, detail)
	}

	const pictures = 90
	producer := fmt.Sprintf("videotestsrc num-buffers=%d ! video/x-raw,width=320,height=240,framerate=30/1 ! %s ! fdsink fd=1",
		pictures, encoderSegment("vp8enc", false, keyframeIntervalFrames(30)))
	var producerErr bytes.Buffer
	produce := exec.CommandContext(ctx, launch, append([]string{"-q"}, strings.Fields(producer)...)...)
	produce.Stderr = &producerErr
	stream, err := produce.Output()
	if err != nil {
		unavailable("VP8 producer pipeline", err, producerErr.Bytes())
	}

	// decode counts the pictures the viewer's demuxer and decoder get out of a
	// stream, each shrunk to a 16x12 grey thumbnail so they can be counted.
	decode := func(stdin []byte, source ...string) (int, error) {
		t.Helper()
		out := filepath.Join(t.TempDir(), "pictures")
		args := append([]string{"-q"}, source...)
		args = append(args, strings.Fields("! matroskademux ! vp8dec ! videoconvert ! videoscale ! video/x-raw,format=GRAY8,width=16,height=12 ! filesink")...)
		cmd := exec.CommandContext(ctx, launch, append(args, "location="+out)...)
		cmd.Stdin = bytes.NewReader(stdin)
		detail, err := cmd.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%w: %s", err, detail)
		}
		info, statErr := os.Stat(out)
		if statErr != nil {
			return 0, err
		}
		return int(info.Size() / (16 * 12)), err
	}
	if got, err := decode(stream, "fdsrc", "fd=0"); err != nil {
		unavailable("VP8 viewer pipeline", err, nil)
	} else if got != pictures {
		t.Fatalf("the untouched stream decoded to %d pictures, want %d", got, pictures)
	}

	frames := vp8TestFrames(t, stream, 4096)
	// lose drops two reads, the first of them behind a read that ends inside
	// an element: the case a demuxer does not get past.
	lose := func(from int) []*videoFrame {
		t.Helper()
		for ; from+2 < len(frames); from++ {
			if frames[from-1].webmWhole != len(frames[from-1].data) {
				return append(append([]*videoFrame(nil), frames[:from]...), frames[from+2:]...)
			}
		}
		t.Fatal("every read ends between two elements; the test proves nothing")
		return nil
	}

	// A viewer that loses reads a third of the way in keeps decoding to the end.
	viewer := bytes.Join(pumpVP8(t, lose(len(frames)/3)), nil)
	got, err := decode(viewer, "fdsrc", "fd=0")
	if err != nil || got < pictures/2 {
		t.Fatalf("viewer that lost reads decoded %d of %d pictures: %v", got, pictures, err)
	}
	t.Logf("viewer that lost reads a third of the way in decoded %d of %d pictures", got, pictures)

	// A capture that joins the running stream and then loses reads writes
	// segment files that each open and decode from their first byte.
	c := newTestCameraCapture(t, &fakeReceipt{})
	recordVP8(t, c, lose(len(frames) * 2 / 3)[len(frames)/3:])
	if c.segment != nil {
		if err := c.segment.Close(); err != nil {
			t.Fatal(err)
		}
	}
	names, err := filepath.Glob(filepath.Join(c.dir, "segment-*.webm"))
	if err != nil || len(names) != 2 {
		t.Fatalf("segments %v (%v), want one before the loss and one after", names, err)
	}
	recorded := 0
	for _, name := range names {
		got, err := decode(nil, "filesrc", "location="+name)
		if err != nil || got == 0 {
			t.Fatalf("%s decoded %d pictures: %v", filepath.Base(name), got, err)
		}
		recorded += got
		if probe, lookErr := exec.LookPath("ffprobe"); lookErr == nil {
			out, err := exec.CommandContext(ctx, probe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name", "-of", "csv=p=0", name).Output()
			if err != nil || strings.TrimSpace(string(out)) != "vp8" {
				t.Fatalf("ffprobe %s: %q, %v", filepath.Base(name), out, err)
			}
		}
	}
	if recorded < pictures/3 {
		t.Fatalf("segments decode to %d pictures of the %d recorded stretch", recorded, pictures*2/3)
	}
	t.Logf("capture that joined a third of the way in and lost reads wrote %d segments decoding to %d pictures", len(names), recorded)
}
