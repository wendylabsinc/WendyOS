package usbthermal

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"
	"time"
)

// A thermal block that straddles two composite frames carries the next
// frame's 12-byte header inside it (see Parser). These tests use real bytes:
//
//   - tc001max-straddled-frame.bin.gz: one raw frame (196,608 bytes) exactly as
//     the agent's raw tap served it on exxon-01 at 14:53 UTC on 2026-10-30,
//     before this fix. Its temperature rows hold a composite-frame header at
//     byte 162,816 (temperature row 126, column 0), and everything after it is
//     six pixels late. Read the way an app reads the plane, its hottest pixel
//     is 682.52 degC at (3, 126): the header's source clock. The room was at
//     about 20 degC. 10 of the 150 frames captured then looked like this.
//   - straddleHeader: the header found in another of those frames, at block
//     offset 115,252 (temperature row 33, column 26).
const straddledFrameHeaderAt = 162816

var (
	straddleHeader      = []byte{0x0c, 0x8c, 0x00, 0x00, 0x00, 0x00, 0x25, 0xd8, 0xa9, 0xdf, 0x52, 0x01}
	straddleHeaderValue = Header{Info: 0x8C, SourceClock: 0xdfa9d825, FrameNumber: 0x152}
)

func straddledFrame(t *testing.T) []byte {
	t.Helper()
	f := readFixture(t, "tc001max-straddled-frame.bin.gz")
	if len(f) != tc001Max.RawBytes() {
		t.Fatalf("straddled frame is %d bytes, want %d", len(f), tc001Max.RawBytes())
	}
	return f
}

// insertHeader returns a copy of a block window with straddleHeader inserted
// at block offset off, as the camera inserts a composite-frame header.
func insertHeader(window []byte, off int) []byte {
	at := windowMargin + off
	return slices.Concat(window[:at], straddleHeader, window[at:])
}

// hottest returns what an app reading the frame's plane reports: the hottest
// pixel that has a reading, in degC.
func hottest(f RawFrame) (celsius float64, x, y int) {
	best := -1
	for yy := range f.Height / 2 {
		for xx := range f.Width {
			if v, _ := f.Temperature(xx, yy); !noReading(v) && int(v) > best {
				best, x, y = int(v), xx, yy
			}
		}
	}
	return CountsToCelsius(uint16(best)), x, y
}

func rawFrameOf(data []byte) RawFrame {
	return RawFrame{Data: data, Width: 256, Height: 384, BytesPerLine: 512}
}

// TestParserSplicesAHeaderOutOfABlock: wherever the header lands inside a
// block, the block is delivered exactly as the camera built it, without the
// header, and counted as spliced.
func TestParserSplicesAHeaderOutOfABlock(t *testing.T) {
	l := tc001Max
	window, block := blockWindow(t), realBlock(t)
	places := map[string]int{
		"temperature row 126, column 0 (on a packet boundary)": l.PictureBytes() + 126*l.RowBytes(),
		"temperature row 33, column 26 (as measured)":          115252,
		"last temperature pixel":                               l.RawBytes() - 2,
		"picture row 10, column 3":                             10*l.RowBytes() + 6,
		"second byte of the picture":                           2,
		"metadata row, before the frame counter":               l.MetadataOffset() + 20,
		"metadata row, in its trailing 0xFF 0x00 pairs":        l.MetadataOffset() + 2*190,
		"first bytes of the end row":                           l.MetadataOffset() + l.RowBytes(),
	}
	for name, off := range places {
		t.Run(name, func(t *testing.T) {
			stream := insertHeader(window, off)
			for _, n := range []int{7, 513, bulkReadBytes, len(stream)} {
				p := NewParser(l)
				got := feedInChunks(p, stream, n)
				if len(got) != 1 {
					t.Fatalf("chunk %d: got %d blocks, want 1 (stats %+v)", n, len(got), p.Stats())
				}
				if !bytes.Equal(got[0], block) {
					t.Fatalf("chunk %d: the block delivered is not the camera's block without the header", n)
				}
				st := p.Stats()
				if st.Frames != 1 || st.Spliced != 1 || st.Rejected != 0 || st.Headers != 1 || st.LastHeader != straddleHeaderValue {
					t.Fatalf("chunk %d: stats %+v", n, st)
				}
			}
		})
	}
}

