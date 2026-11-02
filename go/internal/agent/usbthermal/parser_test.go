package usbthermal

import (
	"bytes"
	"math/rand/v2"
	"slices"
	"testing"
)

// TestParserFindsTheBlockInTheRealStream feeds the real stream excerpt in
// pieces of every awkward size, including single bytes and pieces that split
// the end row, and expects exactly the block, byte for byte.
func TestParserFindsTheBlockInTheRealStream(t *testing.T) {
	window, block := blockWindow(t), realBlock(t)
	for _, n := range []int{1, 7, 511, 512, 513, 4096, bulkReadBytes, len(window)} {
		p := NewParser(tc001Max)
		got := feedInChunks(p, window, n)
		if len(got) != 1 {
			t.Fatalf("chunk %d: got %d blocks, want 1 (stats %+v)", n, len(got), p.Stats())
		}
		if !bytes.Equal(got[0], block) {
			t.Fatalf("chunk %d: the block delivered is not the block in the stream", n)
		}
		if st := p.Stats(); st.Frames != 1 || st.Rejected != 0 || st.Partial != 0 || st.Bytes != uint64(len(window)) {
			t.Fatalf("chunk %d: stats %+v", n, st)
		}
	}
}

// TestParserDeliversACopy: the block handed out must not change when the
// parser reuses its buffer for the next frame.
func TestParserDeliversACopy(t *testing.T) {
	window := blockWindow(t)
	p := NewParser(tc001Max)
	var first []byte
	p.Feed(window, func(b []byte) { first = b })
	keep := bytes.Clone(first)
	for range 3 {
		p.Feed(withCounter(window, 1), func([]byte) {})
	}
	if !bytes.Equal(first, keep) {
		t.Fatal("a delivered block changed after later frames were parsed")
	}
}

// TestParserJoinedMidBlock: a stream joined partway through a block (the
// normal case at start-up) skips that block and finds the next one.
func TestParserJoinedMidBlock(t *testing.T) {
	window := blockWindow(t)
	p := NewParser(tc001Max)
	got := feedInChunks(p, window[windowMargin+1000:], bulkReadBytes)
	if len(got) != 0 {
		t.Fatalf("delivered %d blocks from a stream that began inside the only block", len(got))
	}
	if st := p.Stats(); st.Partial != 1 || st.Rejected != 0 {
		t.Fatalf("stats %+v, want one partial block and no rejections", st)
	}
	got = feedInChunks(p, withCounter(window, fixtureCounter+1), bulkReadBytes)
	if len(got) != 1 || blockSequence(tc001Max, got[0]) != fixtureCounter+1 {
		t.Fatalf("after joining mid-block, got %d blocks; want the next one", len(got))
	}
}

// TestParserRejectsDamagedBlocks: a block that is not exactly what the camera
// sends is never delivered, and is counted.
func TestParserRejectsDamagedBlocks(t *testing.T) {
	window := blockWindow(t)
	l := tc001Max
	damage := map[string]func([]byte) []byte{
		"one chroma byte changed": func(w []byte) []byte {
			w = bytes.Clone(w)
			w[windowMargin+2*1000+1] = 0x81
			return w
		},
		"one packet of temperatures lost": func(w []byte) []byte {
			cut := windowMargin + l.PictureBytes() + 50*l.RowBytes()
			return slices.Concat(w[:cut], w[cut+usbPacketBytes:])
		},
		"one packet of picture lost": func(w []byte) []byte {
			cut := windowMargin + 10*l.RowBytes()
			return slices.Concat(w[:cut], w[cut+usbPacketBytes:])
		},
		"metadata row lost": func(w []byte) []byte {
			cut := windowMargin + l.MetadataOffset()
			return slices.Concat(w[:cut], w[cut+l.RowBytes():])
		},
	}
	for name, f := range damage {
		t.Run(name, func(t *testing.T) {
			p := NewParser(l)
			if got := feedInChunks(p, f(window), bulkReadBytes); len(got) != 0 {
				t.Fatalf("delivered %d damaged blocks", len(got))
			}
			if st := p.Stats(); st.Rejected == 0 {
				t.Fatalf("stats %+v, want the damaged block counted as rejected", st)
			}
		})
	}
}