// TestParserHeaderNextToABlockIsNotSpliced: a header right before or right
// after a block is between blocks, where it belongs.
func TestParserHeaderNextToABlockIsNotSpliced(t *testing.T) {
	l := tc001Max
	window, block := blockWindow(t), realBlock(t)
	for name, off := range map[string]int{"before": 0, "after": l.BlockBytes()} {
		p := NewParser(l)
		got := feedInChunks(p, insertHeader(window, off), bulkReadBytes)
		if len(got) != 1 || !bytes.Equal(got[0], block) {
			t.Fatalf("header %s the block: got %d blocks (stats %+v)", name, len(got), p.Stats())
		}
		if st := p.Stats(); st.Spliced != 0 || st.Rejected != 0 {
			t.Fatalf("header %s the block: stats %+v", name, st)
		}
	}
}

// TestParserHeaderInTheEndRowLosesTheBlock: a header late in the end row
// breaks the run that marks the block's end. The block is lost, never
// delivered wrong.
func TestParserHeaderInTheEndRowLosesTheBlock(t *testing.T) {
	l := tc001Max
	window := blockWindow(t)
	for _, into := range []int{100, 300, 500} {
		p := NewParser(l)
		for _, b := range feedInChunks(p, insertHeader(window, l.MetadataOffset()+l.RowBytes()+into), bulkReadBytes) {
			if !bytes.Equal(b, realBlock(t)) {
				t.Fatalf("header %d bytes into the end row: delivered a block that is not the camera's", into)
			}
		}
	}
}

// TestParserTwoHeadersInOneBlock cannot be a straddle (composite frames are
// nine blocks long) and is rejected.
func TestParserTwoHeadersInOneBlock(t *testing.T) {
	l := tc001Max
	stream := insertHeader(insertHeader(blockWindow(t), l.PictureBytes()+100*l.RowBytes()), l.PictureBytes()+20*l.RowBytes())
	p := NewParser(l)
	if got := feedInChunks(p, stream, bulkReadBytes); len(got) != 0 {
		t.Fatalf("delivered %d blocks with two headers inside", len(got))
	}
	if p.Stats().Rejected == 0 {
		t.Fatalf("stats %+v, want the block rejected", p.Stats())
	}
}

// TestParserRealStraddledFrame rebuilds the stream around the frame the raw
// tap served on exxon-01 with a header in its temperatures, and checks that
// the parser now delivers the room, not the header.
//
// The raw tap served the first 196,608 bytes of what the old parser took as
// the block, which are the stream's bytes verbatim, header included. What
// followed them in the stream is rebuilt: the last six temperature pixels
// (pushed past the frame by the header; taken from the frame before it in the
// same capture, a still scene), then the metadata and end rows of the fixture
// block.
func TestParserRealStraddledFrame(t *testing.T) {
	l := tc001Max
	served := straddledFrame(t)
	window := blockWindow(t)

	// As served, the frame is what made ThermalNotify raise fire alarms.
	h, ok := ParseHeader(served[straddledFrameHeaderAt:])
	if !ok || h.SourceClock != 0xe03ceeeb || h.FrameNumber != 981 {
		t.Fatalf("no composite-frame header at byte %d of the served frame", straddledFrameHeaderAt)
	}
	if c, x, y := hottest(rawFrameOf(served)); x != 3 || y != 126 || c < 682.5 || c > 682.6 {
		t.Fatalf("served frame's hottest pixel %.2f degC at (%d, %d), want 682.52 at (3, 126)", c, x, y)
	}
	if err := rawFrameOf(served).CheckTemperatures(); !errors.Is(err, ErrNotTemperatures) {
		t.Fatalf("CheckTemperatures(served frame) = %v, want ErrNotTemperatures", err)
	}

	lastSix := []byte{0xd8, 0x4b, 0xe0, 0x4b, 0xe4, 0x4b, 0xe0, 0x4b, 0xe4, 0x4b, 0xe0, 0x4b}
	stream := slices.Concat(
		window[:windowMargin], // visible bytes before
		served,
		lastSix,
		window[windowMargin+l.MetadataOffset():], // metadata row, end row, visible bytes after
	)
	p := NewParser(l)
	got := feedInChunks(p, stream, bulkReadBytes)
	if len(got) != 1 {
		t.Fatalf("got %d blocks, want 1 (stats %+v)", len(got), p.Stats())
	}
	if st := p.Stats(); st.Spliced != 1 || st.LastHeader != h {
		t.Fatalf("stats %+v, want the served frame's header spliced out", st)
	}
	want := slices.Concat(served[:straddledFrameHeaderAt], served[straddledFrameHeaderAt+headerLength:], lastSix)
	f := rawFrameOf(got[0][:l.RawBytes()])
	if !bytes.Equal(f.Data, want) {
		t.Fatal("the delivered frame is not the served frame without its header")
	}
	if err := f.CheckTemperatures(); err != nil {
		t.Fatalf("CheckTemperatures = %v", err)
	}
	c, x, y := hottest(f)
	if c > 32 || y > 10 {
		t.Fatalf("hottest pixel %.2f degC at (%d, %d), want the room's warm corner (31.2 degC, top left)", c, x, y)
	}
	if med := CountsToCelsius(f.medianCounts()); med < 19.5 || med > 20.2 {
		t.Fatalf("median %.2f degC, want the room's 19.85", med)
	}
	// Every temperature this camera measures is a multiple of 4 counts
	// (1/16 K). The header's words are not; none may be left.
	for i := l.PictureBytes(); i < l.RawBytes(); i += 2 {
		if v := binary.LittleEndian.Uint16(f.Data[i:]); v%4 != 0 {
			t.Fatalf("temperature word at byte %d is %d, not a measurement", i, v)
		}
	}
}