// TestParserResyncsAfterGarbage: noise, a damaged block and a half block
// between good ones cost exactly the frames they hit.
func TestParserResyncsAfterGarbage(t *testing.T) {
	window := blockWindow(t)
	rng := rand.New(rand.NewPCG(1, 2))
	noise := make([]byte, 300_000)
	for i := range noise {
		noise[i] = byte(rng.UintN(256))
	}
	broken := bytes.Clone(withCounter(window, fixtureCounter+1))
	broken[windowMargin+1] = 0x00 // first chroma byte
	stream := slices.Concat(
		window,
		noise,
		broken,
		window[windowMargin+tc001Max.BlockBytes()/2:], // the second half of a block
		withCounter(window, fixtureCounter+2),
	)
	p := NewParser(tc001Max)
	got := feedInChunks(p, stream, bulkReadBytes)
	var seqs []uint16
	for _, b := range got {
		seqs = append(seqs, blockSequence(tc001Max, b))
	}
	if !slices.Equal(seqs, []uint16{fixtureCounter, fixtureCounter + 2}) {
		t.Fatalf("delivered counters %v, want [%d %d]", seqs, fixtureCounter, fixtureCounter+2)
	}
	st := p.Stats()
	if st.Rejected < 2 {
		t.Fatalf("stats %+v: the damaged block and the half block should both be rejected", st)
	}
	if st.Lost != 1 {
		t.Fatalf("stats %+v: the camera's counter skipped one frame, want Lost 1", st)
	}
}

// TestParserCountsLostFrames uses the camera's own counter, including across
// its uint16 wrap; a big jump is a camera restart, not a loss.
func TestParserCountsLostFrames(t *testing.T) {
	window := blockWindow(t)
	cases := []struct {
		name string
		seqs []uint16
		lost uint64
	}{
		{"consecutive", []uint16{10, 11, 12}, 0},
		{"two missing", []uint16{10, 13}, 2},
		{"across the wrap", []uint16{65534, 65535, 0, 2}, 1},
		{"camera restarted", []uint16{21901, 5}, 0},
	}
	for _, c := range cases {
		p := NewParser(tc001Max)
		for _, s := range c.seqs {
			p.Feed(withCounter(window, s), func([]byte) {})
		}
		if st := p.Stats(); st.Lost != c.lost || st.Frames != uint64(len(c.seqs)) {
			t.Errorf("%s: frames %d lost %d, want %d and %d", c.name, st.Frames, st.Lost, len(c.seqs), c.lost)
		}
	}
}

// TestParserEndRowFollowedByMorePairs: when the visible bytes after a block
// happen to start with 0xFF 0x00 too, the run is longer than the block's own,
// and only the true end passes validation.
func TestParserEndRowFollowedByMorePairs(t *testing.T) {
	window := bytes.Clone(blockWindow(t))
	after := windowMargin + tc001Max.BlockBytes()
	for i := 0; i < 64; i += 2 {
		window[after+i], window[after+i+1] = 0xFF, 0x00
	}
	p := NewParser(tc001Max)
	got := feedInChunks(p, window, 1000)
	if len(got) != 1 || !bytes.Equal(got[0], realBlock(t)) {
		t.Fatalf("got %d blocks (stats %+v), want the real block", len(got), p.Stats())
	}
}