// TestParserPhaseFromASplicedHeader: a spliced header fixes the packet phase,
// so the next header between blocks is counted even when blocks do not start
// on a packet boundary.
func TestParserPhaseFromASplicedHeader(t *testing.T) {
	l := tc001Max
	header := readFixture(t, "tc001max-header-window.bin")[headerAt:]
	// The block starts on a packet boundary of the window, so the spliced
	// header sits 52 bytes into a packet of the block, and the block ends 40
	// bytes before a boundary of the header's phase.
	stream := insertHeader(blockWindow(t), 115252)
	hdrPos := windowMargin + 115252
	pad := ((hdrPos-len(stream))%usbPacketBytes + usbPacketBytes) % usbPacketBytes
	stream = slices.Concat(stream, bytes.Repeat([]byte{0x11}, pad), header)
	p := NewParser(l)
	feedInChunks(p, stream, 333)
	if st := p.Stats(); st.Spliced != 1 || st.Headers != 2 || st.LastHeader.FrameNumber != 100 {
		t.Fatalf("stats %+v, want the spliced header and the one after the block", st)
	}
}

// TestCameraDeliversStraddledBlocksWhole runs the driver over a stream whose
// every block has a header in its temperature rows.
func TestCameraDeliversStraddledBlocksWhole(t *testing.T) {
	l := tc001Max
	b := newFakeBus(t)
	b.plugTC001Max("1-2.3", 15)
	dev := newFakeDevice()
	window := blockWindow(t)
	dev.bulk = replayFrames(1, func(seq uint16) []byte {
		return insertHeader(withCounter(window, seq), l.PictureBytes()+126*l.RowBytes())
	})
	b.attach(b.node(1, 15), dev)
	m := newTestManager(t, b, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raw, err := m.SubscribeRaw(ctx, exxonStableID)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if f := receive(t, raw); !bytes.Equal(f.Data, realBlock(t)[:l.RawBytes()]) {
			t.Fatal("raw frame is not the camera's block without the header")
		}
	}
}

// TestCameraWithholdsFramesThatAreNotTemperatures: a block that frames
// correctly but whose temperature rows fail CheckTemperatures never reaches a
// raw subscriber; its picture still reaches the live view, and LastError says
// why.
func TestCameraWithholdsFramesThatAreNotTemperatures(t *testing.T) {
	l := tc001Max
	b := newFakeBus(t)
	b.plugTC001Max("1-2.3", 15)
	dev := newFakeDevice()
	window := blockWindow(t)
	dev.bulk = replayFrames(1, func(seq uint16) []byte {
		w := withCounter(window, seq)
		// The picture rows where the temperatures belong: 239-243 degC.
		copy(w[windowMargin+l.PictureBytes():], w[windowMargin:windowMargin+l.PictureBytes()])
		return w
	})
	b.attach(b.node(1, 15), dev)
	m := newTestManager(t, b, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raw, err := m.SubscribeRaw(ctx, exxonStableID)
	if err != nil {
		t.Fatal(err)
	}
	pics, err := m.SubscribePictures(ctx, exxonStableID)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		receive(t, pics)
	}
	select {
	case f := <-raw:
		t.Fatalf("a raw frame reached the subscriber (hottest %.1f degC)", func() float64 { c, _, _ := hottest(f); return c }())
	case <-time.After(200 * time.Millisecond):
	}
	if err := m.LastError(exxonStableID); !errors.Is(err, ErrNotTemperatures) {
		t.Fatalf("LastError = %v, want ErrNotTemperatures", err)
	}
}