// TestParserRejectsEndlessFill: a stream of nothing but the end-row pattern
// (a wedged camera, say) is not a block end and does not grow the buffer.
func TestParserRejectsEndlessFill(t *testing.T) {
	p := NewParser(tc001Max)
	fill := bytes.Repeat([]byte{0xFF, 0x00}, 2_000_000)
	if got := feedInChunks(p, fill, bulkReadBytes); len(got) != 0 {
		t.Fatalf("delivered %d blocks from fill bytes", len(got))
	}
	if p.Stats().Rejected == 0 {
		t.Fatal("fill bytes were not counted as rejected")
	}
	if limit := tc001Max.BlockBytes() + compactThreshold + bulkReadBytes; len(p.buf) > limit {
		t.Fatalf("parser holds %d bytes, want at most %d", len(p.buf), limit)
	}
}

// TestParserBoundsItsMemory: a long stream with no thermal block in it (the
// visible camera's bytes) keeps the parser's history bounded.
func TestParserBoundsItsMemory(t *testing.T) {
	visible := blockWindow(t)[:windowMargin] // real visible-camera bytes
	p := NewParser(tc001Max)
	limit := tc001Max.BlockBytes() + compactThreshold + bulkReadBytes
	for range 2000 { // 8 MB
		p.Feed(visible, func([]byte) { t.Fatal("found a block in visible bytes") })
		if len(p.buf) > limit {
			t.Fatalf("parser holds %d bytes, want at most %d", len(p.buf), limit)
		}
	}
	if st := p.Stats(); st.Bytes != 2000*uint64(len(visible)) || st.Frames != 0 {
		t.Fatalf("stats %+v", st)
	}
}

// TestParseHeaderRealBytes decodes a composite-frame header the camera sent.
func TestParseHeaderRealBytes(t *testing.T) {
	window := readFixture(t, "tc001max-header-window.bin")
	h, ok := ParseHeader(window[headerAt:])
	if !ok {
		t.Fatalf("real header % x not recognised", window[headerAt:headerAt+headerLength])
	}
	want := Header{Info: 0x8C, PresentationTS: 0, SourceClock: 0x01ebc3d4, FrameNumber: 100}
	if h != want {
		t.Fatalf("header = %+v, want %+v", h, want)
	}
	// Nowhere else in the window, at any offset, looks like a header.
	for i := range len(window) - headerLength {
		if _, ok := ParseHeader(window[i:]); ok && i != headerAt {
			t.Fatalf("visible bytes at offset %d parse as a header", i)
		}
	}
	bad := map[string][]byte{
		"too short":            window[headerAt : headerAt+11],
		"wrong length byte":    slices.Concat([]byte{0x0D}, window[headerAt+1:headerAt+12]),
		"wrong info byte":      slices.Concat([]byte{0x0C, 0x80}, window[headerAt+2:headerAt+12]),
		"frame number >= 2048": slices.Concat(window[headerAt:headerAt+10], []byte{0x00, 0x08}),
	}
	for name, b := range bad {
		if _, ok := ParseHeader(b); ok {
			t.Errorf("%s: parsed as a header", name)
		}
	}
}

// TestParserCountsHeadersAtPacketBoundaries: once a block has fixed the
// packet phase, headers are counted where they can occur and nowhere else.
func TestParserCountsHeadersAtPacketBoundaries(t *testing.T) {
	window := blockWindow(t)
	header := readFixture(t, "tc001max-header-window.bin")[headerAt:] // the header and 1012 bytes after it
	// The window ends 4096 bytes (8 packets) after the block, so the header
	// that follows it starts on a packet boundary. The second copy is one byte
	// off the packet phase, where a header cannot be, and must not count.
	stream := slices.Concat(window, header,
		[]byte{0x11}, header[:usbPacketBytes+1],
		bytes.Repeat([]byte{0x11}, usbPacketBytes))
	p := NewParser(tc001Max)
	feedInChunks(p, stream, 333)
	st := p.Stats()
	if st.Headers != 1 {
		t.Fatalf("counted %d headers, want 1", st.Headers)
	}
	if st.LastHeader.FrameNumber != 100 {
		t.Fatalf("last header %+v", st.LastHeader)
	}
}
